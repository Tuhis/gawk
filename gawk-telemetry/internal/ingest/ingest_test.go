package ingest

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/wire"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/schema"
)

var (
	testKey          = bytes.Repeat([]byte{0x5a}, wire.TelemetryKeySize)
	otherKey         = bytes.Repeat([]byte{0x11}, wire.TelemetryKeySize)
	testBroadcastKey = []byte{0x1a, 0x2b, 0x3c, 0x4d, 0x5e, 0x6f}
	testNow          = time.Unix(1785715200, 0).UTC()
)

type recordingSink struct {
	batches []Accepted
	err     error
}

func (s *recordingSink) Accept(a Accepted) error {
	if s.err != nil {
		return s.err
	}
	s.batches = append(s.batches, a)
	return nil
}

func newHandler(t *testing.T, sink Sink) *Handler {
	t.Helper()
	h, err := New(Options{Key: testKey, Sink: sink, Now: func() time.Time { return testNow }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

func mintToken(t *testing.T, key []byte, role wire.TelemetryRole) string {
	t.Helper()
	tok, err := wire.MintTelemetrySessionToken(key, testBroadcastKey, role, testNow)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return hex.EncodeToString(tok)
}

// body builds a valid envelope, then applies overrides by raw JSON surgery so
// a test can make ONE field wrong without the Go type system fixing it first.
func body(t *testing.T, overrides map[string]any) []byte {
	t.Helper()
	base := map[string]any{
		"v":            1,
		"token":        mintToken(t, testKey, wire.TelemetryRoleViewer),
		"role":         "viewer",
		"broadcastKey": hex.EncodeToString(testBroadcastKey),
		"seq":          0,
		"final":        false,
		"app": map[string]any{
			"version": "0.33.2", "surface": "viewer", "browser": "Chrome 152", "os": "Windows",
		},
		"startedAtMs": 1785715200000,
		"samples":     []any{map[string]any{"tMs": 0, "stats": map[string]any{"receivedFps": 30}}},
		"events":      []any{},
	}
	for k, v := range overrides {
		if v == nil {
			delete(base, k)
			continue
		}
		base[k] = v
	}
	b, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// --- envelope strictness (D15) ------------------------------------------

func TestIngestAcceptsAValidBatch(t *testing.T) {
	sink := &recordingSink{}
	h := newHandler(t, sink)
	a, status, err := h.Validate(body(t, nil))
	if err != nil {
		t.Fatalf("Validate: %v (status %d)", err, status)
	}
	if len(a.SessionID) != wire.TelemetrySessionIDLen {
		t.Errorf("sessionId = %q, want %d hex chars", a.SessionID, wire.TelemetrySessionIDLen)
	}
	if a.Role != "viewer" || a.BroadcastKey != hex.EncodeToString(testBroadcastKey) {
		t.Errorf("role/key = %q/%q", a.Role, a.BroadcastKey)
	}
	if len(a.Samples) != 1 {
		t.Fatalf("samples = %d, want 1", len(a.Samples))
	}
	if got, ok := schema.Number(a.Samples[0].Stats, "receivedFps"); !ok || got != 30 {
		t.Errorf("receivedFps = %v/%v, want 30", got, ok)
	}
}

// The sessionId is the AUTHENTICATED one, taken from the verified token —
// never from anything the client asserted alongside it.
func TestIngestSessionIDComesFromTheToken(t *testing.T) {
	h := newHandler(t, &recordingSink{})
	tok, err := wire.MintTelemetrySessionToken(testKey, testBroadcastKey, wire.TelemetryRoleViewer, testNow)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := wire.TelemetrySessionID(tok)

	// A batch that also claims a different sessionId in an unknown envelope
	// field must not be able to influence the answer.
	a, _, err := h.Validate(body(t, map[string]any{
		"token":     hex.EncodeToString(tok),
		"sessionId": "ffffffffffffffffffffffff",
	}))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if a.SessionID != want {
		t.Errorf("sessionId = %q, want the token's %q", a.SessionID, want)
	}
}

func TestIngestRejectsEnvelopeViolations(t *testing.T) {
	h := newHandler(t, &recordingSink{})
	viewerTok := mintToken(t, testKey, wire.TelemetryRoleViewer)

	cases := []struct {
		name string
		body []byte
		want int
	}{
		{"malformed JSON", []byte("{not json"), http.StatusBadRequest},
		{"empty body", []byte(""), http.StatusBadRequest},
		{"missing v", body(t, map[string]any{"v": nil}), http.StatusBadRequest},
		{"unknown v", body(t, map[string]any{"v": 2}), http.StatusBadRequest},
		{"wrongly-typed v", body(t, map[string]any{"v": "1"}), http.StatusBadRequest},
		{"missing seq", body(t, map[string]any{"seq": nil}), http.StatusBadRequest},
		{"negative seq", body(t, map[string]any{"seq": -1}), http.StatusBadRequest},
		{"missing final", body(t, map[string]any{"final": nil}), http.StatusBadRequest},
		{"missing startedAtMs", body(t, map[string]any{"startedAtMs": nil}), http.StatusBadRequest},
		{"missing app.version", body(t, map[string]any{
			"app": map[string]any{"surface": "viewer"},
		}), http.StatusBadRequest},
		{"unknown role", body(t, map[string]any{"role": "edge"}), http.StatusBadRequest},
		// An edge is plumbing, not a client (docs/33 §4.1) — it is never even
		// issued a token, and the role vocabulary refuses to name it.
		{"role and surface disagree", body(t, map[string]any{
			"role": "broadcaster",
			"app":  map[string]any{"version": "1", "surface": "viewer"},
		}), http.StatusBadRequest},
		{"bad broadcastKey hex", body(t, map[string]any{"broadcastKey": "zzzzzzzzzzzz"}), http.StatusBadRequest},
		// A client reporting the RAW joinable broadcast ID instead of the
		// obfuscated key fails here — which is the mechanism behind D8's
		// claim, not merely a convention.
		{"raw broadcast ID in the key field", body(t, map[string]any{"broadcastKey": "K7XQ2M"}), http.StatusBadRequest},
		{"bad token hex", body(t, map[string]any{"token": "nothex"}), http.StatusUnauthorized},
		{"short token", body(t, map[string]any{"token": viewerTok[:10]}), http.StatusUnauthorized},
		{"token from another fleet", body(t, map[string]any{
			"token": mintToken(t, otherKey, wire.TelemetryRoleViewer),
		}), http.StatusUnauthorized},
		{"viewer token claiming broadcaster", body(t, map[string]any{
			"role": "broadcaster",
			"app":  map[string]any{"version": "1", "surface": "broadcaster"},
		}), http.StatusUnauthorized},
		{"samples not an array", body(t, map[string]any{"samples": 5}), http.StatusBadRequest},
		{"sample stats not an object", body(t, map[string]any{
			"samples": []any{map[string]any{"tMs": 0, "stats": 5}},
		}), http.StatusBadRequest},
		{"sample missing tMs", body(t, map[string]any{
			"samples": []any{map[string]any{"stats": map[string]any{}}},
		}), http.StatusBadRequest},
		{"sample tMs not a number", body(t, map[string]any{
			"samples": []any{map[string]any{"tMs": "0", "stats": map[string]any{}}},
		}), http.StatusBadRequest},
		{"events not an array", body(t, map[string]any{"events": "nope"}), http.StatusBadRequest},
		{"event with empty kind", body(t, map[string]any{
			"events": []any{map[string]any{"tMs": 1, "kind": ""}},
		}), http.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, status, err := h.Validate(tc.body)
			if err == nil {
				t.Fatalf("accepted %s; want rejection", tc.name)
			}
			if status != tc.want {
				t.Errorf("status = %d, want %d (%v)", status, tc.want, err)
			}
		})
	}
}

// An expired token is rejected — but distinguishably from a forged one only in
// the log, never in the response: on a public endpoint that difference is a
// free oracle, and a client can act on neither.
func TestIngestRejectsExpiredToken(t *testing.T) {
	sink := &recordingSink{}
	h, err := New(Options{Key: testKey, Sink: sink, Now: func() time.Time {
		return testNow.Add(wire.TelemetryTokenTTL + time.Hour)
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, status, err := h.Validate(body(t, nil)); err == nil || status != http.StatusUnauthorized {
		t.Errorf("status = %d, err = %v; want 401", status, err)
	}
}

// A rejected batch writes NOTHING. A partial write would leave a hole in a
// session's timeline that reads exactly like a network gap.
func TestIngestRejectionWritesNothing(t *testing.T) {
	sink := &recordingSink{}
	h := newHandler(t, sink)
	srv := httptest.NewServer(h)
	defer srv.Close()

	for _, b := range [][]byte{
		[]byte("{not json"),
		body(t, map[string]any{"v": 2}),
		body(t, map[string]any{"token": mintToken(t, otherKey, wire.TelemetryRoleViewer)}),
		body(t, map[string]any{"samples": []any{map[string]any{"tMs": 0, "stats": 5}}}),
	} {
		resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Errorf("status = %d for a bad batch, want 4xx", resp.StatusCode)
		}
	}
	if len(sink.batches) != 0 {
		t.Errorf("sink saw %d batches from rejected requests, want 0", len(sink.batches))
	}
}

func TestIngestStructuralBounds(t *testing.T) {
	h := newHandler(t, &recordingSink{})

	tooManySamples := make([]any, schema.MaxSamplesPerBatch+1)
	for i := range tooManySamples {
		tooManySamples[i] = map[string]any{"tMs": i, "stats": map[string]any{}}
	}
	atLimitSamples := tooManySamples[:schema.MaxSamplesPerBatch]

	tooManyEvents := make([]any, schema.MaxEventsPerBatch+1)
	for i := range tooManyEvents {
		tooManyEvents[i] = map[string]any{"tMs": i, "kind": "tick"}
	}
	atLimitEvents := tooManyEvents[:schema.MaxEventsPerBatch]

	// Boundary: exactly at the limit is accepted, one over is not.
	if _, _, err := h.Validate(body(t, map[string]any{"samples": atLimitSamples})); err != nil {
		t.Errorf("rejected exactly MaxSamplesPerBatch: %v", err)
	}
	if _, status, err := h.Validate(body(t, map[string]any{"samples": tooManySamples})); err == nil {
		t.Errorf("accepted MaxSamplesPerBatch+1 (status %d)", status)
	}
	if _, _, err := h.Validate(body(t, map[string]any{"events": atLimitEvents})); err != nil {
		t.Errorf("rejected exactly MaxEventsPerBatch: %v", err)
	}
	if _, status, err := h.Validate(body(t, map[string]any{"events": tooManyEvents})); err == nil {
		t.Errorf("accepted MaxEventsPerBatch+1 (status %d)", status)
	}
}

// The bounds are enforced regardless of any field's TYPE — that is what keeps
// them from rotting as the stats objects grow.
func TestIngestStructuralBoundsAreTypeAgnostic(t *testing.T) {
	h := newHandler(t, &recordingSink{})

	deep := any(map[string]any{"leaf": 1})
	for range schema.MaxDepth + 4 {
		deep = map[string]any{"nest": deep}
	}
	wide := map[string]any{}
	for i := range schema.MaxFieldsPerObject + 100 {
		wide[fmt.Sprintf("f%d", i)] = i
	}
	long := strings.Repeat("x", schema.MaxStringLen*3)

	a, _, err := h.Validate(body(t, map[string]any{
		"samples": []any{map[string]any{"tMs": 0, "stats": map[string]any{
			"deep": deep, "wide": wide, "long": long, "receivedFps": 30,
		}}},
	}))
	// None of these are fatal — they are payload, and the payload never
	// rejects. They are bounded, counted and the batch survives.
	if err != nil {
		t.Fatalf("structural excess rejected the batch: %v", err)
	}
	if a.Anomalies.Total() == 0 {
		t.Error("structural excess produced no anomalies")
	}
	if got, ok := schema.String(a.Samples[0].Stats, "long"); !ok || len(got) != schema.MaxStringLen {
		t.Errorf("long string len = %d, want truncated to %d", len(got), schema.MaxStringLen)
	}
	if a.Anomalies.Coerced == 0 {
		t.Error("a truncated string was not counted as a coercion")
	}
	// The healthy field beside them survives untouched.
	if v, ok := schema.Number(a.Samples[0].Stats, "receivedFps"); !ok || v != 30 {
		t.Errorf("receivedFps = %v/%v, want 30 alongside the bounded fields", v, ok)
	}
}

func TestIngestRejectsOversizeBody(t *testing.T) {
	h := newHandler(t, &recordingSink{})
	srv := httptest.NewServer(h)
	defer srv.Close()

	huge := bytes.Repeat([]byte("a"), schema.MaxBodyBytes+1024)
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(huge))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", resp.StatusCode)
	}
}

// --- payload tolerance (D15) --------------------------------------------

// The skew case that motivates the whole decision: a "next release" stats
// object with fields this build has never heard of must ingest cleanly, and
// the fields must survive verbatim so they become queryable the day the
// service learns their names.
func TestIngestAcceptsUnknownFieldsVerbatim(t *testing.T) {
	h := newHandler(t, &recordingSink{})
	next := map[string]any{"receivedFps": 30.0}
	for i := range 20 {
		next[fmt.Sprintf("brandNewFieldFromR29_%d", i)] = float64(i)
	}
	next["nestedNewThing"] = map[string]any{"a": 1.0, "b": "two"}

	a, _, err := h.Validate(body(t, map[string]any{
		"samples": []any{map[string]any{"tMs": 0, "stats": next}},
	}))
	if err != nil {
		t.Fatalf("a newer client's stats object was rejected: %v", err)
	}
	stats := a.Samples[0].Stats
	for i := range 20 {
		name := fmt.Sprintf("brandNewFieldFromR29_%d", i)
		if got, ok := schema.Number(stats, name); !ok || got != float64(i) {
			t.Errorf("%s = %v/%v, want it to survive verbatim", name, got, ok)
		}
	}
	nested, _ := stats["nestedNewThing"].(map[string]any)
	if nested == nil || nested["b"] != "two" {
		t.Errorf("nested unknown object did not survive: %v", stats["nestedNewThing"])
	}
	// Counted, not complained about: a spike of unknowns means clients are
	// running ahead of the service, which is information.
	if a.Anomalies.Unknown < 21 {
		t.Errorf("unknown count = %d, want >= 21", a.Anomalies.Unknown)
	}
	if a.Anomalies.Dropped != 0 {
		t.Errorf("dropped = %d; unknown fields must never be dropped", a.Anomalies.Dropped)
	}
}

// A wrongly-typed KNOWN field is dropped and counted — never fatal, and
// crucially never allowed to reach a numeric series.
func TestIngestDropsWronglyTypedKnownFields(t *testing.T) {
	h := newHandler(t, &recordingSink{})
	bad := map[string]any{
		"receivedFps":           "30", // the string that must never become a data point
		"decoderFps":            nil,
		"capToRenderMs":         map[string]any{"oops": 1},
		"isHardwareAccelerated": "yes",
		"deliveryMode":          7,
		"framesAssembled":       120.0, // healthy, alongside
	}
	a, _, err := h.Validate(body(t, map[string]any{
		"samples": []any{map[string]any{"tMs": 0, "stats": bad}},
	}))
	if err != nil {
		t.Fatalf("wrongly-typed payload fields rejected the batch: %v", err)
	}
	stats := a.Samples[0].Stats
	for _, f := range []string{"receivedFps", "decoderFps", "capToRenderMs", "isHardwareAccelerated", "deliveryMode"} {
		if _, present := stats[f]; present {
			t.Errorf("%s survived with the wrong type: %#v", f, stats[f])
		}
		if _, ok := schema.Number(stats, f); ok {
			t.Errorf("%s reached the numeric reader", f)
		}
	}
	if got, ok := schema.Number(stats, "framesAssembled"); !ok || got != 120 {
		t.Errorf("framesAssembled = %v/%v, want 120 (a healthy field beside bad ones)", got, ok)
	}
	if a.Anomalies.Dropped != 5 {
		t.Errorf("dropped = %d, want 5", a.Anomalies.Dropped)
	}
}

// Non-finite numbers are dropped, not stored: a NaN entering a percentile
// series poisons every quantile computed from it.
func TestIngestDropsNonFiniteNumbers(t *testing.T) {
	h := newHandler(t, &recordingSink{})
	// JSON has no NaN literal, so this arrives the way it actually would — as
	// a string a lenient encoder produced.
	raw := `{"v":1,"token":"` + mintToken(t, testKey, wire.TelemetryRoleViewer) + `",` +
		`"role":"viewer","broadcastKey":"` + hex.EncodeToString(testBroadcastKey) + `",` +
		`"seq":0,"final":false,"app":{"version":"1","surface":"viewer","browser":"x","os":"y"},` +
		`"startedAtMs":1785715200000,` +
		`"samples":[{"tMs":0,"stats":{"receivedFps":"NaN","decoderFps":1e400,"framesAssembled":5}}],"events":[]}`
	a, _, err := h.Validate([]byte(raw))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	stats := a.Samples[0].Stats
	if _, ok := schema.Number(stats, "receivedFps"); ok {
		t.Error(`"NaN" survived into a numeric field`)
	}
	if _, ok := schema.Number(stats, "decoderFps"); ok {
		t.Error("an overflowing number survived into a numeric field")
	}
	if v, _ := schema.Number(stats, "framesAssembled"); v != 5 {
		t.Errorf("framesAssembled = %v, want 5", v)
	}
}

// --- transport-level behaviour ------------------------------------------

func TestIngestRejectsNonPost(t *testing.T) {
	h := newHandler(t, &recordingSink{})
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", resp.StatusCode)
	}
}

func TestIngestRateLimits(t *testing.T) {
	sink := &recordingSink{}
	h, err := New(Options{
		Key: testKey, Sink: sink, RatePerSec: 1, Burst: 2,
		Now: func() time.Time { return testNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	var limited bool
	for range 6 {
		resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body(t, nil)))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			limited = true
		}
	}
	if !limited {
		t.Error("a burst past the limit was never rate-limited")
	}
}

func TestIngestSinkFailureIs500(t *testing.T) {
	h := newHandler(t, &recordingSink{err: fmt.Errorf("disk full")})
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Post(srv.URL, "application/json", bytes.NewReader(body(t, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	// 5xx, so the client retries — unlike a 4xx, which it drops immediately.
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
}

func TestNewRequiresAKeyAndASink(t *testing.T) {
	if _, err := New(Options{Sink: &recordingSink{}}); err == nil {
		t.Error("New accepted a missing key")
	}
	if _, err := New(Options{Key: testKey[:16], Sink: &recordingSink{}}); err == nil {
		t.Error("New accepted a short key")
	}
	if _, err := New(Options{Key: testKey}); err == nil {
		t.Error("New accepted a missing sink")
	}
}
