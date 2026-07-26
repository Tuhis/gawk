package telemetry

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
)

type capture struct {
	mu      sync.Mutex
	bodies  []string
	status  int
	srv     *httptest.Server
	failing bool
}

func newCapture(t *testing.T) *capture {
	t.Helper()
	c := &capture{status: http.StatusNoContent}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies = append(c.bodies, string(buf))
		status, failing := c.status, c.failing
		c.mu.Unlock()
		if failing {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *capture) batches(t *testing.T) []Batch {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Batch, 0, len(c.bodies))
	for _, b := range c.bodies {
		var batch Batch
		if err := json.Unmarshal([]byte(b), &batch); err != nil {
			t.Fatalf("batch is not valid JSON: %v (%s)", err, b)
		}
		out = append(out, batch)
	}
	return out
}

func (c *capture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.bodies)
}

func hello(enabled bool) wire.TelemetryHello {
	return wire.TelemetryHello{
		Enabled:          enabled,
		ReportIntervalMs: 250,
		Token:            make([]byte, wire.TelemetrySessionTokenSize),
		BroadcastKey:     []byte{0x1a, 0x2b, 0x3c, 0x4d, 0x5e, 0x6f},
	}
}

func waitFor(t *testing.T, d time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Off means off: no URL, no hello, or a disabled fleet all produce zero
// requests — not a queue that never drains.
func TestReporterInertWithoutURL(t *testing.T) {
	c := newCapture(t)
	r := New(Options{Version: "1.2.3"})
	if r.Enabled() {
		t.Fatal("Enabled() true with no URL")
	}
	r.Begin(hello(true))
	r.Report(map[string]any{"encoderFps": 60})
	r.Event("test", "")
	r.Flush(true)
	r.Close()
	if c.count() != 0 {
		t.Errorf("sent %d batches with no URL, want 0", c.count())
	}
}

func TestReporterInertWithoutHello(t *testing.T) {
	c := newCapture(t)
	r := New(Options{URL: c.srv.URL, Version: "1.2.3"})
	r.Report(map[string]any{"encoderFps": 60})
	r.Event("test", "")
	r.Flush(true)
	r.Close()
	if c.count() != 0 {
		t.Errorf("sent %d batches with no hello, want 0", c.count())
	}
}

func TestReporterInertWhenFleetDisabled(t *testing.T) {
	c := newCapture(t)
	r := New(Options{URL: c.srv.URL, Version: "1.2.3"})
	r.Begin(hello(false))
	r.Report(map[string]any{"encoderFps": 60})
	r.Flush(true)
	r.Close()
	if c.count() != 0 {
		t.Errorf("sent %d batches with telemetry disabled, want 0", c.count())
	}
}

// The envelope must match the browser's field-for-field: one service parses
// one shape, and this producer is the second one.
func TestReporterEnvelope(t *testing.T) {
	c := newCapture(t)
	r := New(Options{URL: c.srv.URL, Version: "1.2.3"})
	r.Begin(hello(true))
	r.Report(map[string]any{"encoderFps": 60})
	r.Event("resumed", "attempt 2")
	r.Flush(false)
	r.Close()

	batches := c.batches(t)
	if len(batches) == 0 {
		t.Fatal("no batch sent")
	}
	b := batches[0]
	if b.V != 1 || b.Role != "broadcaster" {
		t.Errorf("v/role = %d/%q, want 1/broadcaster", b.V, b.Role)
	}
	if b.BroadcastKey != "1a2b3c4d5e6f" {
		t.Errorf("broadcastKey = %q, want the obfuscated key", b.BroadcastKey)
	}
	if b.App.Version != "1.2.3" || b.App.Surface != "broadcaster" || b.App.Browser != "gawk-broadcast" {
		t.Errorf("app = %+v, want version 1.2.3 / surface broadcaster / client class gawk-broadcast", b.App)
	}
	if len(b.Samples) != 1 || len(b.Events) != 1 {
		t.Errorf("samples/events = %d/%d, want 1/1", len(b.Samples), len(b.Events))
	}
	if b.Events[0].Kind != "resumed" || b.Events[0].Detail != "attempt 2" {
		t.Errorf("event = %+v", b.Events[0])
	}
	// Zero PII: nothing identifying rides along, and the raw broadcast ID is
	// never even in scope here (only the hello's obfuscated key is).
	c.mu.Lock()
	body := c.bodies[0]
	c.mu.Unlock()
	if strings.Contains(body, "K7XQ2M") {
		t.Error("a raw broadcast ID reached the ingest body")
	}
}

// A reconnect is a new relay session: a new token, a final batch for the old
// one, and numbering that restarts.
func TestReporterNewSessionOnSecondHello(t *testing.T) {
	c := newCapture(t)
	r := New(Options{URL: c.srv.URL, Version: "1.2.3"})
	first := hello(true)
	first.Token = make([]byte, wire.TelemetrySessionTokenSize)
	first.Token[0] = 0x11
	r.Begin(first)
	r.Report(map[string]any{"encoderFps": 60})

	second := hello(true)
	second.Token = make([]byte, wire.TelemetrySessionTokenSize)
	second.Token[0] = 0x22
	r.Begin(second)
	waitFor(t, 5*time.Second, func() bool { return c.count() >= 1 }, "final batch of the first session")

	r.Report(map[string]any{"encoderFps": 30})
	r.Flush(false)
	r.Close()

	batches := c.batches(t)
	if len(batches) < 2 {
		t.Fatalf("got %d batches, want at least 2", len(batches))
	}
	if !batches[0].Final {
		t.Error("the first session's last batch is not marked final")
	}
	if batches[0].Token == batches[1].Token {
		t.Error("both sessions used the same token")
	}
	if batches[1].Seq != 0 {
		t.Errorf("second session seq = %d, want 0 (a new session restarts numbering)", batches[1].Seq)
	}
}

// The relay-requested cadence decimates: reporting faster only resends the
// same numbers.
func TestReporterDecimatesToRequestedCadence(t *testing.T) {
	c := newCapture(t)
	now := time.Unix(1700000000, 0)
	r := New(Options{URL: c.srv.URL, Version: "1", now: func() time.Time { return now }})
	h := hello(true)
	h.ReportIntervalMs = 2000
	r.Begin(h)

	for i := 0; i < 20; i++ {
		r.Report(map[string]any{"i": i})
		now = now.Add(500 * time.Millisecond)
	}
	r.Flush(false)
	r.Close()

	b := c.batches(t)[0]
	if len(b.Samples) < 4 || len(b.Samples) > 6 {
		t.Errorf("samples = %d over 10 s at a 2 s cadence, want ~5", len(b.Samples))
	}
}

// A dead ingest must not stop collection or grow without bound.
func TestReporterDropsAfterRetriesAndKeepsCollecting(t *testing.T) {
	c := newCapture(t)
	c.mu.Lock()
	c.failing = true
	c.mu.Unlock()

	r := New(Options{URL: c.srv.URL, Version: "1"})
	r.Begin(hello(true))
	r.Report(map[string]any{"encoderFps": 60})
	r.Flush(false)
	waitFor(t, 15*time.Second, func() bool { return c.count() >= MaxSendAttempts }, "retries exhausted")

	c.mu.Lock()
	c.failing = false
	before := len(c.bodies)
	c.mu.Unlock()

	// Still collecting: the endpoint coming back must resume reporting.
	time.Sleep(300 * time.Millisecond)
	r.Report(map[string]any{"encoderFps": 30})
	r.Flush(false)
	r.Close()
	if c.count() <= before {
		t.Error("reporter stopped sending after a dead-endpoint episode")
	}
}

// A 4xx means this batch's shape or token is wrong; retrying it unchanged
// would fail identically, so it is dropped immediately rather than three times.
func TestReporterDoesNotRetryClientErrors(t *testing.T) {
	c := newCapture(t)
	c.mu.Lock()
	c.status = http.StatusBadRequest
	c.mu.Unlock()

	r := New(Options{URL: c.srv.URL, Version: "1"})
	r.Begin(hello(true))
	r.Report(map[string]any{"encoderFps": 60})
	r.Flush(false)
	// One attempt for this batch, one for the final batch Close() sends — and
	// crucially not three each.
	r.Close()
	if n := c.count(); n != 2 {
		t.Errorf("sent %d times for 400s, want 2 (one attempt per batch, no retries)", n)
	}
}

// The byte budget degrades to events-only and says so on the wire, so a
// clipped session never reads as a complete short one.
func TestReporterByteBudgetDegradesAndSaysSo(t *testing.T) {
	c := newCapture(t)
	now := time.Unix(1700000000, 0)
	r := New(Options{URL: c.srv.URL, Version: "1", now: func() time.Time { return now }})
	r.Begin(hello(true))

	fat := map[string]any{"blob": strings.Repeat("x", 128*1024)}
	for i := 0; i < 64; i++ {
		now = now.Add(time.Second)
		r.Report(fat)
		r.Flush(false)
	}
	now = now.Add(time.Second)
	r.Report(map[string]any{"encoderFps": 60})
	r.Flush(false)
	r.Close()

	batches := c.batches(t)
	last := batches[len(batches)-1]
	if !last.Truncated {
		t.Fatal("session exceeded its byte budget without saying so")
	}
	if len(last.Samples) != 0 {
		t.Errorf("samples = %d after truncation, want 0 (events only)", len(last.Samples))
	}
	var sawMarker bool
	for _, b := range batches {
		for _, e := range b.Events {
			if e.Kind == "telemetry-budget-exhausted" {
				sawMarker = true
			}
		}
	}
	if !sawMarker {
		t.Error("budget exhaustion never reached the wire as an event")
	}
}

// The buffers are bounded and shed the OLDEST, so the retained window stays
// near live — which is when the samples matter most.
func TestReporterBoundsPendingBuffers(t *testing.T) {
	c := newCapture(t)
	now := time.Unix(1700000000, 0)
	r := New(Options{URL: c.srv.URL, Version: "1", now: func() time.Time { return now }})
	r.Begin(hello(true))
	for i := 0; i < maxPendingSamples+50; i++ {
		now = now.Add(time.Second)
		r.Report(map[string]any{"i": i})
	}
	for i := 0; i < maxPendingEvents+50; i++ {
		r.Event("tick", "")
	}
	r.Flush(false)
	r.Close()

	b := c.batches(t)[0]
	if len(b.Samples) != maxPendingSamples || len(b.Events) != maxPendingEvents {
		t.Fatalf("samples/events = %d/%d, want %d/%d",
			len(b.Samples), len(b.Events), maxPendingSamples, maxPendingEvents)
	}
	var last map[string]any
	raw, _ := json.Marshal(b.Samples[len(b.Samples)-1].Stats)
	_ = json.Unmarshal(raw, &last)
	if last["i"] != float64(maxPendingSamples+49) {
		t.Errorf("newest sample = %v, want the most recent one to survive", last["i"])
	}
}
