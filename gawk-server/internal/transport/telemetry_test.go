package transport

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/quic-go/webtransport-go"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/tlsutil"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// The fleet telemetry key used across this file. Shared by every relay pod
// and by gawk-telemetry, exactly like the R17 statsKey / resumeTokenKey.
var testTelemetryKey = bytes.Repeat([]byte{0x5a}, wire.TelemetryKeySize)

func telemetryCfg() config.Config {
	return config.Config{
		MaxSubscribers:          4,
		MaxIdleTimeout:          30 * time.Second,
		KeepAlivePeriod:         10 * time.Second,
		BroadcastGrace:          5 * time.Minute,
		TelemetryKey:            testTelemetryKey,
		TelemetryReportInterval: 2 * time.Second,
	}
}

// readUniMessages reads exactly n server-initiated uni streams and returns
// them keyed by wire type. Dispatching by type rather than arrival order is
// mandatory here: webtransport-go does not accept streams in open order
// (docs/22 finding 9).
func readUniMessages(t *testing.T, ctx context.Context, sess *webtransport.Session, n int) map[uint8][]byte {
	t.Helper()
	out := make(map[uint8][]byte, n)
	for range n {
		acceptCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		stream, err := sess.AcceptUniStream(acceptCtx)
		cancel()
		if err != nil {
			t.Fatalf("AcceptUniStream: %v", err)
		}
		data, err := io.ReadAll(stream)
		if err != nil {
			t.Fatalf("read uni stream: %v", err)
		}
		_, typ, err := wire.PeekType(data)
		if err != nil {
			t.Fatalf("PeekType: %v", err)
		}
		out[typ] = data
	}
	return out
}

// noMoreUniStreams asserts nothing further arrives within a short window.
// Used for the negative cases: a telemetry-disabled fleet, and edge sessions.
func noMoreUniStreams(t *testing.T, ctx context.Context, sess *webtransport.Session) {
	t.Helper()
	acceptCtx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	stream, err := sess.AcceptUniStream(acceptCtx)
	if err != nil {
		return // timed out: nothing more, which is what we want
	}
	data, _ := io.ReadAll(stream)
	_, typ, _ := wire.PeekType(data)
	t.Fatalf("unexpected extra uni stream, type 0x%02x (%d bytes)", typ, len(data))
}

// TM1's core claim: both client routes are handed a telemetry identity, the
// token verifies against the fleet key, and the SAME sessionId appears on the
// relay's own /statusz view — which is the join /statusz could not previously
// support at all.
func TestTelemetryHelloJoinsBothSidesOfASession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	port, clientTLS, r, _ := startTestServerCfg(t, ctx, telemetryCfg())

	pub := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/publish", port), clientTLS)
	defer pub.CloseWithError(0, "")
	// Announce + resume token + telemetry hello.
	msgs := readUniMessages(t, ctx, pub, 3)

	id, err := wire.ParseBroadcastAnnounce(msgs[wire.TypeBroadcastAnnounce])
	if err != nil {
		t.Fatalf("ParseBroadcastAnnounce: %v", err)
	}
	hello, err := wire.ParseTelemetryHello(msgs[wire.TypeTelemetryHello])
	if err != nil {
		t.Fatalf("ParseTelemetryHello: %v", err)
	}
	if !hello.Enabled {
		t.Error("hello.Enabled = false on a fleet with a telemetry key")
	}
	if hello.ReportIntervalMs != 2000 {
		t.Errorf("reportIntervalMs = %d, want 2000", hello.ReportIntervalMs)
	}
	// The hello carries the OBFUSCATED key — never the joinable ID (D8).
	if got, want := hex.EncodeToString(hello.BroadcastKey), r.ObfuscateID(id); got != want {
		t.Errorf("hello broadcastKey = %s, want the /statusz key %s", got, want)
	}
	if bytes.Contains(msgs[wire.TypeTelemetryHello], []byte(id)) {
		t.Error("the raw broadcast ID appears in the telemetry hello")
	}

	pubSession, err := wire.VerifyTelemetrySessionToken(
		testTelemetryKey, hello.Token, hello.BroadcastKey, wire.TelemetryRoleBroadcaster, time.Now())
	if err != nil {
		t.Fatalf("publisher token does not verify as broadcaster: %v", err)
	}
	// Role is bound into the tag: a broadcaster's token cannot submit
	// viewer-shaped records.
	if _, err := wire.VerifyTelemetrySessionToken(
		testTelemetryKey, hello.Token, hello.BroadcastKey, wire.TelemetryRoleViewer, time.Now()); err == nil {
		t.Error("publisher token verified as a viewer; role is not bound")
	}

	sub := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/subscribe/%s", port, id), clientTLS)
	defer sub.CloseWithError(0, "")
	subMsgs := readUniMessages(t, ctx, sub, 1)
	subHello, err := wire.ParseTelemetryHello(subMsgs[wire.TypeTelemetryHello])
	if err != nil {
		t.Fatalf("subscriber ParseTelemetryHello: %v", err)
	}
	viewerSession, err := wire.VerifyTelemetrySessionToken(
		testTelemetryKey, subHello.Token, subHello.BroadcastKey, wire.TelemetryRoleViewer, time.Now())
	if err != nil {
		t.Fatalf("subscriber token does not verify as viewer: %v", err)
	}
	if viewerSession == pubSession {
		t.Error("publisher and subscriber share a sessionId; they are different sessions")
	}

	// The relay's own view carries both handles — this is the join.
	var st hub.Stats
	waitFor(t, 5*time.Second, func() bool {
		st = r.Stats().Broadcasts[r.ObfuscateID(id)]
		return len(st.SubscriberDetails) == 1
	}, "subscriber visible in /statusz")

	if st.PublisherSessionID != pubSession {
		t.Errorf("statusz publisherSessionId = %q, want %q", st.PublisherSessionID, pubSession)
	}
	if got := st.SubscriberDetails[0].SessionID; got != viewerSession {
		t.Errorf("statusz subscriberDetails[0].sessionId = %q, want %q", got, viewerSession)
	}
	// sessionId is additive: the pre-existing display key is untouched and
	// still its own independent handle (D2 — deriving one from the other
	// would leak part of a bearer credential into /statusz).
	if st.SubscriberDetails[0].Key == "" || st.SubscriberDetails[0].Key == viewerSession {
		t.Errorf("subscriber key %q must stay a separate, non-empty handle", st.SubscriberDetails[0].Key)
	}
}

// The default fleet has no telemetry key, so nothing changes: no third uni
// stream on publish, no stream at all on subscribe, and /statusz renders
// exactly as before. This is the "an install that never enables it is
// byte-identical to today" claim (D12), asserted rather than asserted-about.
func TestTelemetryDisabledSendsNoHello(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cfg := telemetryCfg()
	cfg.TelemetryKey = nil
	port, clientTLS, r, _ := startTestServerCfg(t, ctx, cfg)

	pub := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/publish", port), clientTLS)
	defer pub.CloseWithError(0, "")
	msgs := readUniMessages(t, ctx, pub, 2) // announce + resume token only
	if _, ok := msgs[wire.TypeTelemetryHello]; ok {
		t.Fatal("telemetry hello sent with no fleet key")
	}
	noMoreUniStreams(t, ctx, pub)

	id, err := wire.ParseBroadcastAnnounce(msgs[wire.TypeBroadcastAnnounce])
	if err != nil {
		t.Fatalf("ParseBroadcastAnnounce: %v", err)
	}
	sub := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/subscribe/%s", port, id), clientTLS)
	defer sub.CloseWithError(0, "")
	noMoreUniStreams(t, ctx, sub)

	var st hub.Stats
	waitFor(t, 5*time.Second, func() bool {
		st = r.Stats().Broadcasts[r.ObfuscateID(id)]
		return len(st.SubscriberDetails) == 1
	}, "subscriber visible in /statusz")
	if st.PublisherSessionID != "" {
		t.Errorf("publisherSessionId = %q with telemetry off, want empty", st.PublisherSessionID)
	}
	if got := st.SubscriberDetails[0].SessionID; got != "" {
		t.Errorf("subscriberDetails[0].sessionId = %q with telemetry off, want empty", got)
	}
}

// A short telemetry key is a misconfiguration, not a weaker mode: the relay
// must not mint tokens a verifier would accept under an equally short key.
// It degrades to "telemetry off" rather than to "telemetry, badly".
func TestTelemetryShortKeyDisables(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cfg := telemetryCfg()
	cfg.TelemetryKey = bytes.Repeat([]byte{0x01}, 16)
	port, clientTLS, _, _ := startTestServerCfg(t, ctx, cfg)

	pub := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/publish", port), clientTLS)
	defer pub.CloseWithError(0, "")
	msgs := readUniMessages(t, ctx, pub, 2)
	if _, ok := msgs[wire.TypeTelemetryHello]; ok {
		t.Fatal("telemetry hello sent with an undersized fleet key")
	}
	noMoreUniStreams(t, ctx, pub)
}

// The publisher's telemetry handle follows the session, not the broadcast: a
// superseded publisher's handle must not stay attached to a broadcast served
// by a different session, and a closed publisher must not leave a stale one
// behind for the grace period.
func TestTelemetryPublisherSessionFollowsTheSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	port, clientTLS, r, _ := startTestServerCfg(t, ctx, telemetryCfg())

	pub := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/publish", port), clientTLS)
	msgs := readUniMessages(t, ctx, pub, 3)
	id, err := wire.ParseBroadcastAnnounce(msgs[wire.TypeBroadcastAnnounce])
	if err != nil {
		t.Fatalf("ParseBroadcastAnnounce: %v", err)
	}
	hello, err := wire.ParseTelemetryHello(msgs[wire.TypeTelemetryHello])
	if err != nil {
		t.Fatalf("ParseTelemetryHello: %v", err)
	}
	first, err := wire.TelemetrySessionID(hello.Token)
	if err != nil {
		t.Fatalf("TelemetrySessionID: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool {
		return r.Stats().Broadcasts[r.ObfuscateID(id)].PublisherSessionID == first
	}, "first publisher session recorded")

	// Closing the publisher leaves the hub in grace with no publisher — and
	// no telemetry handle, so relay observations during the grace window are
	// never attributed to a session that has ended.
	pub.CloseWithError(0, "")
	waitFor(t, 5*time.Second, func() bool {
		st := r.Stats().Broadcasts[r.ObfuscateID(id)]
		return !st.PublisherActive && st.PublisherSessionID == ""
	}, "publisher session handle cleared on close")

	// A reclaim gets its own handle.
	token := hex.EncodeToString(mustParseResumeToken(t, msgs[wire.TypeResumeToken]))
	pub2 := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/publish/%s?resume=%s", port, id, token), clientTLS)
	defer pub2.CloseWithError(0, "")
	msgs2 := readUniMessages(t, ctx, pub2, 3)
	hello2, err := wire.ParseTelemetryHello(msgs2[wire.TypeTelemetryHello])
	if err != nil {
		t.Fatalf("reclaim ParseTelemetryHello: %v", err)
	}
	second, err := wire.TelemetrySessionID(hello2.Token)
	if err != nil {
		t.Fatalf("TelemetrySessionID: %v", err)
	}
	if second == first {
		t.Error("reclaim reused the previous session handle; sessions must be distinct")
	}
	waitFor(t, 5*time.Second, func() bool {
		return r.Stats().Broadcasts[r.ObfuscateID(id)].PublisherSessionID == second
	}, "reclaimed publisher session recorded")
}

func mustParseResumeToken(t *testing.T, msg []byte) []byte {
	t.Helper()
	tok, err := wire.ParseResumeToken(msg)
	if err != nil {
		t.Fatalf("ParseResumeToken: %v", err)
	}
	return tok
}

// An edge is plumbing, not a client (docs/33 §4.1). Its /internal/subscribe
// session must receive no hello — otherwise every hop would mint a session
// nobody watches, and edge-shaped rows would pollute the per-viewer store the
// whole item exists to build.
func TestTelemetryHelloNotSentToInternalSubscribe(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cs := fake.NewClientset()

	cert, err := tlsutil.GenerateDevCert([]string{"localhost", "127.0.0.1"}, time.Hour)
	if err != nil {
		t.Fatalf("GenerateDevCert: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert.Leaf)
	clientTLS := &tls.Config{RootCAs: pool, ServerName: "localhost", NextProtos: []string{"h3"}}

	withTelemetry := func(c *config.Config) {
		c.TelemetryKey = testTelemetryKey
		c.TelemetryReportInterval = 2 * time.Second
	}
	pod := startClusteredPod(t, ctx, cs, "pod-a", cert, pool, 0, withTelemetry)

	pub := dial(t, ctx, fmt.Sprintf("https://%s/publish", pod.addr()), clientTLS)
	defer pub.CloseWithError(0, "")
	msgs := readUniMessages(t, ctx, pub, 3)
	id, err := wire.ParseBroadcastAnnounce(msgs[wire.TypeBroadcastAnnounce])
	if err != nil {
		t.Fatalf("ParseBroadcastAnnounce: %v", err)
	}
	if _, ok := msgs[wire.TypeTelemetryHello]; !ok {
		t.Fatal("publisher got no telemetry hello; the fixture is wrong, not the assertion")
	}

	edge := dial(t, ctx, fmt.Sprintf("https://%s/internal/subscribe/%s?psk=fleet-psk&gen=1&proto=%d",
		pod.addr(), id, wire.Version), clientTLS)
	defer edge.CloseWithError(0, "")
	noMoreUniStreams(t, ctx, edge)

	// And the edge session carries no sessionId in /statusz either.
	var st hub.Stats
	waitFor(t, 5*time.Second, func() bool {
		st = pod.registry.Stats().Broadcasts[pod.registry.ObfuscateID(id)]
		return st.EdgeSessions == 1
	}, "edge session visible in /statusz")
	for _, d := range st.SubscriberDetails {
		if d.Internal && d.SessionID != "" {
			t.Errorf("edge session carries sessionId %q, want none", d.SessionID)
		}
	}
}
