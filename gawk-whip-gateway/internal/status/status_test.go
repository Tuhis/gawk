package status

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

var t0 = time.Unix(1_800_000_000, 0)

func h(b byte) Hash { var x Hash; x[0] = b; return x }

// WG6: "the backoff runs 30 s → 60 s → … → 10 min on repeats".
func TestBackoffLadder(t *testing.T) {
	p := BackoffPolicy{Initial: 30 * time.Second, Max: 10 * time.Minute}
	want := []time.Duration{30, 60, 120, 240, 480, 600, 600, 600}
	var r Record
	for i, w := range want {
		if got := p.StartBackoff(&r, t0); got != w*time.Second {
			t.Fatalf("repeat %d: %v, want %vs", i, got, w)
		}
	}
}

// WG7: "the table evicts beyond its bound" (LRU), never a live record.
func TestTableEvictsLRUBeyondItsBoundButNeverLive(t *testing.T) {
	tb := NewTable(3, 10*time.Minute)
	tb.Update(h(1), t0, func(r *Record) { r.State = StateLive })
	for i := byte(2); i <= 5; i++ {
		tb.Update(h(i), t0, func(r *Record) { r.LastError = &ErrorInfo{Reason: ReasonCodec} })
	}
	if tb.Len() != 3 {
		t.Fatalf("len %d", tb.Len())
	}
	if _, ok := tb.Get(h(1), t0); !ok {
		t.Fatal("live record evicted")
	}
	if _, ok := tb.Get(h(2), t0); ok {
		t.Fatal("LRU record kept")
	}
	tb.Get(h(4), t0) // touch
	tb.Update(h(6), t0, func(*Record) {})
	if _, ok := tb.Get(h(4), t0); !ok {
		t.Fatal("recently used record evicted")
	}
	if _, ok := tb.Get(h(3), t0); ok {
		t.Fatal("least recently used record kept")
	}
}

// D9: refusals are reported for the record lifetime (10 min); a record
// outlives its backoff by one lifetime, keeping the ladder's level.
func TestTableExpiry(t *testing.T) {
	tb := NewTable(10, 10*time.Minute)
	tb.Update(h(1), t0, func(r *Record) { r.LastError = &ErrorInfo{Reason: ReasonGatewayFull} })
	if _, ok := tb.Get(h(1), t0.Add(10*time.Minute-time.Second)); !ok {
		t.Fatal("expired early")
	}
	if _, ok := tb.Get(h(1), t0.Add(10*time.Minute)); ok {
		t.Fatal("kept past its lifetime")
	}
	p := BackoffPolicy{Initial: 30 * time.Second, Max: 10 * time.Minute}
	tb.Update(h(2), t0, func(r *Record) { r.BackoffLevel = 5; p.StartBackoff(r, t0) })
	if r, ok := tb.Get(h(2), t0.Add(19*time.Minute)); !ok || r.BackoffLevel != 6 {
		t.Fatal("a record must outlive its backoff by one lifetime")
	}
}

func TestDeleteIfVersionAndPut(t *testing.T) {
	tb := NewTable(10, time.Hour)
	r := tb.Update(h(1), t0, func(r *Record) { r.State = StateEnded })
	if tb.DeleteIfVersion(h(1), r.Version+1) {
		t.Fatal("deleted a different version")
	}
	if !tb.DeleteIfVersion(h(1), r.Version) {
		t.Fatal("ack of the adopted version refused")
	}
	adopted := tb.Put(h(2), Record{UpdatedAt: t0, Version: 7, State: StateEnded})
	if adopted.Version != 7 || !adopted.UpdatedAt.Equal(t0) {
		t.Fatalf("Put: %+v", adopted)
	}
}

// D9's JSON: camelCase, omitted when unknown, warnings always present.
func TestStatusJSON(t *testing.T) {
	var nilRec *Record
	b, _ := json.Marshal(nilRec.Status(t0, nil))
	if string(b) != `{"state":"idle","warnings":[]}` {
		t.Fatalf("idle: %s", b)
	}
	loss, viewers := 0.1, uint32(3)
	rec := &Record{State: StateLive, BackoffUntil: t0.Add(-time.Second)}
	b, _ = json.Marshal(rec.Status(t0, &Snapshot{Codec: "H.264", Width: 1920, Height: 1080, FPS: 60, GOPMs: 2000,
		BitrateKbps: 6000, LossPct: &loss, Viewers: &viewers, Warnings: []Warning{{Reason: WarnGOPLong, GOPMs: 2000}}}))
	want := `{"state":"live","codec":"H.264","resolution":{"width":1920,"height":1080},"fps":60,"gopMs":2000,"bitrateKbps":6000,"lossPct":0.1,"viewers":3,"warnings":[{"reason":"gop_long","gopMs":2000}]}`
	if string(b) != want {
		t.Fatalf("live:\n%s\nwant\n%s", b, want)
	}
	rec = &Record{State: StateEnded, LastError: &ErrorInfo{Reason: ReasonGOPTooLong, GOPMs: 4000}, BackoffUntil: t0.Add(48 * time.Second)}
	b, _ = json.Marshal(rec.Status(t0, nil))
	if !strings.Contains(string(b), `"lastError":{"reason":"gop_too_long","gopMs":4000},"retryAfterMs":48000`) {
		t.Fatalf("ended: %s", b)
	}
}

func TestReasonSetsAreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range append(append([]string{}, WarningReasons...), ErrorReasons...) {
		if seen[r] || r == "" {
			t.Fatalf("reason %q duplicated or empty", r)
		}
		seen[r] = true
	}
	if len(ErrorReasons) != 13 {
		t.Fatalf("%d lastError reasons, the contract fixes 13", len(ErrorReasons))
	}
}
