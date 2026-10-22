package transport

import (
	"log/slog"
	"net/http"
	"net/netip"
	"strings"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/broadcastid"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// sessionKind names the client-facing route a session arrived on, for
// sendSessionNotices.
type sessionKind uint8

const (
	kindEcho sessionKind = iota
	kindPublish
	kindSubscribe
	kindRoom
)

func (k sessionKind) String() string {
	switch k {
	case kindEcho:
		return "echo"
	case kindPublish:
		return "publish"
	case kindSubscribe:
		return "subscribe"
	case kindRoom:
		return "room"
	}
	return "unknown"
}

// LocalRelayProvider is what a PoP relay installs (R74, docs/76 item 45) so
// every client session learns it is on a local relay: its name, whether it
// is linked, the home app origin and the wall code. publishID is the
// session's broadcast ID on a publish session (so the on-wall state can be
// per broadcast) and "" on every other kind. Called on its own goroutine,
// once per session; it must not block for long.
type LocalRelayProvider interface {
	LocalRelayInfo(publishID string) wire.LocalRelayInfo
}

// Offerer is what a home fleet installs (R74, docs/76 items 41-42) to
// suggest a PoP relay to a client on that PoP's network. remote is the
// session's canonical peer address (invalid when unknown); lr reports the
// client sent lr=1, i.e. it will follow an offer. ok false means no offer.
// Called on its own goroutine, once per session.
type Offerer interface {
	Offer(remote netip.Addr, lr bool) (offer wire.LocalRelayOffer, ok bool)
}

// localRelayBox / offererBox wrap the interfaces so they fit an
// atomic.Pointer.
type localRelayBox struct{ p LocalRelayProvider }
type offererBox struct{ o Offerer }

// SetLocalRelay installs the PoP's LocalRelayInfo provider; nil removes it.
// Safe while serving.
func (s *Server) SetLocalRelay(p LocalRelayProvider) {
	if p == nil {
		s.localRelay.Store(nil)
		return
	}
	s.localRelay.Store(&localRelayBox{p: p})
}

// SetOfferer installs the home's LocalRelayOffer source; nil removes it.
// Safe while serving.
func (s *Server) SetOfferer(o Offerer) {
	if o == nil {
		s.offerer.Store(nil)
		return
	}
	s.offerer.Store(&offererBox{o: o})
}

// sendSessionNotices sends the per-session R74 notices (docs/76) at the tail
// of every client-facing session setup: /echo, /publish, a viewer's
// /subscribe (never a stripe leg) and room sessions. It must not block the
// caller: each send runs on its own goroutine.
//
// Never on /internal/* or /pop/*: an edge or PoP pump reads every uni
// stream there as media. The callers already keep to client routes; the
// path guard keeps a future caller honest. The offer also never goes to a
// session announcing popOrigin — a PoP's own dial to home is not a guest.
//
// On a publish session the broadcast ID comes from the {id} path value;
// a minted session has none there, so the publish route calls
// sendPublishSessionNotices with the minted ID instead.
func (s *Server) sendSessionNotices(sess *webtransport.Session, r *http.Request, k sessionKind) {
	var publishID string
	if k == kindPublish {
		if id, err := broadcastid.Normalize(r.PathValue("id")); err == nil {
			publishID = id
		}
	}
	s.sendNotices(sess, r, k, publishID)
}

// sendPublishSessionNotices is sendSessionNotices for a publish session
// whose broadcast ID the handler holds (minted or reclaimed).
func (s *Server) sendPublishSessionNotices(sess *webtransport.Session, r *http.Request, id string) {
	s.sendNotices(sess, r, kindPublish, id)
}

func (s *Server) sendNotices(sess *webtransport.Session, r *http.Request, k sessionKind, publishID string) {
	if p := r.URL.Path; strings.HasPrefix(p, "/internal/") || strings.HasPrefix(p, "/pop/") {
		return
	}
	if k == kindPublish && !s.cfg.ScreeningNotice && s.screeningNoticeOn() {
		// Home's screening notice mirrored by a linked PoP (docs/76 item
		// 32). With the relay's own -screening-notice the publish route
		// sends it already.
		go s.sendRelayIdentity(sess, s.log.With("remote", r.RemoteAddr, "route", k.String()))
	}
	lr := s.localRelay.Load()
	of := s.offerer.Load()
	if lr == nil && of == nil {
		return
	}
	if k != kindPublish {
		publishID = ""
	}
	log := s.log.With("remote", r.RemoteAddr, "route", k.String())
	if lr != nil {
		go s.sendLocalRelayInfo(sess, lr.p, publishID, log)
	}
	if of != nil && r.Header.Get("Origin") != popOrigin {
		remote := remoteIP(r.RemoteAddr)
		follows := r.URL.Query().Get("lr") == "1"
		go s.sendLocalRelayOffer(sess, of.o, remote, follows, log)
	}
}

// sendLocalRelayInfo encodes and sends one LocalRelayInfo. Encode failure is
// a provider bug (Warn); a send failure is the client's business (Debug).
func (s *Server) sendLocalRelayInfo(sess *webtransport.Session, p LocalRelayProvider, publishID string, log *slog.Logger) {
	msg, err := wire.AppendLocalRelayInfo(nil, p.LocalRelayInfo(publishID))
	if err != nil {
		log.Warn("local relay info skipped: encode failed", "err", err)
		return
	}
	if err := sendUniMessage(sess, msg); err != nil {
		log.Debug("local relay info not sent", "err", err)
	}
}

// sendLocalRelayOffer asks the Offerer and, on a match, sends one
// LocalRelayOffer. Same logging rule as sendLocalRelayInfo.
func (s *Server) sendLocalRelayOffer(sess *webtransport.Session, o Offerer, remote netip.Addr, lr bool, log *slog.Logger) {
	offer, ok := o.Offer(remote, lr)
	if !ok {
		return
	}
	msg, err := wire.AppendLocalRelayOffer(nil, offer)
	if err != nil {
		log.Warn("local relay offer skipped: encode failed", "err", err)
		return
	}
	if err := sendUniMessage(sess, msg); err != nil {
		log.Debug("local relay offer not sent", "err", err)
	}
}
