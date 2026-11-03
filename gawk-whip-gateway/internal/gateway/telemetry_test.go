package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/wire"

	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/telemetry"
)

// ingestCapture is a stand-in gawk-telemetry ingest: it keeps every batch.
type ingestCapture struct {
	srv *httptest.Server

	mu      sync.Mutex
	batches []telemetry.Batch
	bodies  []string
}

func newIngestCapture(t *testing.T) *ingestCapture {
	t.Helper()
	c := &ingestCapture{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			// Not the reporter: something else on the host probing a
			// loopback port.
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var batch telemetry.Batch
		if err := json.Unmarshal(b, &batch); err != nil {
			t.Errorf("unparsable batch (%d bytes): %v", len(b), err)
		}
		c.mu.Lock()
		c.batches = append(c.batches, batch)
		c.bodies = append(c.bodies, string(b))
		c.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *ingestCapture) snapshot() ([]telemetry.Batch, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]telemetry.Batch(nil), c.batches...), append([]string(nil), c.bodies...)
}

func telemetryHello(t *testing.T) []byte {
	t.Helper()
	b, err := wire.AppendTelemetryHello(nil, wire.TelemetryHello{
		Enabled:          true,
		ReportIntervalMs: 250,
		Token:            make([]byte, wire.TelemetrySessionTokenSize),
		BroadcastKey:     []byte{0xd7, 0xea, 0x26, 0x80, 0x58, 0x24},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// WG11: an OBS session reports a broadcaster lane to gawk-telemetry — the
// engine's send-side stats and the gateway's own view of the OBS → gateway
// leg in one sample, under the relay's telemetry identity, with the sender
// reduced to a class — and its last batch is final when the session ends.
func TestWG11SessionReportsBroadcasterTelemetry(t *testing.T) {
	ingest := newIngestCapture(t)
	relay := newFakeRelay()
	relay.hello = telemetryHello(t)
	g := startGW(t, relay, func(o *Options) { o.TelemetryURL = ingest.srv.URL })
	key := newKey(t)

	c, offer := newClientPC(t, clientOpts{})
	ua := http.Header{"User-Agent": {"Mozilla/5.0 (OBS-Studio/32.0.1; Windows x86_64; en-US)"}}
	r := do(t, http.MethodPost, g.public.URL+"/whip", key.String(), "application/sdp", offer, ua)
	if r.code != http.StatusCreated {
		t.Fatalf("POST: %d %s", r.code, r.body)
	}
	c.base, c.key, c.location = g.public.URL, key.String(), r.hdr.Get("Location")
	if err := c.pc.SetRemoteDescription(sdpAnswer(r.body)); err != nil {
		t.Fatal(err)
	}
	c.waitConnected()

	deadline := time.Now().Add(2500 * time.Millisecond)
	for time.Now().Before(deadline) {
		c.sendGOP(10)
		time.Sleep(50 * time.Millisecond)
	}
	if r := c.delete(); r.code != http.StatusOK && r.code != http.StatusNoContent {
		t.Fatalf("DELETE: %d %s", r.code, r.body)
	}

	var batches []telemetry.Batch
	var bodies []string
	eventually(t, "a final telemetry batch", 10*time.Second, func() bool {
		batches, bodies = ingest.snapshot()
		return len(batches) > 0 && batches[len(batches)-1].Final
	})

	var samples []map[string]any
	for _, b := range batches {
		if b.Role != "broadcaster" || b.BroadcastKey != "d7ea26805824" {
			t.Fatalf("batch identity = %q/%q, want broadcaster/d7ea26805824", b.Role, b.BroadcastKey)
		}
		if b.App.Browser != "gawk-whip-gateway" || b.App.OS != "Windows" || b.App.Surface != "broadcaster" {
			t.Fatalf("app = %+v, want gawk-whip-gateway / Windows / broadcaster", b.App)
		}
		for _, s := range b.Samples {
			m, ok := s.Stats.(map[string]any)
			if !ok {
				t.Fatalf("sample stats are %T, want an object", s.Stats)
			}
			samples = append(samples, m)
		}
	}
	if len(samples) == 0 {
		t.Fatal("no samples reported")
	}
	last := samples[len(samples)-1]
	if last["whipSender"] != "OBS 32.0" {
		t.Errorf("whipSender = %v, want the class \"OBS 32.0\"", last["whipSender"])
	}
	// The engine's side (the funnel the dashboard's lane plots) and the
	// gateway's side, flat in one object.
	for _, f := range []string{
		"sentFps", "encodedFrames", "sentFrames", "codec",
		"whipPacketsExpected", "whipPacketsLost", "whipBytesReceived", "whipNacksSent",
		"whipRetransmitsRecovered", "whipRetransmitsLate", "whipRetransmitMaxMs",
		"whipRttAvailable", "whipWindowMs",
		"whipFramesWindowDropped", "whipFramesIdrWaitDropped", "whipFramesBackpressureDropped",
		"whipGopMs",
	} {
		if _, ok := last[f]; !ok {
			t.Errorf("sample lacks %q: %v", f, last)
		}
	}
	if n, _ := last["whipPacketsExpected"].(float64); n == 0 {
		t.Errorf("whipPacketsExpected = %v after a second of media", last["whipPacketsExpected"])
	}
	// The engine's stats arrive on its own 1 s cadence, so look across the
	// session rather than at one sample.
	var encoded float64
	for _, s := range samples {
		if n, _ := s["encodedFrames"].(float64); n > encoded {
			encoded = n
		}
	}
	if encoded == 0 {
		t.Error("encodedFrames is 0 in every sample: the engine's stats never reached one")
	}
	// Zero PII: neither the stream key nor the raw broadcast ID nor the
	// sender's full user-agent reaches the ingest.
	for _, body := range bodies {
		for _, secret := range []string{key.String(), key.Token, key.ID, "OBS-Studio/32.0.1", "x86_64"} {
			if strings.Contains(body, secret) {
				t.Fatalf("telemetry carried %q", secret)
			}
		}
	}
}

// WG11: the sender class is coarse — product and minor version, or a
// family — and the OS class comes from the same header.
func TestWG11SenderClass(t *testing.T) {
	for _, tc := range []struct{ ua, client, os string }{
		{"Mozilla/5.0 (OBS-Studio/32.0.1; Windows x86_64; en-US)", "OBS 32.0", "Windows"},
		{"Mozilla/5.0 (OBS-Studio/31.1.0-beta2; Macintosh; en-GB)", "OBS 31.1", "macOS"},
		{"Mozilla/5.0 (OBS-Studio/30.2.3; Linux x86_64; fi-FI)", "OBS 30.2", "Linux"},
		{"Lavf/61.7.100", "FFmpeg", ""},
		{"GStreamer/1.24.2", "GStreamer", ""},
		{"", "WHIP client", ""},
		{"curl/8.5.0", "WHIP client", ""},
	} {
		client, os := senderClass(tc.ua)
		if client != tc.client || os != tc.os {
			t.Errorf("senderClass(%q) = %q, %q; want %q, %q", tc.ua, client, os, tc.client, tc.os)
		}
	}
}
