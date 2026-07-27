package engine

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// Auto-resume: the native broadcaster's half of R17 W2 (docs/22).
//
// The browser broadcaster has reclaimed its broadcast on a transport drop
// since R17 — that is what the relay's grace window, its resume tokens and its
// "newest publisher wins" takeover exist to serve. The native engine shipped
// without it, and on 2026-07-27 that cost a live broadcast: the publisher's
// QUIC session died 78 minutes in, the relay held the ID open for its full
// grace and then garbage-collected it with three viewers still attached,
// because nothing ever came back to claim it. The broadcaster process was
// still capturing and encoding the whole time.
//
// The shape mirrors the browser's: transport-only reconnect. Capture, the
// encoder and the frameId space all survive, because the expensive and
// user-visible parts of a broadcast (the share picker, the encoder cascade,
// the GPU context) have nothing to do with which QUIC connection the bytes
// left on.
const (
	// resumeInitialDelay is the pause before the first reclaim attempt.
	// R17 puts a rollout blip at ≤1 s end to end, and the relay's drain sends
	// its 4002 while the pod is still Ready, so reconnecting almost
	// immediately is the designed behavior rather than an impatience.
	//
	// It is not zero: webtransport-go gives a client no way to read the close
	// code (its session context is cancelled with a plain context.Canceled,
	// cause discarded), so we cannot tell a planned drain from an abrupt death
	// and must not hot-loop against a relay that is genuinely gone.
	resumeInitialDelay = 250 * time.Millisecond
	// resumeMaxDelay caps the backoff. Kept well under any sane grace so the
	// window is spent trying rather than waiting.
	resumeMaxDelay = 5 * time.Second
	// resumeWindow bounds the whole effort. It matches the relay's *default*
	// -broadcast-grace rather than trying to discover the deployed value:
	// there is no way to ask, and a relay whose grace is shorter answers with
	// a 404 the moment the hub is gone, which ends this sooner and for the
	// right reason. Past the window the encoder would be burning a GPU on
	// frames with nowhere to go.
	resumeWindow = 5 * time.Minute
)

// supervise owns what a dead transport means. A recoverable loss is reclaimed;
// an unrecoverable one ends the session (and, through finish, the screen
// capture with it).
func (s *Session) supervise(ctx context.Context, relay RelaySession) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-relay.Context().Done():
		}
		s.log.Info("relay session ended", "err", context.Cause(relay.Context()))

		if code, ok := sessionCloseCode(relay); ok && terminalForPublisher(code) {
			s.log.Info("broadcast will not be resumed", "close_code", code)
			s.cb.err(closeCodeError(code))
			s.finish()
			return
		}

		next, err := s.resume(ctx)
		if err != nil {
			if ctx.Err() != nil {
				// Stop() cancelled us mid-reclaim; it owns the ending.
				return
			}
			s.log.Warn("broadcast could not be resumed", "err", err)
			s.cb.err(fmt.Errorf("lost the connection to the relay and could not resume: %w", err))
			s.finish()
			return
		}

		// The relay dropped this broadcast's cached ClockMapping when the new
		// publisher session claimed the hub, and the reclaim may well have
		// landed on a different pod — so both halves of the clock story start
		// again from nothing (see TimeSyncClient.Reset).
		s.resetClockSync()
		s.attach(ctx, next)
		s.noteResumed()
		s.log.Info("broadcast resumed", "broadcast_id", s.BroadcastID())
		s.cb.resumed()

		// Nothing forces a keyframe here: the engine has no such control over
		// the GStreamer child, and the 500 ms GOP (MediaConfig.GOPMs) means the
		// relay's invalidated keyframe cache refills within half a second. A
		// viewer sees at most one GOP of "awaiting keyframe".
		relay = next
	}
}

// resume reclaims the broadcast on a fresh session, retrying with backoff
// until it works, the relay says it never will, or the window closes.
func (s *Session) resume(ctx context.Context) (RelaySession, error) {
	s.mu.Lock()
	id, token := s.cfg.BroadcastID, s.cfg.ResumeToken
	relayURL, secret, insecure := s.cfg.RelayURL, s.cfg.PublishSecret, s.cfg.Insecure
	origin := s.cfg.Origin
	s.mu.Unlock()

	if id == "" {
		// The transport died before the announce arrived, so there is no code
		// to reclaim. Deliberately not a mint: the relay may well have minted
		// one we never heard, and starting a second broadcast would strand it
		// — R1's bug, in the one place the ID is genuinely unknown.
		return nil, errors.New("the relay session ended before the broadcast code arrived")
	}
	if origin == "" {
		origin = DefaultOrigin
	}
	url, err := PublishURL(relayURL, id, secret, token)
	if err != nil {
		return nil, fmt.Errorf("bad relay URL: %w", err)
	}

	s.setResuming(true)
	defer s.setResuming(false)

	deadline := time.Now().Add(resumeWindow)
	delay := resumeInitialDelay
	var lastErr error
	for attempt := 1; ; attempt++ {
		// Checked before the callback, not just before the dial: Stop() can
		// race the transport's death, and announcing a reclaim the shell will
		// never see finish would flash "Reconnecting…" on the way out.
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		s.cb.resuming(attempt, lastErr)
		if !sleepCtx(ctx, delay) {
			return nil, ctx.Err()
		}

		relay, status, err := s.dial(ctx, url, origin, insecure)
		if err == nil {
			return relay, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		lastErr = &StartError{Phase: PhaseConnect, Status: status, Err: err}
		s.log.Info("reclaim attempt failed", "attempt", attempt, "status", status, "err", err)

		if resumeTerminal(status) || time.Now().After(deadline) {
			return nil, lastErr
		}
		if delay *= 2; delay > resumeMaxDelay {
			delay = resumeMaxDelay
		}
	}
}

// sessionCloseCode reads the WebTransport close code the relay sent, if it
// sent one at all.
//
// Only valid once the session's context is done — and that is what makes the
// mechanism sound rather than a hack. webtransport-go discards the cause when
// it cancels a client session's context, but it keeps the close error, and a
// closed Session hands it back from OpenUniStream *before* touching the
// connection. So on a dead session this opens nothing; on a live one it would,
// which is why the only caller checks Context().Done() first.
//
// No code at all (ok=false) is the ordinary abrupt death — idle timeout,
// stateless reset, the connection simply going away — which is precisely the
// case auto-resume exists for.
func sessionCloseCode(relay RelaySession) (uint32, bool) {
	_, err := relay.OpenUniStream()
	var se *webtransport.SessionError
	if errors.As(err, &se) {
		return uint32(se.ErrorCode), true
	}
	return 0, false
}

// terminalForPublisher reports whether a close code means this broadcaster
// must stay down.
//
// CloseCodePublisherSuperseded is the load-bearing one, and it is a relay
// invariant rather than a preference: "newest publisher wins" only converges
// because the deposed client does not come back. Two engines that both
// auto-resumed would depose each other forever, each reclaim killing the other
// broadcaster's stream — wire.go says so where the code is defined, and
// TestReclaimSupersedesAgainstRealRelay is what catches a regression.
func terminalForPublisher(code uint32) bool {
	switch code {
	case wire.CloseCodeBroadcastEnded, wire.CloseCodePublisherSuperseded:
		return true
	}
	return false
}

func closeCodeError(code uint32) error {
	switch code {
	case wire.CloseCodePublisherSuperseded:
		return errors.New("another broadcaster took over this code — this session has been superseded")
	case wire.CloseCodeBroadcastEnded:
		return errors.New("the relay ended this broadcast")
	}
	return fmt.Errorf("the relay closed this session (code %d)", code)
}

// resumeTerminal reports whether a reclaim's HTTP status means retrying can
// only ever fail again. Everything else — including a bare transport failure,
// which reaches here as status 0 — is worth another attempt.
//
// These are the statuses handlePublish actually returns; keep this in step
// with gawk-server/internal/transport/server.go, exactly as StartError.Message
// already is.
func resumeTerminal(status int) bool {
	switch status {
	case http.StatusUnauthorized, // the publish secret is wrong
		http.StatusForbidden, // the resume token was refused
		http.StatusNotFound,  // the grace expired: this broadcast is gone
		http.StatusConflict:  // someone else holds the code
		return true
	}
	return false
}

// sleepCtx waits d, or returns false if ctx ended first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// resetClockSync discards the relay-clock estimate and re-arms the
// ClockMapping publication.
func (s *Session) resetClockSync() {
	if s.ts != nil {
		s.ts.Reset()
	}
	s.clockPubMu.Lock()
	defer s.clockPubMu.Unlock()
	if s.clockPub != nil {
		s.clockPub.reset()
	}
}

func (s *Session) clockMappingDue(nowUs uint64, haveSample bool) bool {
	s.clockPubMu.Lock()
	defer s.clockPubMu.Unlock()
	return s.clockPub.due(nowUs, haveSample)
}

func (s *Session) setResuming(v bool) {
	s.mu.Lock()
	s.resuming = v
	s.mu.Unlock()
}

func (s *Session) noteResumed() {
	s.mu.Lock()
	s.resumes++
	s.mu.Unlock()
}
