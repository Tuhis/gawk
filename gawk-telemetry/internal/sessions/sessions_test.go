package sessions

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/ingest"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/rollup"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/schema"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/store"
)

const (
	testSession   = "000102030405060708090a0b"
	testBroadcast = "1a2b3c4d5e6f"
)

type harness struct {
	store  *store.Store
	writer *Writer
	now    time.Time
	rows   []rollup.Row
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{now: time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)}
	st, err := store.New(store.Options{Root: t.TempDir(), Now: func() time.Time { return h.now }})
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	h.store = st
	t.Cleanup(func() { _ = st.Close() })

	h.writer = NewWriter(Options{
		Store: st,
		Now:   func() time.Time { return h.now },
		Finalize: func(live Live, lines [][]byte) {
			in := ParseTimeline(live, lines)
			if in.EndedAtMs == 0 {
				in.EndedAtMs = h.now.UnixMilli()
			}
			h.rows = append(h.rows, rollup.Compute(in))
		},
	})
	return h
}

func batch(seq int, final bool, samples []ingest.Sample, events []ingest.Event) ingest.Accepted {
	return ingest.Accepted{
		SessionID:    testSession,
		BroadcastKey: testBroadcast,
		Role:         "viewer",
		Seq:          seq,
		Final:        final,
		App:          ingest.AppInfo{Version: "0.33.2", Surface: "viewer", Browser: "Chrome 152", OS: "Windows"},
		StartedAtMs:  1785715200000,
		Samples:      samples,
		Events:       events,
	}
}

func fpsSamples(t0 float64, values ...float64) []ingest.Sample {
	out := make([]ingest.Sample, 0, len(values))
	for i, v := range values {
		out = append(out, ingest.Sample{
			TMs:   t0 + float64(i)*2000,
			Stats: map[string]any{"receivedFps": v, "framesAssembled": float64(i * 60)},
		})
	}
	return out
}

func TestWriterStoresLinesInTheRightPartition(t *testing.T) {
	h := newHarness(t)
	a := batch(0, false, fpsSamples(0, 30, 30, 30), []ingest.Event{{TMs: 100, Kind: "watching"}})
	a.ReceivedAt = h.now
	if err := h.writer.Accept(a); err != nil {
		t.Fatalf("Accept: %v", err)
	}

	ref := store.SessionRef{Date: "2026-07-26", BroadcastKey: testBroadcast, SessionID: testSession}
	lines, err := h.store.ReadSession(ref)
	if err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	// One meta + three samples + one event.
	if len(lines) != 5 {
		t.Fatalf("lines = %d, want 5", len(lines))
	}
	var first Record
	if err := json.Unmarshal(lines[0], &first); err != nil {
		t.Fatal(err)
	}
	if first.Kind != "meta" || first.App == nil || first.App.Browser != "Chrome 152" {
		t.Errorf("first line = %+v, want the meta record", first)
	}
	// Every line stands alone: a reader never depends on file position.
	for i, ln := range lines {
		var rec Record
		if err := json.Unmarshal(ln, &rec); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		if rec.SessionID != testSession || rec.BroadcastKey != testBroadcast || rec.Role != "viewer" {
			t.Errorf("line %d lacks its own identity: %+v", i, rec)
		}
	}
}

// A session's partition is fixed at its FIRST batch, so one spanning midnight
// stays in one file rather than splitting a timeline across two.
func TestSessionStaysInOnePartitionAcrossMidnight(t *testing.T) {
	h := newHarness(t)
	h.now = time.Date(2026, 7, 26, 23, 59, 30, 0, time.UTC)
	a := batch(0, false, fpsSamples(0, 30), nil)
	a.ReceivedAt = h.now
	if err := h.writer.Accept(a); err != nil {
		t.Fatal(err)
	}
	h.now = time.Date(2026, 7, 27, 0, 0, 30, 0, time.UTC)
	b := batch(1, false, fpsSamples(60000, 30), nil)
	b.ReceivedAt = h.now
	if err := h.writer.Accept(b); err != nil {
		t.Fatal(err)
	}

	ref := store.SessionRef{Date: "2026-07-26", BroadcastKey: testBroadcast, SessionID: testSession}
	lines, err := h.store.ReadSession(ref)
	if err != nil {
		t.Fatalf("ReadSession: %v", err)
	}
	if len(lines) != 3 {
		t.Errorf("lines in the start-day partition = %d, want 3 (the whole session)", len(lines))
	}
	if _, err := h.store.ReadSession(store.SessionRef{
		Date: "2026-07-27", BroadcastKey: testBroadcast, SessionID: testSession,
	}); err == nil {
		t.Error("the session also wrote into the next day's partition")
	}
}

func TestFinalBatchFinalizesAndGzips(t *testing.T) {
	h := newHarness(t)
	a := batch(0, false, fpsSamples(0, 30, 30), nil)
	a.ReceivedAt = h.now
	if err := h.writer.Accept(a); err != nil {
		t.Fatal(err)
	}
	f := batch(1, true, nil, []ingest.Event{{TMs: 5000, Kind: "ended"}})
	f.ReceivedAt = h.now
	if err := h.writer.Accept(f); err != nil {
		t.Fatal(err)
	}

	if len(h.rows) != 1 {
		t.Fatalf("rollup rows = %d, want 1", len(h.rows))
	}
	if !h.rows[0].EndedCleanly {
		t.Error("a final batch must record the session as ended cleanly")
	}
	if len(h.writer.LiveSessions()) != 0 {
		t.Error("session still live after its final batch")
	}
	// Finalized ⇒ gzipped, and still readable.
	ref := store.SessionRef{Date: "2026-07-26", BroadcastKey: testBroadcast, SessionID: testSession}
	if lines, err := h.store.ReadSession(ref); err != nil || len(lines) != 4 {
		t.Errorf("post-finalize read = %v / %d lines, want 4", err, len(lines))
	}
}

// The ORDINARY end of a session: a browser tab closed mid-stream sends no
// final batch, and neither does a crashed client — which are precisely the
// sessions worth having.
func TestIdleSweepFinalizesVanishedSessions(t *testing.T) {
	h := newHarness(t)
	a := batch(0, false, fpsSamples(0, 30), nil)
	a.ReceivedAt = h.now
	if err := h.writer.Accept(a); err != nil {
		t.Fatal(err)
	}
	if n := h.writer.SweepIdle(); n != 0 {
		t.Errorf("swept %d fresh sessions, want 0", n)
	}
	h.now = h.now.Add(DefaultIdleTimeout + time.Minute)
	if n := h.writer.SweepIdle(); n != 1 {
		t.Fatalf("swept %d idle sessions, want 1", n)
	}
	if len(h.rows) != 1 {
		t.Fatalf("rollup rows = %d, want 1", len(h.rows))
	}
	if h.rows[0].EndedCleanly {
		t.Error("a vanished session must not be recorded as ended cleanly")
	}
}

// A dropped batch shows up as a gap in `seq` — a diagnosable fact about
// coverage rather than a silent hole.
func TestSeqGapsAreRecorded(t *testing.T) {
	h := newHarness(t)
	for _, seq := range []int{0, 1, 4} { // 2 and 3 were dropped after retries
		a := batch(seq, false, fpsSamples(float64(seq)*2000, 30), nil)
		a.ReceivedAt = h.now
		if err := h.writer.Accept(a); err != nil {
			t.Fatal(err)
		}
	}
	a := batch(5, true, nil, nil)
	a.ReceivedAt = h.now
	if err := h.writer.Accept(a); err != nil {
		t.Fatal(err)
	}
	if h.rows[0].SeqGaps != 2 {
		t.Errorf("seqGaps = %d, want 2", h.rows[0].SeqGaps)
	}
}

// --- TM5: the rollup itself ---------------------------------------------

func fpsTimeline(good int, stallSamples int, more int) []rollup.Sample {
	var samples []rollup.Sample
	tMs := 0.0
	push := func(fps, sinceFrame float64) {
		samples = append(samples, rollup.Sample{TMs: tMs, Stats: map[string]any{
			"receivedFps": fps, "timeSinceLastFrameMs": sinceFrame,
		}})
		tMs += 2000
	}
	for range good {
		push(30, 33)
	}
	for i := range stallSamples {
		push(0, 2000*float64(i+1))
	}
	for range more {
		push(30, 33)
	}
	return samples
}

// The headline case from the acceptance criteria — with the honest limit
// attached, because getting this wrong is how a verdict misses a freeze.
//
// A SUSTAINED degradation shows up in the low tail, which is why p05 is
// carried alongside p95 for fps (the bad tail is low there). A SHORT freeze
// does not, and cannot: two bad samples in 122 is 1.6 %, well under the 5th
// percentile, so p05 correctly reads 30. That is exactly why the row also
// carries explicit stall fields rather than trusting percentiles to surface
// every freeze — a "mean hides it" argument that stopped at percentiles would
// have shipped a rollup that hides it too.
func TestRollupPercentilesAndStallFieldsCoverDifferentFreezes(t *testing.T) {
	t.Run("a short freeze is invisible to p05 but caught by the stall fields", func(t *testing.T) {
		samples := fpsTimeline(60, 2, 60) // one 4 s stall in ~4 minutes
		row := rollup.Compute(rollup.Input{
			SessionID: testSession, BroadcastKey: testBroadcast, Role: "viewer",
			StartedAtMs: 1785715200000, EndedAtMs: 1785715200000 + 244000,
			Samples: samples,
		})
		fps := row.Series["receivedFps"]
		if fps == nil {
			t.Fatal("no receivedFps series")
		}
		if fps.Median != 30 || fps.P05 != 30 {
			t.Errorf("median/p05 = %v/%v; 1.6%% of samples cannot move the 5th percentile", fps.Median, fps.P05)
		}
		// min still records it, and the stall fields name it properly: ONE
		// episode of 4 s, not however many samples it spanned.
		if fps.Min != 0 {
			t.Errorf("min fps = %v, want 0", fps.Min)
		}
		if row.Stalls != 1 {
			t.Errorf("stalls = %d, want 1 episode", row.Stalls)
		}
		if row.LongestStallMs != 4000 || row.TotalStallMs != 4000 {
			t.Errorf("longest/total stall = %v/%v, want 4000/4000", row.LongestStallMs, row.TotalStallMs)
		}
	})

	t.Run("a sustained degradation shows in the low tail", func(t *testing.T) {
		samples := fpsTimeline(80, 12, 8) // 12 % of the session is bad
		row := rollup.Compute(rollup.Input{
			SessionID: testSession, BroadcastKey: testBroadcast, Role: "viewer",
			Samples: samples,
		})
		fps := row.Series["receivedFps"]
		if fps.Median != 30 {
			t.Errorf("median = %v, want 30 (a mean would already be dragged down)", fps.Median)
		}
		if fps.P05 != 0 {
			t.Errorf("p05 = %v, want 0 — a sustained bad tail must be visible", fps.P05)
		}
	})

	t.Run("two separate freezes count as two episodes", func(t *testing.T) {
		samples := append(fpsTimeline(10, 2, 10), fpsTimeline(0, 3, 10)...)
		row := rollup.Compute(rollup.Input{SessionID: testSession, Role: "viewer", Samples: samples})
		if row.Stalls != 2 {
			t.Errorf("stalls = %d, want 2 episodes", row.Stalls)
		}
		if row.LongestStallMs != 6000 {
			t.Errorf("longestStallMs = %v, want 6000 (the deeper of the two)", row.LongestStallMs)
		}
		if row.TotalStallMs != 10000 {
			t.Errorf("totalStallMs = %v, want 10000 (4000 + 6000)", row.TotalStallMs)
		}
	})
}

// Typed by construction: every emitted numeric is a finite number or ABSENT.
// Never a coerced guess, a null, or a zero standing in for "unknown".
func TestRollupIsTypedByConstruction(t *testing.T) {
	// A session whose samples are deliberately full of the junk D15 tolerates.
	junk := map[string]any{
		"receivedFps":          "30",             // wrong type
		"decoderFps":           nil,              // null
		"capToRenderMs":        map[string]any{}, // wrong shape
		"renderedFps":          24.0,             // the one good field
		"timeSinceLastFrameMs": "soon",
	}
	clean, an := schema.SanitizeStats(junk, schema.ViewerFields)
	row := rollup.Compute(rollup.Input{
		SessionID: testSession, BroadcastKey: testBroadcast, Role: "viewer",
		Anomalies: an,
		Samples:   []rollup.Sample{{TMs: 0, Stats: clean}, {TMs: 2000, Stats: clean}},
	})

	for _, absent := range []string{"receivedFps", "decoderFps", "capToRenderMs"} {
		if st, present := row.Series[absent]; present {
			t.Errorf("%s produced a series %+v from unusable values; want absent", absent, st)
		}
	}
	if st := row.Series["renderedFps"]; st == nil || st.Median != 24 {
		t.Errorf("renderedFps = %+v, want a real series", st)
	}
	// The junk is visible as a tally rather than as silently missing data.
	if row.SchemaAnomalies.Dropped == 0 {
		t.Error("schemaAnomalies did not record the dropped fields")
	}

	// Every value that DID make it must be finite. Serialize and re-read: JSON
	// is where a NaN would surface as a marshal error or a literal `null`.
	b, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("a rollup row must always serialize: %v", err)
	}
	if strings.Contains(string(b), "NaN") || strings.Contains(string(b), "Inf") {
		t.Errorf("row contains a non-finite value: %s", b)
	}
}

// An older release's row must still load and query — the additive-forever
// rule (D4) is what makes the permanent artifact safe to keep.
func TestRollupRowFromAnOlderSchemaStillLoads(t *testing.T) {
	old := []byte(`{
	  "sessionId":"000102030405060708090a0b",
	  "broadcastKey":"1a2b3c4d5e6f",
	  "role":"viewer",
	  "relayCoverage":"none",
	  "schemaAnomalies":{"dropped":0,"coerced":0,"unknown":0},
	  "samples":10,
	  "series":{"receivedFps":{"n":10,"min":29,"p05":29,"median":30,"p95":31,"max":31}}
	}`)
	var row rollup.Row
	if err := json.Unmarshal(old, &row); err != nil {
		t.Fatalf("an older row failed to load: %v", err)
	}
	if row.SessionID != testSession || row.Series["receivedFps"].Median != 30 {
		t.Errorf("row = %+v", row)
	}
	// Fields this release added are simply absent, not wrong.
	if row.Stalls != 0 || row.Counters != nil || row.Verdict != nil {
		t.Errorf("newer fields on an older row = %d / %v / %v, want zero values", row.Stalls, row.Counters, row.Verdict)
	}
}

// The whole point of the raw/permanent split: rollups outlive the samples they
// were computed from.
func TestRollupsSurviveARawPrune(t *testing.T) {
	h := newHarness(t)
	h.writer = NewWriter(Options{
		Store: h.store, Now: func() time.Time { return h.now },
		Finalize: RollupFinalizer(h.store, nil, nil, func() time.Time { return h.now }),
	})
	a := batch(0, true, fpsSamples(0, 30, 30), nil)
	a.ReceivedAt = h.now
	if err := h.writer.Accept(a); err != nil {
		t.Fatal(err)
	}

	// Prune everything, including today.
	if _, err := h.store.Prune(h.now.Add(24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.store.ReadSession(store.SessionRef{
		Date: "2026-07-26", BroadcastKey: testBroadcast, SessionID: testSession,
	}); err == nil {
		t.Error("raw session survived a prune past its date")
	}
	rows, err := h.store.ReadRollups(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rollups after prune = %d, want 1 (permanent)", len(rows))
	}
	var row rollup.Row
	if err := json.Unmarshal(rows[0], &row); err != nil {
		t.Fatal(err)
	}
	if row.SessionID != testSession || row.Series["receivedFps"] == nil {
		t.Errorf("surviving rollup lost its content: %+v", row)
	}
}

// A session that hit its byte budget or lost batches says so on the row — a
// verdict computed over it is a verdict to distrust, and that must be visible.
func TestRollupCarriesTruncationAndGaps(t *testing.T) {
	row := rollup.Compute(rollup.Input{
		SessionID: testSession, BroadcastKey: testBroadcast, Role: "viewer",
		Truncated: true, SeqGaps: 3,
		Anomalies: schema.Anomalies{Dropped: 4, Unknown: 12, Coerced: 1},
		Samples:   []rollup.Sample{{TMs: 0, Stats: map[string]any{"receivedFps": 30.0}}},
	})
	if !row.Truncated || row.SeqGaps != 3 {
		t.Errorf("truncated/seqGaps = %v/%d, want true/3", row.Truncated, row.SeqGaps)
	}
	if row.SchemaAnomalies.Dropped != 4 || row.SchemaAnomalies.Unknown != 12 {
		t.Errorf("schemaAnomalies = %+v", row.SchemaAnomalies)
	}
}

func TestRollupExtractsCloseCodesAndReconnects(t *testing.T) {
	row := rollup.Compute(rollup.Input{
		SessionID: testSession, Role: "viewer",
		Events: []rollup.Event{
			{TMs: 1000, Kind: "reconnect", Detail: "attempt 1 close 4002: draining"},
			{TMs: 2000, Kind: "reconnect", Detail: "attempt 2 close 4002: draining"},
			{TMs: 9000, Kind: "error", Detail: "lost: close 4000 broadcast ended"},
			{TMs: 9500, Kind: "ended"},
		},
	})
	if row.Reconnects != 2 {
		t.Errorf("reconnects = %d, want 2", row.Reconnects)
	}
	if len(row.CloseCodes) != 2 || row.CloseCodes[0] != "4000" || row.CloseCodes[1] != "4002" {
		t.Errorf("closeCodes = %v, want [4000 4002]", row.CloseCodes)
	}
}

// A session that reported nothing usable still gets a row: "this session
// reported nothing" is itself an answer someone will need.
func TestRollupOfAnEmptySession(t *testing.T) {
	row := rollup.Compute(rollup.Input{SessionID: testSession, BroadcastKey: testBroadcast, Role: "viewer"})
	if row.SessionID != testSession {
		t.Error("empty session produced no identity")
	}
	if row.Series != nil || row.Counters != nil {
		t.Errorf("empty session invented series/counters: %v / %v", row.Series, row.Counters)
	}
	if row.RelayCoverage != "none" {
		t.Errorf("relayCoverage = %q, want none", row.RelayCoverage)
	}
	if _, err := json.Marshal(row); err != nil {
		t.Errorf("empty row does not serialize: %v", err)
	}
}
