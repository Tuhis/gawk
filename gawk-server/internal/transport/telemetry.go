// Telemetry hello (docs/33 §4.1): the relay's half of the correlation ID, so
// its view of a session joins the client's own reports. It rides a reliable
// uni stream, not a datagram: a join-time datagram is easily lost, and a lost
// hello is a session that silently never reports. Best-effort: a failure must
// never take down a working broadcast.
package transport

import (
	"context"
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

// TelemetryReportIntervalMs is the cadence this relay's hellos ask for, which
// home tells its PoPs so theirs match (docs/78 D2).
func (s *Server) TelemetryReportIntervalMs() uint16 { return s.telemetryReportIntervalMs() }

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
// goroutine so a client that grants no uni credit or never reads can't wedge
// the session's loop. Since R40 (docs/71 D13) a publish session gets one too
// — only with -screening-notice on, so with it off publish sessions stay
// byte-identical — because the publish session is the one channel every
// broadcaster opens, at exactly the moment the notice matters. Subscribe and
// /internal/subscribe never call this.
func (s *Server) sendRelayIdentity(sess *webtransport.Session, log *slog.Logger) {
	version := s.cfg.ReleaseVersion
	if version == "" {
		version = "dev"
	}
	var notices uint8
	if s.screeningNoticeOn() {
		notices |= wire.RelayNoticeScreening
	}
	msg, err := wire.AppendRelayIdentity(nil, wire.RelayIdentity{
		ServerVersion: version,
		Name:          s.cfg.ServerName,
		Notices:       notices,
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

// SetScreeningNotice sets the dynamic half of the R40 screening notice: a
// linked PoP turns it on while home's is on (docs/76 item 32), so its party
// broadcasters see the disclosure anyone streaming to home sees. It only
// ever adds the notice: a relay started with -screening-notice keeps it
// whatever this says. Safe while serving; it applies to sessions that start
// afterwards.
func (s *Server) SetScreeningNotice(on bool) { s.popMod.screening.Store(on) }

// screeningNoticeOn is the effective notice: the relay's own flag, or the
// override home set.
func (s *Server) screeningNoticeOn() bool {
	return s.cfg.ScreeningNotice || s.popMod.screening.Load()
}

// PopTelemetry is a PoP's source of telemetry identities (R77, docs/78):
// home mints them over the PoP's link, so the fleet key never reaches the
// PoP (D1). popclient.Client implements it; the transport finds it on the
// installed LocalRelayProvider, as it finds PopDownlinkSource.
type PopTelemetry interface {
	// TelemetryHello returns a hello for one session of broadcast id in
	// role — home's broadcast key and a token home minted — and the ingest
	// URL to advertise (empty for none). It waits until ctx is done when no
	// token is at hand; ok is false when there is none to give (home
	// collects no telemetry, refused, or the wait ran out).
	TelemetryHello(ctx context.Context, id string, role wire.TelemetryRole) (hello wire.TelemetryHello, advertiseURL string, ok bool)
}

// popTelemetryWait bounds how long a session's hello may wait for home to
// mint (docs/78 D5). The session never waits: the hello is sent late, or
// not at all.
const popTelemetryWait = 5 * time.Second

func (s *Server) popTelemetry() PopTelemetry {
	if s.telemetryEnabled() {
		// A relay that holds the key mints its own (a home pod); RelayProfile
		// refuses the key, so a PoP never does (D1).
		return nil
	}
	if b := s.localRelay.Load(); b != nil {
		if pt, ok := b.p.(PopTelemetry); ok {
			return pt
		}
	}
	return nil
}

// sendSessionTelemetry gives a session its telemetry identity and the
// ingest URL. A relay holding the fleet key mints inline and returns
// at once; a PoP asks home (sendPopTelemetry) without delaying the session.
// record receives the sessionId, now or later, when there is one.
func (s *Server) sendSessionTelemetry(ctx context.Context, sess *webtransport.Session, id string, role wire.TelemetryRole, record func(string), log *slog.Logger) {
	if pt := s.popTelemetry(); pt != nil {
		go s.sendPopTelemetry(ctx, pt, sess, id, role, record, log)
		return
	}
	record(s.sendTelemetryHello(sess, id, role, log))
	s.sendTelemetryEndpoint(sess, log)
}

// sendPopTelemetry is a PoP session's hello (docs/78 D5): byte-identical to
// a home pod's, with a token home minted. Runs on its own goroutine.
func (s *Server) sendPopTelemetry(ctx context.Context, pt PopTelemetry, sess *webtransport.Session, id string, role wire.TelemetryRole, record func(string), log *slog.Logger) {
	wctx, cancel := context.WithTimeout(ctx, popTelemetryWait)
	defer cancel()
	hello, url, ok := pt.TelemetryHello(wctx, id, role)
	if !ok || ctx.Err() != nil {
		return
	}
	msg, err := wire.AppendTelemetryHello(nil, hello)
	if err != nil {
		log.Warn("pop telemetry hello skipped: encode failed", "err", err)
		return
	}
	if err := sendUniMessage(sess, msg); err != nil {
		log.Warn("pop telemetry hello not sent; this session will not report", "err", err)
		return
	}
	if sid, err := wire.TelemetrySessionID(hello.Token); err == nil {
		record(sid)
	}
	if url != "" {
		if m, err := wire.AppendTelemetryEndpoint(nil, url); err == nil {
			_ = sendUniMessage(sess, m)
		}
	}
}
