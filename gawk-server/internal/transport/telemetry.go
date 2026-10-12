// Telemetry hello (docs/33 §4.1): the relay's half of the correlation ID, so
// its view of a session joins the client's own reports. It rides a reliable
// uni stream, not a datagram: a join-time datagram is easily lost, and a lost
// hello is a session that silently never reports. Best-effort: a failure must
// never take down a working broadcast.
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
// presence is the switch: with no key the relay cannot mint a token, so it
// sends no hello and every client collects nothing.
func (s *Server) telemetryEnabled() bool {
	return len(s.cfg.TelemetryKey) == wire.TelemetryKeySize
}

// telemetryReportIntervalMs is the cadence the hello asks clients to use,
// already clamped by config parsing to fit a uint16 of milliseconds.
func (s *Server) telemetryReportIntervalMs() uint16 {
	ms := s.cfg.TelemetryReportInterval.Milliseconds()
	// Zero means a Server built from a zero Config (tests): use the knob's
	// floor rather than ask clients to report every 0 ms.
	if ms <= 0 {
		ms = config.MinTelemetryReportInterval.Milliseconds()
	}
	if ms > 0xFFFF {
		return 0xFFFF
	}
	return uint16(ms)
}

// sendTelemetryHello mints this session's telemetry token, sends it on a
// fresh uni stream, and returns the sessionId the relay records so the two
// views join; "" when telemetry is off or anything failed. broadcastID is the
// raw ID and never reaches the client: the hello carries the obfuscated key,
// and the token's tag covers that key, so a client reporting the raw ID fails
// verification at the ingest.
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
		// Never fatal: the missing session shows up as a client that never
		// reported, never as healthy.
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

// sendTelemetryEndpoint advertises the fleet's ingest URL (docs/40 §4.10) on
// its own uni stream, separate from the hello because the hello's strict
// exact-length parser cannot be extended without breaking existing readers.
// Sent only when the fleet collects telemetry and has an advertised URL;
// never on /internal/subscribe (an edge is not a client). Best-effort.
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

// sendRelayIdentity answers a probe's identity half (docs/40 §4.4): one
// RelayIdentity on a uni stream at /echo session start. Runs in its own
// goroutine so an echo client that grants no uni credit or never reads can't
// wedge the echo loop. Media routes never call this.
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
