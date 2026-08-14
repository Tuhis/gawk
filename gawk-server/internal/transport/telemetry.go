// Telemetry hello (R28 TM1, docs/33 D2 + §4.1): the relay's half of the
// correlation ID.
//
// /statusz has always named a subscriber by a random per-session key and never
// told that client its own key, so the relay's view of a viewer and the
// viewer's view of itself were two datasets that could not be joined —
// "per-viewer experience" is exactly that join, and closing it is what the
// rest of R28 is built on.
//
// The hello rides a reliable unidirectional stream, following ResumeToken
// (0x09) rather than DeliveryAck (0x0C). DeliveryAck picked a datagram and had
// to grow a re-announce loop because a single join-time datagram gets lost at
// exactly the moment a client is least likely to be draining its queue; a lost
// hello is worse than a mislabelled row — it is a session that silently never
// reports at all, which is indistinguishable from a viewer who never showed up.
//
// Sending it is best-effort in one specific sense: a failure to open or write
// the stream must never take down a working broadcast. Telemetry that can
// degrade a stream has failed on its own terms (docs/33 D9).
package transport

import (
	"encoding/hex"
	"log/slog"
	"time"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// telemetryEnabled reports whether this fleet collects telemetry. The key's
// presence IS the switch (docs/33 D12): with no key the relay cannot mint a
// token, so it sends no hello and every client collects nothing — observably
// identical to a relay predating R28, which is what makes an install with
// telemetry off byte-identical to today.
func (s *Server) telemetryEnabled() bool {
	return len(s.cfg.TelemetryKey) == wire.TelemetryKeySize
}

// telemetryReportIntervalMs is the cadence the hello asks clients to use,
// already clamped by config parsing into a range a uint16 of milliseconds can
// carry.
func (s *Server) telemetryReportIntervalMs() uint16 {
	ms := s.cfg.TelemetryReportInterval.Milliseconds()
	// A zero interval means a Server built directly from a zero Config (tests,
	// and any future caller bypassing config parsing). Fall back to the knob's
	// documented floor rather than asking clients to report every 0 ms.
	if ms <= 0 {
		ms = config.MinTelemetryReportInterval.Milliseconds()
	}
	if ms > 0xFFFF {
		return 0xFFFF
	}
	return uint16(ms)
}

// sendTelemetryHello mints this session's telemetry token, tells the client
// over a fresh uni stream, and returns the sessionId the relay should record
// on its own side so the two views join. Returns "" whenever telemetry is off
// or anything failed — the caller records nothing and the session proceeds
// exactly as it would have.
//
// broadcastID is the raw, joinable ID; it never reaches the client. What the
// hello carries is the obfuscated key, and the token's tag is computed over
// that same obfuscated key — so a client that reported the raw ID instead
// would fail verification at the ingest rather than being trusted.
func (s *Server) sendTelemetryHello(sess *webtransport.Session, broadcastID string, role wire.TelemetryRole, log *slog.Logger) string {
	if !s.telemetryEnabled() {
		return ""
	}
	broadcastKey, err := hex.DecodeString(s.registry.ObfuscateID(broadcastID))
	if err != nil || len(broadcastKey) != wire.TelemetryBroadcastKeySize {
		log.Warn("telemetry hello skipped: bad obfuscated broadcast key", "err", err)
		return ""
	}
	token, err := wire.MintTelemetrySessionToken(s.cfg.TelemetryKey, broadcastKey, role, time.Now())
	if err != nil {
		log.Warn("telemetry hello skipped: token mint failed", "err", err)
		return ""
	}
	msg, err := wire.AppendTelemetryHello(nil, wire.TelemetryHello{
		Enabled:          true,
		ReportIntervalMs: s.telemetryReportIntervalMs(),
		Token:            token,
		BroadcastKey:     broadcastKey,
	})
	if err != nil {
		log.Warn("telemetry hello skipped: encode failed", "err", err)
		return ""
	}
	if err := sendUniMessage(sess, msg); err != nil {
		// Never fatal: this session streams fine without telemetry, and the
		// missing session shows up honestly as a client that never reported
		// (docs/33 §4.8.2 — never as healthy).
		log.Warn("telemetry hello not sent; this session will not report", "err", err)
		return ""
	}
	// The token itself is never logged and never stored. Only the sessionId
	// derived from its nonce leaves this function.
	sessionID, err := wire.TelemetrySessionID(token)
	if err != nil {
		return ""
	}
	return sessionID
}

// sendTelemetryEndpoint advertises the fleet's ingest URL (R37, docs/40
// §4.10 D14) on its own uni stream, composing with the 0x0D hello — the
// hello's strict exact-length parser cannot be extended without breaking
// every existing reader. Sent only when the fleet both collects telemetry
// and has an advertised URL configured; callers on /internal/subscribe never
// call this (an edge is plumbing, not a client). Best-effort under the same
// docs/33 D9 posture as the hello: failure never degrades the session.
func (s *Server) sendTelemetryEndpoint(sess *webtransport.Session, log *slog.Logger) {
	if !s.telemetryEnabled() || s.cfg.TelemetryAdvertiseURL == "" {
		return
	}
	msg, err := wire.AppendTelemetryEndpoint(nil, s.cfg.TelemetryAdvertiseURL)
	if err != nil {
		// Config parsing validated the URL, so this is unreachable outside a
		// hand-built Config; skipping beats killing a working session.
		log.Warn("telemetry endpoint skipped: encode failed", "err", err)
		return
	}
	if err := sendUniMessage(sess, msg); err != nil {
		log.Warn("telemetry endpoint not sent; this session reports to its configured URL", "err", err)
	}
}

// sendRelayIdentity answers a probe's identity half (R37, docs/40 §4.4):
// one RelayIdentity on a server-opened uni stream at /echo session start.
// Runs in its own goroutine and is best-effort by construction — an echo
// client that grants no uni-stream credit (OpenUniStream errors) or never
// reads simply gets no identity, and the echo loop never notices (SP5's
// no-wedging criterion). Media routes never call this in R37.
func (s *Server) sendRelayIdentity(sess *webtransport.Session, log *slog.Logger) {
	version := s.cfg.ReleaseVersion
	if version == "" {
		version = "dev"
	}
	msg, err := wire.AppendRelayIdentity(nil, wire.RelayIdentity{
		ServerVersion: version,
		Name:          s.cfg.ServerName,
	})
	if err != nil {
		// Config parsing validated the name; a bad build-stamped version is
		// the only path here and is a build bug, not a session's problem.
		log.Warn("relay identity skipped: encode failed", "err", err)
		return
	}
	if err := sendUniMessage(sess, msg); err != nil {
		log.Debug("relay identity not sent", "err", err)
	}
}
