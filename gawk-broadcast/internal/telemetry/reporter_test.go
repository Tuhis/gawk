package telemetry

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

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

// capturedLogs collects one reporter's log output for assertions.
type capturedLogs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *capturedLogs) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *capturedLogs) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func newLogged(t *testing.T, opts Options) (*Reporter, *capturedLogs) {
	t.Helper()
	logs := &capturedLogs{}
	opts.Log = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return New(opts), logs
}

// Every way of not reporting has to say so, out loud, at the moment it is
// decided.
//
// On 2026-07-27 a native broadcast ran for 25 minutes and reported nothing. The
// relay had minted its token and sent the hello; the client simply never
// started a session. Finding even that much took reading relay pod logs against
// stored telemetry, because the reporter's silence was indistinguishable from a
// crash, a fleet with no key, or a dropped batch — three very different bugs
// with one symptom. A reporter that is inert must be inert *loudly*: the
// difference between "no ingest URL" and "the fleet has telemetry off" is the
// difference between a settings mistake and a deployment one.
func TestReporterSaysWhyItIsNotReporting(t *testing.T) {
	t.Run("no ingest URL", func(t *testing.T) {
		r, logs := newLogged(t, Options{})
		r.Begin(hello(true))
		r.Close()
		if !strings.Contains(logs.String(), "no ingest URL") {
			t.Errorf("a hello with no ingest URL logged nothing about it; got:\n%s", logs.String())
		}
	})

	t.Run("fleet telemetry off", func(t *testing.T) {
		c := newCapture(t)
		r, logs := newLogged(t, Options{URL: c.srv.URL})
		r.Begin(hello(false))
		r.Close()
		if !strings.Contains(logs.String(), "fleet") {
			t.Errorf("a disabled-fleet hello logged nothing about it; got:\n%s", logs.String())
		}
	})
}

// A session that IS reporting says which session it is. The id is derived from
// the token's nonce exactly as the relay derives it, so the line names the same
// row /statusz and the dashboard do — which is what turns "the broadcaster is
// not in the list" into a one-line answer instead of an investigation.
func TestReporterLogsItsSessionID(t *testing.T) {
	c := newCapture(t)
	r, logs := newLogged(t, Options{URL: c.srv.URL})
	h := hello(true)
	want, err := wire.TelemetrySessionID(h.Token)
	if err != nil {
		t.Fatalf("TelemetrySessionID: %v", err)
	}
	r.Begin(h)
	r.Close()
	if !strings.Contains(logs.String(), want) {
		t.Errorf("session start did not log the session id %q; got:\n%s", want, logs.String())
	}
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

// The endpoint is a setting, and settings are edited: a reporter that fixed it
// at construction would need an app restart to honour the field the user just
// typed into. Each session's batches go to the endpoint configured when they
// were produced — a repointed reporter must not deliver the previous session's
// data to the new collector, whose key cannot verify that token anyway.
func TestSetURLRepointsAndDisables(t *testing.T) {
	first, second := newCapture(t), newCapture(t)

	r := New(Options{URL: first.srv.URL, Version: "1.2.3"})
	r.Begin(hello(true))
	r.Report(map[string]any{"encoderFps": 60})
	r.Finish()

	r.SetURL(second.srv.URL)
	r.Begin(hello(true))
	r.Report(map[string]any{"encoderFps": 30})
	r.Finish()

	// Off means the next session produces nothing at all.
	r.SetURL("")
	if r.Enabled() {
		t.Error("Enabled() true after SetURL(\"\")")
	}
	r.Begin(hello(true))
	r.Report(map[string]any{"encoderFps": 15})
	r.Finish()
	r.Close()

	if got := first.count(); got != 1 {
		t.Errorf("first endpoint received %d batches, want 1", got)
	}
	if got := second.count(); got != 1 {
		t.Errorf("second endpoint received %d batches, want exactly its own session's 1", got)
	}
}

// R37 (docs/40 §4.10, D15): a relay-advertised ingest endpoint (wire 0x12)
// wins over the configured one — the relay minted the session's token, so it
// is the one party that can name a destination where that token verifies.
func TestAdvertisedURLWinsOverConfigured(t *testing.T) {
	configured, advertised := newCapture(t), newCapture(t)
	r := New(Options{URL: configured.srv.URL, Version: "1.2.3"})
	r.SetAdvertisedURL(advertised.srv.URL)
	r.Begin(hello(true))
	r.Report(map[string]any{"encoderFps": 60})
	r.Flush(false)
	r.Close()

	if got := advertised.count(); got == 0 {
		t.Error("the advertised endpoint received nothing")
	}
	if got := configured.count(); got != 0 {
		t.Errorf("the configured endpoint received %d batches while an advertised one was active, want 0", got)
	}

	// Clearing falls back to the configured endpoint (a later session against
	// a relay that advertises nothing must not inherit this relay's URL).
	r2 := New(Options{URL: configured.srv.URL, Version: "1.2.3"})
	r2.SetAdvertisedURL(advertised.srv.URL)
	r2.SetAdvertisedURL("")
	if !r2.Enabled() {
		t.Fatal("Enabled() false with a configured URL after the advertised one cleared")
	}
	r2.Begin(hello(true))
	r2.Report(map[string]any{"encoderFps": 30})
	r2.Flush(false)
	r2.Close()
	if got := configured.count(); got == 0 {
		t.Error("clearing the advertised URL did not fall back to the configured endpoint")
	}
}

// The docs/40 §4.10 guard, and its release: with no configured endpoint (a
// foreign relay, nothing set explicitly) a collecting session sends NOTHING —
// its batches could only be rejected at the home deployment's token check.
// And because the 0x12 stream races the 0x0D hello, an advertisement arriving
// *after* the hello must still turn reporting on, buffered recent window
// included.
func TestNoEndpointMeansNoRequestsUntilAdvertised(t *testing.T) {
	c := newCapture(t)
	r := New(Options{Version: "1.2.3"}) // no configured URL: the foreign-relay shape
	r.Begin(hello(true))
	r.Report(map[string]any{"encoderFps": 60})
	r.Event("resumed", "")
	r.Flush(false)
	r.Flush(true)
	if got := c.count(); got != 0 {
		t.Fatalf("sent %d batches with no endpoint anywhere, want 0 — they could only be rejected", got)
	}

	// The late advertisement: hello first, 0x12 second.
	r.SetAdvertisedURL(c.srv.URL)
	r.Flush(false)
	r.Close()
	batches := c.batches(t)
	if len(batches) == 0 {
		t.Fatal("an advertised URL arriving after the hello never turned reporting on")
	}
	if len(batches[0].Samples) == 0 || len(batches[0].Events) == 0 {
		t.Errorf("the pre-advertisement window was lost: samples/events = %d/%d, want the buffered ones",
			len(batches[0].Samples), len(batches[0].Events))
	}
}

// truncateStr() bounds Event Kind/Detail (gawk-telemetry/internal/ingest has
// the same fix under the name clip() — the two Go modules share no util
// package, so the fix is duplicated rather than shared). A byte slice can
// land inside a multi-byte rune, storing invalid UTF-8 that json.Marshal on
// the receiving service then silently rewrites to U+FFFD — corrupting the
// value rather than merely shortening it. Built so the emoji's first byte
// lands exactly on the cut.
func TestTruncateStrStopsOnARuneBoundary(t *testing.T) {
	s := strings.Repeat("a", 63) + "世" + strings.Repeat("z", 10)
	got := truncateStr(s, 64)
	if len(got) > 64 {
		t.Fatalf("truncateStr exceeded the byte budget: len=%d, value=%q", len(got), got)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("truncateStr produced invalid UTF-8: %q", got)
	}
	if want := strings.Repeat("a", 63); got != want {
		t.Errorf("truncateStr(...) = %q, want %q (the split rune dropped whole)", got, want)
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

// 429 is the one 4xx that means "later, not never" (review finding 2): the
// service is shedding load and the batch itself is fine. Treating it as
// permanent made the two collectors disagree about the same status, and threw
// telemetry away exactly when the fleet was busy enough for it to matter.
func TestPostRetriesOn429ButNotOnOther4xx(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		wantRetry bool
	}{
		{"too many requests", http.StatusTooManyRequests, true},
		{"bad request", http.StatusBadRequest, false},
		{"unauthorized", http.StatusUnauthorized, false},
		{"server error", http.StatusInternalServerError, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
			}))
			t.Cleanup(srv.Close)
			r := &Reporter{opts: Options{Client: srv.Client()}}
			done, retry := r.post(outbound{url: srv.URL, body: []byte(`{}`)})
			if done {
				t.Errorf("status %d reported as delivered", tc.status)
			}
			if retry != tc.wantRetry {
				t.Errorf("status %d: retry = %v, want %v", tc.status, retry, tc.wantRetry)
			}
		})
	}
}

// R37 (docs/40 §5 G3): the 250 ms sampling floor is security-load-bearing —
// the hello's interval is relay-chosen and, with 0x12, the destination can be
// a hostile relay's choice too, so the clamp is what bounds the reflector
// rate. A relay asking for 0 ms must not make this reporter sample faster
// than the floor.
func TestReporterClampsBelowFloorInterval(t *testing.T) {
	c := newCapture(t)
	now := time.Unix(1700000000, 0)
	r := New(Options{URL: c.srv.URL, Version: "1", now: func() time.Time { return now }})
	h := hello(true)
	h.ReportIntervalMs = 0 // hostile/misconfigured relay asks for "as fast as possible"
	r.Begin(h)

	r.Report(map[string]any{"i": 0})
	now = now.Add(100 * time.Millisecond) // under the floor — must coalesce
	r.Report(map[string]any{"i": 1})
	now = now.Add(200 * time.Millisecond) // 300 ms total — past the floor
	r.Report(map[string]any{"i": 2})
	r.Flush(false)
	r.Close()

	b := c.batches(t)[0]
	if len(b.Samples) != 2 {
		t.Fatalf("samples = %d, want 2 (the floor must coalesce the 100 ms sample)", len(b.Samples))
	}
}
