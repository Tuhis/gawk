package transport

// The mint-only publish (R33, docs/83 D3): CONNECT /publish?mint=1. The web
// app mints an OBS stream key — a broadcast ID and its resume token — without
// starting a broadcast. It passes every gate an ordinary mint passes
// (draining, the per-IP rate limit, ?secret=, the IP ban — all run by
// handlePublish before it gets here — and CheckPublishNew), announces the ID
// and the token exactly as a mint does, then closes with 4017
// PublishMinted. Nothing else happens: no hub, no broadcast.started, no
// /statusz or admin row, no max-broadcasts slot, no broadcast-started metric.
// The first CONNECT /publish/{id}?resume=<token> creates the broadcast
// through the ordinary unknown-ID reclaim.

import (
	"context"
	"errors"
	"net/http"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/cluster"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// mintOnlyCloseReason is the 4017 close's reason string.
const mintOnlyCloseReason = "minted, nothing to publish"

// errMintDrawExhausted: every draw named an ID already in use.
var errMintDrawExhausted = errors.New("every drawn broadcast ID was in use")

// handleMintOnly serves CONNECT /publish?mint=1 once handlePublish's shared
// gates have passed.
func (s *Server) handleMintOnly(w http.ResponseWriter, r *http.Request, src requestSource) {
	// A PoP relay (a MintGate installed, docs/76) clears every minted ID with
	// its home fleet; a mint-only would hand out a key for an ID the home
	// never checked, and the gateway publishes to its own home relay anyway
	// (docs/83 OD4, non-goals). So the route doesn't exist on a PoP.
	if s.mintGate.Load() != nil {
		s.metrics.Connection("publish", metrics.OutcomeNotFound)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	// At capacity a mint-only is refused like a mint (D3: it passes
	// CheckPublishNew), although it would take no slot itself: a key minted
	// on a full relay could not be used now either, and the 429 tells the
	// user so while the dial can still fail cleanly.
	if err := s.registry.CheckPublishNew(); err != nil {
		s.metrics.Connection("publish", metrics.OutcomeLimitRejected)
		s.log.Warn("publish mint-only rejected",
			"remote", src.remote, "origin", r.Header.Get("Origin"), "err", err)
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	sess, err := s.wt.Upgrade(w, r)
	if err != nil {
		s.metrics.Connection("publish", metrics.OutcomeUpgradeFailed)
		s.log.Warn("publish upgrade failed", "err", err)
		w.WriteHeader(http.StatusForbidden) // never an implicit 200
		return
	}
	defer s.trackSession(sess)()
	s.postUpgradePublishHook("")

	// The ban TOCTOU window, as on the publish path: an IP ban landing during
	// the upgrade must not hand this client a key. There is no session to
	// kill here — none is tracked as a publisher — so the re-check is the
	// whole defence.
	if rec, banned := s.banSet().BannedIP(src.addr, s.clock()); banned {
		s.metrics.Connection("publish", metrics.OutcomeBanned)
		s.log.Warn("publish mint-only closed: banned during the upgrade",
			"remote", src.remote, "target_type", string(rec.Target.Type))
		closeWithNotice(sess, wire.CloseCodeTerminatedByOperator, terminationReason)
		return
	}

	id, err := s.drawMintOnlyID(r.Context(), s.registry.MintUnusedID)
	if err != nil {
		s.metrics.Connection("publish", metrics.OutcomeError)
		s.log.Warn("publish mint-only failed to draw a broadcast ID", "remote", src.remote, "err", err)
		sess.CloseWithError(webtransport.SessionErrorCode(http.StatusServiceUnavailable), "failed to mint a broadcast ID")
		return
	}

	// The same two messages, built the same way, in the same order as the
	// publish path's announce: clients dispatch them by wire type.
	announceBytes, err := wire.AppendBroadcastAnnounce(nil, id)
	if err != nil {
		s.metrics.Connection("publish", metrics.OutcomeError)
		s.log.Warn("failed to build broadcast announce bytes", "err", err)
		sess.CloseWithError(500, "failed to build announce")
		return
	}
	if err := sendUniMessage(sess, announceBytes); err != nil {
		s.metrics.Connection("publish", metrics.OutcomeError)
		s.log.Warn("failed to send broadcast announce", "err", err)
		sess.CloseWithError(500, "failed to send announce")
		return
	}
	// Never logged, like every resume token.
	tokenBytes, err := wire.AppendResumeToken(nil, s.resume.mint(id))
	if err != nil {
		s.metrics.Connection("publish", metrics.OutcomeError)
		s.log.Warn("failed to build resume token message", "err", err)
		sess.CloseWithError(500, "failed to build resume token")
		return
	}
	if err := sendUniMessage(sess, tokenBytes); err != nil {
		s.metrics.Connection("publish", metrics.OutcomeError)
		s.log.Warn("failed to send resume token", "err", err)
		sess.CloseWithError(500, "failed to send resume token")
		return
	}

	s.metrics.Connection("publish", metrics.OutcomeMinted)
	// The HMAC'd key, never the raw ID: it is joinable once broadcasting.
	s.log.Info("publish key minted", "remote", src.remote, "broadcast_key", s.broadcastKey(id))
	// The notice is written after the announce and the token, and the close
	// waits closeNoticeSettle after it, so all three are on the wire before
	// the close cancels what the peer has not read.
	closeWithNotice(sess, wire.CloseCodePublishMinted, mintOnlyCloseReason)
}

// drawMintOnlyID draws a fresh broadcast ID that names no broadcast in use;
// draw is the registry's (Registry.MintUnusedID), injectable for tests.
//
// Nothing reserves it, and nothing needs to. The ID's only later use is a
// reclaim presenting its resume token, HMAC(key, ID), which only this mint
// handed out; a reclaim of an unknown ID creates the broadcast (any pod, any
// time), so an unused minted ID holds no state anywhere and cannot be stolen
// without the token. What must hold is that it is unused NOW: a minted ID
// equal to a live or graced broadcast's would hand this client the token
// that supersedes that broadcast's owner (4004). So the draw skips
//   - every hub this pod knows and every reserved code (rooms, PoP
//     reservations): Registry.MintUnusedID, StartPublish's own rules;
//   - in cluster mode, any ID with an origin Lease anywhere in the fleet —
//     live, graced or a PoP's — asked of the API server (Resolve, not the
//     informer cache, which says nothing before it syncs). A mint is a rare,
//     rate-limited user action, so the GET is affordable;
//   - an ID under an ID ban, whose key could never publish.
//
// The residual race — another pod minting the same ID between this check
// and the client's first reclaim — is the ordinary mint's collision odds
// (one in ~31^6 per concurrent mint). A minted key also outlives the mint:
// an OBS key is saved for weeks, and an ordinary mint that later draws the
// same ID while the key is idle would be superseded by the key's next
// reclaim. That is the same property any long-lived resume token has, and
// the odds per mint are the same one in ~31^6.
func (s *Server) drawMintOnlyID(ctx context.Context, draw func() (string, error)) (string, error) {
	coord := s.clusterCoord()
	for range mintRedraws + 1 {
		id, err := draw()
		if err != nil {
			return "", err
		}
		if _, banned := s.banSet().BannedID(id, s.clock()); banned {
			continue
		}
		if coord != nil {
			_, err := coord.Resolve(ctx, id)
			switch {
			case err == nil:
				continue // a Lease exists: in use somewhere in the fleet
			case !errors.Is(err, cluster.ErrNotFound):
				return "", err
			}
		}
		return id, nil
	}
	return "", errMintDrawExhausted
}
