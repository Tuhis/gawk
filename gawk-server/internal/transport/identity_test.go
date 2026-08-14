// R37 (docs/40 SP5 + SP11): the relay's identity on /echo and the telemetry
// endpoint advertisement on the media routes.
package transport

import (
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// collectUniMessages accepts uni streams for the given window and returns
// each stream's full payload. Order between the relay's announcement streams
// is unspecified, so callers dispatch by type byte.
func collectUniMessages(t *testing.T, sess *webtransport.Session, window time.Duration) [][]byte {
	t.Helper()
	var msgs [][]byte
	deadline := time.Now().Add(window)
	for {
		ctx, cancel := context.WithDeadline(context.Background(), deadline)
		stream, err := sess.AcceptUniStream(ctx)
		cancel()
		if err != nil {
			return msgs
		}
		data, err := io.ReadAll(stream)
		if err != nil {
			t.Fatalf("read uni stream: %v", err)
		}
		msgs = append(msgs, data)
	}
}

func identityFrom(t *testing.T, msgs [][]byte) (wire.RelayIdentity, int) {
	t.Helper()
	var id wire.RelayIdentity
	count := 0
	for _, m := range msgs {
		if len(m) >= 2 && m[0] == wire.Version && m[1] == wire.TypeRelayIdentity {
			parsed, err := wire.ParseRelayIdentity(m)
			if err != nil {
				t.Fatalf("ParseRelayIdentity: %v", err)
			}
			id = parsed
			count++
		}
	}
	return id, count
}

func endpointsFrom(t *testing.T, msgs [][]byte) []string {
	t.Helper()
	var urls []string
	for _, m := range msgs {
		if len(m) >= 2 && m[0] == wire.Version && m[1] == wire.TypeTelemetryEndpoint {
			u, err := wire.ParseTelemetryEndpoint(m)
			if err != nil {
				t.Fatalf("ParseTelemetryEndpoint: %v", err)
			}
			urls = append(urls, u)
		}
	}
	return urls
}

// The probe's identity half (docs/40 §4.4): an echo session receives exactly
// one RelayIdentity uni stream carrying the configured name and the stamped
// release version.
func TestEchoSendsRelayIdentity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, _, _ := startTestServerCfg(t, ctx, config.Config{
		MaxSubscribers:  2,
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 10 * time.Second,
		ServerName:      "Test Homelab",
		ReleaseVersion:  "9.9.9",
	})

	sess := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/echo", port), clientTLS)
	msgs := collectUniMessages(t, sess, 2*time.Second)
	id, count := identityFrom(t, msgs)
	if count != 1 {
		t.Fatalf("got %d RelayIdentity messages, want exactly 1 (all: %d msgs)", count, len(msgs))
	}
	if id.ServerVersion != "9.9.9" || id.Name != "Test Homelab" {
		t.Fatalf("identity = %+v, want 9.9.9 / Test Homelab", id)
	}
}

// An unset -server-name still identifies the release ("dev" when nothing was
// stamped), with an empty name.
func TestEchoIdentityWithoutName(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, _, _ := startTestServer(t, ctx, 2)

	sess := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/echo", port), clientTLS)
	id, count := identityFrom(t, collectUniMessages(t, sess, 2*time.Second))
	if count != 1 {
		t.Fatalf("got %d RelayIdentity messages, want 1", count)
	}
	if id.ServerVersion != "dev" || id.Name != "" {
		t.Fatalf("identity = %+v, want dev / unnamed", id)
	}
}

// SP5's no-wedging criterion, tested at the layer that CAN be tested: a
// client that never drains its incoming uni streams must still get prompt
// echoes — the identity send runs off the echo loop's critical path (a `go`
// statement in handleEcho) and its outcome is irrelevant to the service.
//
// The stronger variant — a client granting zero uni-stream credit — is not
// constructible with a conformant stack: HTTP/3 itself needs incoming uni
// credit for the peer's control/QPACK streams, so a `MaxIncomingUniStreams:
// -1` client cannot even complete the WebTransport handshake (verified: the
// Dial blocks forever in the h3 settings wait). The residual property —
// OpenUniStream failing or stalling never reaches the echo loop — is
// structural: the send is a separate goroutine and sendUniMessage's error is
// logged and dropped (telemetry.go sendRelayIdentity).
func TestEchoAnswersWhileIdentityStreamUnread(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, _, _ := startTestServerCfg(t, ctx, config.Config{
		MaxSubscribers:  2,
		MaxIdleTimeout:  30 * time.Second,
		KeepAlivePeriod: 10 * time.Second,
		ServerName:      "Never Read",
	})

	sess := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/echo", port), clientTLS)
	// Deliberately never AcceptUniStream: the identity stream sits undrained
	// for the whole session while the echo path is exercised.
	for i := 0; i < 3; i++ {
		ping := []byte{0x01, 0x05, byte(i), 0xad}
		if err := sess.SendDatagram(ping); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
		recvCtx, recvCancel := context.WithTimeout(ctx, 3*time.Second)
		got, err := sess.ReceiveDatagram(recvCtx)
		recvCancel()
		if err != nil {
			t.Fatalf("echo %d did not answer with the identity stream unread: %v", i, err)
		}
		if string(got) != string(ping) {
			t.Fatalf("echo %d: got %x, want %x", i, got, ping)
		}
	}
}

func telemetryTestKey() []byte {
	k := make([]byte, wire.TelemetryKeySize)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

// SP11: enabled telemetry + a configured advertise URL ⇒ exactly one 0x12
// per session on both media routes.
func TestMediaRoutesAdvertiseTelemetryEndpoint(t *testing.T) {
	const advertised = "https://gawk.example.com/api/telemetry/v1/ingest"
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	port, clientTLS, _, _ := startTestServerCfg(t, ctx, config.Config{
		MaxSubscribers:          2,
		MaxIdleTimeout:          30 * time.Second,
		KeepAlivePeriod:         10 * time.Second,
		TelemetryKey:            telemetryTestKey(),
		TelemetryReportInterval: 2 * time.Second,
		TelemetryAdvertiseURL:   advertised,
	})

	// The handshake helper consumes uni streams in whatever order they are
	// accepted — the 0x12 may be among its leftovers (review finding R3-B),
	// so the assertion runs over consumed + subsequently-collected messages.
	pub := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/publish", port), clientTLS)
	id, _, consumed := readPublisherHandshakeMsgs(t, ctx, pub)
	pubMsgs := append(consumed, collectUniMessages(t, pub, 2*time.Second)...)
	pubURLs := endpointsFrom(t, pubMsgs)
	if len(pubURLs) != 1 || pubURLs[0] != advertised {
		t.Fatalf("publish endpoints = %v, want exactly [%s]", pubURLs, advertised)
	}

	sub := dialSubscriber(t, ctx, port, id, clientTLS)
	subURLs := endpointsFrom(t, collectUniMessages(t, sub, 2*time.Second))
	if len(subURLs) != 1 || subURLs[0] != advertised {
		t.Fatalf("subscribe endpoints = %v, want exactly [%s]", subURLs, advertised)
	}
}

// No advertise URL, or telemetry off entirely ⇒ no 0x12 anywhere.
func TestNoTelemetryEndpointWhenUnconfigured(t *testing.T) {
	cases := []struct {
		name string
		cfg  config.Config
	}{
		{"telemetry on, no advertise URL", config.Config{
			TelemetryKey:            telemetryTestKey(),
			TelemetryReportInterval: 2 * time.Second,
		}},
		{"advertise URL set, telemetry off", config.Config{
			TelemetryAdvertiseURL: "https://gawk.example.com/api/telemetry/v1/ingest",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			cfg := tc.cfg
			cfg.MaxSubscribers = 2
			cfg.MaxIdleTimeout = 30 * time.Second
			cfg.KeepAlivePeriod = 10 * time.Second
			port, clientTLS, _, _ := startTestServerCfg(t, ctx, cfg)

			pub := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/publish", port), clientTLS)
			id, _, consumed := readPublisherHandshakeMsgs(t, ctx, pub)
			pubMsgs := append(consumed, collectUniMessages(t, pub, 1500*time.Millisecond)...)
			if urls := endpointsFrom(t, pubMsgs); len(urls) != 0 {
				t.Fatalf("publish endpoints = %v, want none", urls)
			}
			sub := dialSubscriber(t, ctx, port, id, clientTLS)
			if urls := endpointsFrom(t, collectUniMessages(t, sub, 1500*time.Millisecond)); len(urls) != 0 {
				t.Fatalf("subscribe endpoints = %v, want none", urls)
			}
		})
	}
}

// The knobs fail fast at parse time (SP11's fail-fast criterion) and reach
// the production config (the R2 lesson — walk the knob to its consumer).
func TestAdvertiseKnobsParsing(t *testing.T) {
	env := func(map[string]string) func(string) string {
		return func(string) string { return "" }
	}
	if _, err := config.ParseFlags([]string{"-telemetry-advertise-url", "http://insecure.example/x"}, env(nil)); err == nil {
		t.Fatal("http advertise URL parsed, want fail-fast error")
	}
	if _, err := config.ParseFlags([]string{"-telemetry-advertise-url", "not a url"}, env(nil)); err == nil {
		t.Fatal("junk advertise URL parsed, want fail-fast error")
	}
	if _, err := config.ParseFlags([]string{"-server-name", string(make([]byte, 100))}, env(nil)); err == nil {
		t.Fatal("oversize server name parsed, want fail-fast error")
	}
	cfg, err := config.ParseFlags([]string{
		"-telemetry-advertise-url", "https://gawk.example.com/api/telemetry/v1/ingest",
		"-server-name", "Homelab",
	}, env(nil))
	if err != nil {
		t.Fatalf("valid knobs: %v", err)
	}
	if cfg.TelemetryAdvertiseURL != "https://gawk.example.com/api/telemetry/v1/ingest" || cfg.ServerName != "Homelab" {
		t.Fatalf("knobs did not reach config: %+v", cfg)
	}

	// Env fallbacks (GAWK_*), the other half of the plumbing invariant.
	envs := map[string]string{
		"GAWK_TELEMETRY_ADVERTISE_URL": "https://env.example/ingest",
		"GAWK_SERVER_NAME":             "Env Named",
	}
	cfg, err = config.ParseFlags(nil, func(k string) string { return envs[k] })
	if err != nil {
		t.Fatalf("env knobs: %v", err)
	}
	if cfg.TelemetryAdvertiseURL != "https://env.example/ingest" || cfg.ServerName != "Env Named" {
		t.Fatalf("env knobs did not reach config: %+v", cfg)
	}
}
