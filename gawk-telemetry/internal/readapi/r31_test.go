package readapi

// R31's read-surface tests (docs/36). They are grouped by chunk so a failing
// one names the acceptance criterion it belongs to.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/annotations"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/ingest"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/live"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/rollup"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/rules"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/schema"
)

// --- UD1: the machine surface does not move --------------------------------

// The single most load-bearing test in this file. UD1 says every existing
// default stays byte-identical, and the reason is not neatness: `get_session`'s
// default response is what a model pays for, and R31 added a placement
// envelope, a window, a field list and a full-resolution mode to the same
// endpoint. Any one of them leaking into the default would spend context on a
// browser's convenience.
func TestR31DefaultsAreByteIdentical(t *testing.T) {
	f := newFixture(t)
	sid := "00a1a2a3a4a5a6a7a8a9aaab"
	f.seed(t, sid, "viewer", healthyViewerStats(300), []ingest.Event{{TMs: 1000, Kind: "watching"}})

	viaGo, err := f.api.GetSession(sid, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	viaZero, err := f.api.GetSessionDetail(sid, SessionQuery{})
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(viaGo)
	b, _ := json.Marshal(viaZero)
	if string(a) != string(b) {
		t.Fatalf("a zero SessionQuery is not the old default:\n old: %s\n new: %s", a, b)
	}
	// And none of the new envelope keys may appear without being asked for.
	var got map[string]any
	if err := json.Unmarshal(a, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{
		"broadcastKey", "startedAtMs", "endedAtMs", "clockOffsetMs",
		"step", "fromMs", "toMs", "available", "truncated",
	} {
		if _, present := got[key]; present {
			t.Errorf("default get_session response carries R31's %q; UD1 says it must not", key)
		}
	}

	// The HTTP default must match the Go default too, since MCP and curl are
	// the two things that see it.
	srv := httptest.NewServer(f.api.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/sessions/" + sid)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var viaHTTP Timeline
	if err := json.NewDecoder(resp.Body).Decode(&viaHTTP); err != nil {
		t.Fatal(err)
	}
	c, _ := json.Marshal(&viaHTTP)
	if string(c) != string(a) {
		t.Errorf("HTTP default != Go default:\n http: %s\n go:   %s", c, a)
	}
}

// --- TH2: session detail from disk -----------------------------------------

// §1.1's finding, as a test: `store.ReadSession` flushes the open writer, so a
// session that is STILL RUNNING has its full-resolution timeline on disk and
// already served. The client-side accumulation R31 deletes was never necessary.
func TestLiveSessionShowsEverySampleFromDisk(t *testing.T) {
	f := newFixture(t)
	sid := "00b1b2b3b4b5b6b7b8b9babb"
	// Deliberately NOT final: this is a session in progress.
	if err := f.writer.Accept(ingest.Accepted{
		SessionID: sid, BroadcastKey: bkey, Role: "viewer", Seq: 0,
		App:         ingest.AppInfo{Version: "0.33.2", Surface: "viewer"},
		StartedAtMs: f.now.UnixMilli(), ReceivedAt: f.now,
		Samples: samplesOf(healthyViewerStats(120)),
	}); err != nil {
		t.Fatal(err)
	}

	tl, err := f.api.GetSessionDetail(sid, SessionQuery{Full: true, Detail: true})
	if err != nil {
		t.Fatalf("a live session must be readable: %v", err)
	}
	if tl.TotalSample != 120 {
		t.Errorf("totalSamples = %d, want 120 — the open session's file was not fully read", tl.TotalSample)
	}
	if len(tl.Points) != 120 {
		t.Errorf("full resolution returned %d points for 120 samples", len(tl.Points))
	}
	if tl.StartedAtMs == 0 {
		t.Error("no startedAtMs: absolute time (UD5) is not recoverable from tMs alone")
	}
}

// TH2's criterion "a live session updates as batches land" needs the page to
// KNOW a session is live, and `endedAtMs` cannot tell it: ParseTimeline falls
// back to the last `receivedAtMs` when nothing else supplies an end, so every
// session — open or finished — comes back with one. A detail page testing for
// its absence would never refresh.
//
// The projection is the only thing that knows, so it is what is asked.
func TestALiveSessionIsMarkedLive(t *testing.T) {
	f := newFixture(t)
	projection := live.New(func() time.Time { return f.now })
	api, err := New(Options{
		Store: f.store, Live: projection, Now: func() time.Time { return f.now },
	})
	if err != nil {
		t.Fatal(err)
	}

	open := "0100000000000000000000aa"
	ended := "0100000000000000000000bb"
	f.seed(t, ended, "viewer", healthyViewerStats(10), nil)

	// An open session: batches landed, no `final`, and the projection has been
	// told — exactly what the writer's Observe hook does in production.
	batch := ingest.Accepted{
		SessionID: open, BroadcastKey: bkey, Role: "viewer", Seq: 0,
		App:         ingest.AppInfo{Version: "0.33.2", Surface: "viewer"},
		StartedAtMs: f.now.UnixMilli(), ReceivedAt: f.now,
		Samples: samplesOf(healthyViewerStats(10)),
	}
	if err := f.writer.Accept(batch); err != nil {
		t.Fatal(err)
	}
	projection.ObserveClient(batch, "Chrome 152", "Windows", "0.33.2")

	liveTL, err := api.GetSessionDetail(open, SessionQuery{Detail: true})
	if err != nil {
		t.Fatal(err)
	}
	if liveTL.EndedAtMs == 0 {
		t.Fatal("fixture assumption broken: an open session came back with no endedAtMs, " +
			"which would have made this bug invisible")
	}
	if !liveTL.Live {
		t.Error("an open session is not marked live; the detail page would never refresh it")
	}

	endedTL, err := api.GetSessionDetail(ended, SessionQuery{Detail: true})
	if err != nil {
		t.Fatal(err)
	}
	if endedTL.Live {
		t.Error("a finished session is marked live; the page would poll it forever")
	}
}

// A live detail page must fetch what has ARRIVED, not re-read the file per
// tick. The window is the mechanism, and it has to actually narrow the work.
func TestWindowedFetchReturnsOnlyTheWindow(t *testing.T) {
	f := newFixture(t)
	sid := "00c1c2c3c4c5c6c7c8c9cacb"
	f.seed(t, sid, "viewer", healthyViewerStats(100), []ingest.Event{
		{TMs: 1000, Kind: "early"}, {TMs: 150000, Kind: "late"},
	})
	start := f.now.UnixMilli()

	// Samples are 2 s apart, so 100 of them span 198 s. Ask for the last 40 s.
	tl, err := f.api.GetSessionDetail(sid, SessionQuery{
		FromMs: start + 160000, Full: true, Detail: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if tl.TotalSample == 0 || tl.TotalSample >= 100 {
		t.Fatalf("windowed totalSamples = %d, want a strict subset of 100", tl.TotalSample)
	}
	for _, p := range tl.Points {
		if at := start + int64(p["tMs"]); at < start+160000 {
			t.Fatalf("point at %d is before the window start", at)
		}
	}
	// Events are windowed with the samples: an event outside the window belongs
	// to a moment the caller did not ask about.
	for _, e := range tl.Events {
		if e.Kind == "early" {
			t.Error("an event before the window survived into a windowed response")
		}
	}
}

// UD10 in its most concrete form: a request for a session that does not exist
// is a 404, never a blank page or an endless spinner (TH1's criterion, checked
// at the layer that decides it).
func TestUnknownAndMalformedSessionsAreDistinguishable(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(f.api.Handler())
	defer srv.Close()

	cases := []struct {
		path string
		want int
	}{
		{"/v1/sessions/000000000000000000000000", http.StatusNotFound},
		{"/v1/sessions/not-hex", http.StatusBadRequest},
		{"/v1/sessions/000000000000000000000000/dips", http.StatusNotFound},
		{"/v1/broadcasts/zzzzzzzzzzzz", http.StatusBadRequest},
	}
	for _, c := range cases {
		resp, err := http.Get(srv.URL + c.path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("GET %s = %d, want %d", c.path, resp.StatusCode, c.want)
		}
	}
}

// The boundary §1.1 draws: reading ONE file because a human clicked is fine;
// the fleet scan touching every session every 2 s is not. Nothing in R31 may
// erode that, and a counter is what makes the erosion visible.
func TestLivePathReadsNoSessionFile(t *testing.T) {
	f := newFixture(t)
	sid := "00d1d2d3d4d5d6d7d8d9dadb"
	f.seed(t, sid, "viewer", healthyViewerStats(20), nil)

	srv := httptest.NewServer(f.api.Handler())
	defer srv.Close()

	before := f.store.SessionReads()
	for range 5 {
		resp, err := http.Get(srv.URL + "/live")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if after := f.store.SessionReads(); after != before {
		t.Errorf("/live opened %d session file(s); the fleet scan must never read one", after-before)
	}
	// And the contrast: a session detail request DOES read one, so the counter
	// is measuring something rather than always being zero.
	resp, err := http.Get(srv.URL + "/v1/sessions/" + sid + "?detail=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if f.store.SessionReads() == before {
		t.Error("the session detail path read no file; the counter is not wired")
	}
}

// --- TH3: history browser --------------------------------------------------

func TestHistoryFiltersSortsAndPaginatesServerSide(t *testing.T) {
	f := newFixture(t)
	f.seed(t, "010000000000000000000001", "viewer", healthyViewerStats(10), nil)
	f.now = f.now.Add(time.Minute)
	f.seed(t, "010000000000000000000002", "viewer", healthyViewerStats(10), nil)
	f.now = f.now.Add(time.Minute)
	f.seed(t, "010000000000000000000003", "broadcaster", healthyBroadcasterStats(10), nil)

	page, err := f.api.SearchSessions(HistoryQuery{From: f.now.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 3 {
		t.Fatalf("total = %d, want 3", page.Total)
	}
	// Newest first by default.
	if page.Rows[0].SessionID != "010000000000000000000003" {
		t.Errorf("first row = %s, want the newest session", page.Rows[0].SessionID)
	}
	// Role filter is applied by the SERVER: the page never holds the rest.
	only, err := f.api.SearchSessions(HistoryQuery{From: f.now.Add(-time.Hour), Role: "broadcaster"})
	if err != nil {
		t.Fatal(err)
	}
	if len(only.Rows) != 1 || only.Total != 1 {
		t.Fatalf("role filter returned %d rows (total %d), want 1", len(only.Rows), only.Total)
	}

	// Paging: a limit of 2 hands back a cursor, and the cursor continues.
	first, err := f.api.SearchSessions(HistoryQuery{From: f.now.Add(-time.Hour), Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Rows) != 2 || first.NextCursor == "" {
		t.Fatalf("page 1 = %d rows, cursor %q", len(first.Rows), first.NextCursor)
	}
	second, err := f.api.SearchSessions(HistoryQuery{From: f.now.Add(-time.Hour), Limit: 2, Cursor: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Rows) != 1 || second.NextCursor != "" {
		t.Fatalf("page 2 = %d rows, cursor %q; want the tail", len(second.Rows), second.NextCursor)
	}
	if second.Rows[0].SessionID == first.Rows[0].SessionID {
		t.Error("the second page repeats the first")
	}
}

// UD10's sharpest edge: "there is nothing here" and "I cannot answer that" must
// not read the same. A range before the oldest stored partition is the second.
func TestARangeWithNoDataReadsDifferentlyFromAnUnanswerableOne(t *testing.T) {
	f := newFixture(t)
	f.seed(t, "020000000000000000000001", "viewer", healthyViewerStats(10), nil)

	// Inside what we hold, but no rows: empty and fully covered.
	empty, err := f.api.SearchSessions(HistoryQuery{
		From: f.now.Add(2 * time.Hour), To: f.now.Add(3 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Rows) != 0 {
		t.Fatalf("expected no rows, got %d", len(empty.Rows))
	}

	// Before anything was ever stored: also no rows, and it SAYS so.
	unanswerable, err := f.api.SearchSessions(HistoryQuery{From: f.now.AddDate(-1, 0, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if unanswerable.Coverage.Note == "" {
		t.Error("a range reaching before the oldest stored partition carries no note; it reads as 'nothing happened'")
	}
	if empty.Coverage.Note == unanswerable.Coverage.Note {
		t.Error("an empty range and an unanswerable one produce the same coverage statement")
	}
}

func TestRetentionBoundaryMarksRowsRollupOnly(t *testing.T) {
	f := newFixture(t)
	api, err := New(Options{
		Store: f.store, Now: func() time.Time { return f.now }, RetentionDays: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.now = f.now.AddDate(0, 0, -60)
	f.seed(t, "030000000000000000000001", "viewer", healthyViewerStats(10), nil)
	f.now = f.now.AddDate(0, 0, 60)

	page, err := api.SearchSessions(HistoryQuery{From: f.now.AddDate(0, 0, -90)})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Rows) == 0 {
		t.Fatal("the permanent rollup row vanished with its raw window")
	}
	if !page.Rows[0].RollupOnly {
		t.Error("a row from before the retention boundary is not marked rollup-only")
	}
	if page.Coverage.RawFromMs == 0 {
		t.Error("no raw-retention boundary reported")
	}
}

// --- TH4: broadcast timeline -----------------------------------------------

func TestBroadcastTimelinePutsEveryParticipantOnOneAxis(t *testing.T) {
	f := newFixture(t)
	f.seed(t, "040000000000000000000001", "broadcaster", healthyBroadcasterStats(30), nil)
	for _, sid := range []string{
		"040000000000000000000002", "040000000000000000000003", "040000000000000000000004",
	} {
		f.seed(t, sid, "viewer", healthyViewerStats(30), nil)
	}

	d, err := f.api.GetBroadcast(bkey, BroadcastQuery{Since: f.now.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Lanes) < 4 {
		t.Fatalf("lanes = %d, want a broadcaster + 3 viewers (+ relay)", len(d.Lanes))
	}
	if d.Lanes[0].Kind != "broadcaster" {
		t.Errorf("first lane is %q; the broadcaster must be on top, since every other lane depends on it", d.Lanes[0].Kind)
	}
	for _, lane := range d.Lanes {
		if lane.CadenceMs <= 0 {
			t.Errorf("lane %s/%s states no cadence; UD9 forbids drawing a lane at a resolution its source cannot support",
				lane.Kind, lane.SessionID)
		}
		if lane.Kind == "relay" {
			continue
		}
		if lane.StartedAtMs == 0 {
			t.Errorf("lane %s has no span; a join must be a span boundary, not a gap in a line", lane.SessionID)
		}
		if len(lane.Points) == 0 {
			t.Errorf("lane %s has no points", lane.SessionID)
		}
		for _, p := range lane.Points {
			// ABSOLUTE time (UD5). A relative tMs on a shared axis is
			// meaningless — that is the entire point of the lane.
			if p.AtMs < 1_000_000_000_000 {
				t.Fatalf("lane %s point at %d is not an absolute timestamp", lane.SessionID, p.AtMs)
			}
		}
	}
}

// The isolating question, as a fixture pair (TH4's criterion): a broadcast where
// everyone dips and one where a single viewer does must be distinguishable
// without reading a number.
func TestBroadcastWideAndSingleViewerDipsAreDistinguishable(t *testing.T) {
	shared := newFixture(t)
	for _, sid := range []string{"050000000000000000000001", "050000000000000000000002"} {
		shared.seed(t, sid, "viewer", dippingViewerStats(60), nil)
	}
	sd, err := shared.api.GetBroadcast(bkey, BroadcastQuery{Since: shared.now.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	dipping := 0
	for _, lane := range sd.Lanes {
		if len(lane.Dips) > 0 {
			dipping++
		}
	}
	if dipping != 2 {
		t.Fatalf("broadcast-wide fixture: %d of 2 viewer lanes carry dips", dipping)
	}

	lone := newFixture(t)
	lone.seed(t, "060000000000000000000001", "viewer", dippingViewerStats(60), nil)
	lone.seed(t, "060000000000000000000002", "viewer", healthyViewerStats(60), nil)
	ld, err := lone.api.GetBroadcast(bkey, BroadcastQuery{Since: lone.now.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	dipping = 0
	for _, lane := range ld.Lanes {
		if len(lane.Dips) > 0 {
			dipping++
		}
	}
	if dipping != 1 {
		t.Fatalf("single-viewer fixture: %d viewer lanes carry dips, want exactly 1", dipping)
	}
}

// --- TH5: the field catalogue ----------------------------------------------

// UD8's whole reason for being server-owned: adding a field to the Go tables
// must make it appear in the picker with NO UI change. The catalogue is
// derived, so this holds by construction — and the test is what keeps it
// derived.
func TestEveryTypedFieldIsInTheCatalogue(t *testing.T) {
	cat := schema.Catalogue()
	byName := map[string]schema.Field{}
	for _, f := range cat {
		byName[f.Name] = f
	}
	for _, table := range []map[string]schema.Kind{schema.ViewerFields, schema.BroadcasterFields} {
		for name := range table {
			f, ok := byName[name]
			if !ok {
				t.Errorf("field %q is typed but absent from the catalogue", name)
				continue
			}
			if f.Semantic == "" {
				t.Errorf("field %q has no semantic; a chart cannot know how to draw it", name)
			}
			if len(f.Roles) == 0 {
				t.Errorf("field %q names no role", name)
			}
		}
	}
}

// D15's complaint, enforced: the catalogue's counter/gauge split and the
// rollup's own counter lists are two opinions about the same fields, so they
// are pinned together rather than left to drift.
func TestCatalogueAgreesWithTheRollupAboutCounters(t *testing.T) {
	byName := map[string]schema.Field{}
	for _, f := range schema.Catalogue() {
		byName[f.Name] = f
	}
	for _, role := range []string{"viewer", "broadcaster"} {
		for _, name := range rollup.CounterFields(role) {
			f, ok := byName[name]
			if !ok {
				continue // an untyped field is D15 working, not a disagreement
			}
			if f.Semantic != schema.SemCounter {
				t.Errorf("%s is a counter to the rollup and %q to the catalogue; plotting it as a line would mislead",
					name, f.Semantic)
			}
		}
		for _, name := range rollup.SeriesFields(role) {
			f, ok := byName[name]
			if !ok {
				continue
			}
			if f.Semantic == schema.SemCounter {
				t.Errorf("%s is a gauge series to the rollup and a counter to the catalogue", name)
			}
		}
	}
}

// The other half of "no field vanishes": a session carrying a field this build
// has never heard of must still be able to offer it (D15 keeps it on disk;
// this is what keeps it reachable).
func TestUnknownFieldsSurviveIntoTheAvailableList(t *testing.T) {
	f := newFixture(t)
	sid := "070000000000000000000001"
	stats := healthyViewerStats(10)
	for i := range stats {
		stats[i]["fieldFromTheFuture"] = float64(i)
	}
	f.seed(t, sid, "viewer", stats, nil)

	tl, err := f.api.GetSessionDetail(sid, SessionQuery{Detail: true})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, name := range tl.Available {
		if name == "fieldFromTheFuture" {
			found = true
		}
	}
	if !found {
		t.Error("a field this build has no type for is missing from `available`; the explorer would lose it")
	}
}

// --- TH6: verdicts, evidence and the rule catalogue ------------------------

func TestRuleCatalogueComesFromTheGoRules(t *testing.T) {
	cat := rules.Catalogue(rules.Playbook())
	if len(cat) != len(rules.Playbook()) {
		t.Fatalf("catalogue has %d entries for %d rules", len(cat), len(rules.Playbook()))
	}
	for _, d := range cat {
		if d.Verdict == "" {
			t.Errorf("rule %q has no verdict", d.ID)
		}
		if d.Why == "" {
			t.Errorf("rule %q has no explanation; the catalogue is transparency or it is nothing", d.ID)
		}
		// D7's cap, stated rather than left to be discovered.
		if !d.ClientOnly {
			continue
		}
		for _, p := range d.Provenance {
			if p == "relay" {
				t.Errorf("rule %q is marked client-only but requires a relay signal", d.ID)
			}
		}
		if d.MaxConfidence >= 1 {
			t.Errorf("rule %q is client-only but claims a max confidence of %v", d.ID, d.MaxConfidence)
		}
	}
	// The thresholds must be the constants the predicate actually compares
	// against, so a rule that HAS one says so.
	for _, id := range []string{"stall-attribution", "leg-a-broadcaster-uplink", "intermittent-fps-dips"} {
		for _, d := range cat {
			if d.ID == id && len(d.Thresholds) == 0 {
				t.Errorf("rule %q compares against constants but publishes none", id)
			}
		}
	}
}

// A trace that explained a verdict the engine did not reach would be worse than
// no trace. One loop, one ranking, and this is what says so.
func TestTraceAgreesWithTheReportItExplains(t *testing.T) {
	f := newFixture(t)
	sid := "080000000000000000000001"
	f.seed(t, sid, "viewer", dippingViewerStats(60), nil)

	plain, err := f.api.Diagnose(sid)
	if err != nil {
		t.Fatal(err)
	}
	traced, err := f.api.DiagnoseTrace(sid)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(plain)
	b, _ := json.Marshal(&traced.Report)
	if string(a) != string(b) {
		t.Fatalf("traced report differs from the plain one:\n plain: %s\n trace: %s", a, b)
	}
	// Every rule is accounted for — a rule missing from the trace is a rule
	// whose silence is unexplained.
	if len(traced.Trace) != len(rules.Playbook()) {
		t.Errorf("trace covers %d of %d rules", len(traced.Trace), len(rules.Playbook()))
	}
	explained := 0
	for _, tr := range traced.Trace {
		if tr.Outcome == "passed" && len(tr.Read) > 0 {
			explained++
		}
		if tr.Outcome == "unavailable" && len(tr.Missing) == 0 {
			t.Errorf("rule %q is unavailable and names no missing signal", tr.ID)
		}
	}
	if explained == 0 {
		t.Error("no passing rule reports the numbers it read; a non-firing rule is unexplained")
	}
}

// --- TH7: fleet timeline and trends ----------------------------------------

func TestTrendsBucketServerSideAndFlagThinBuckets(t *testing.T) {
	f := newFixture(t)
	for i, sid := range []string{
		"090000000000000000000001", "090000000000000000000002", "090000000000000000000003",
	} {
		f.now = f.now.Add(time.Duration(i) * time.Minute)
		f.seed(t, sid, "viewer", healthyViewerStats(10), nil)
	}
	tr, err := f.api.Trends(TrendQuery{
		From: f.now.Add(-2 * time.Hour), Metric: "receivedFps", GroupBy: "appVersion",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(tr.Series) == 0 {
		t.Fatal("no series")
	}
	if tr.Series[0].Group != "0.33.2" {
		t.Errorf("group = %q, want the appVersion", tr.Series[0].Group)
	}
	for _, p := range tr.Series[0].Points {
		// Three sessions is below the floor FleetSummary already refuses to
		// over-claim at, and the honesty carries over rather than being
		// re-decided per view.
		if p.Sessions < ThinSampleFloor && !p.Thin {
			t.Errorf("a bucket of %d sessions is not flagged thin", p.Sessions)
		}
	}
}

func TestCohortStatesItsSampleSize(t *testing.T) {
	f := newFixture(t)
	f.seed(t, "0a0000000000000000000001", "viewer", healthyViewerStats(10), nil)

	c, err := f.api.Cohorts(CohortQuery{
		AFrom: f.now.Add(-time.Hour), ATo: f.now.Add(time.Hour),
		BFrom: f.now.Add(2 * time.Hour), BTo: f.now.Add(3 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Note == "" {
		t.Error("a cohort with one arm empty makes no statement about it")
	}
	if c.B.Sessions != 0 {
		t.Errorf("arm B has %d sessions; the fixture puts none there", c.B.Sessions)
	}
}

func TestFleetTimelineCarriesOneRowPerBroadcast(t *testing.T) {
	f := newFixture(t)
	f.seed(t, "0b0000000000000000000001", "viewer", dippingViewerStats(60), nil)
	f.seed(t, "0b0000000000000000000002", "viewer", healthyViewerStats(60), nil)

	tl, err := f.api.FleetTimelineOf(HistoryQuery{From: f.now.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Rows) != 1 {
		t.Fatalf("rows = %d, want one per broadcast", len(tl.Rows))
	}
	if tl.Rows[0].Sessions != 2 {
		t.Errorf("row covers %d sessions, want 2", tl.Rows[0].Sessions)
	}
	if tl.Rows[0].FromMs == 0 || tl.Rows[0].ToMs == 0 {
		t.Error("a fleet timeline row without a span cannot be drawn on an axis")
	}
}

// --- TH8: annotations ------------------------------------------------------

func TestAnnotationsArePermanentAndNeverTouchASessionFile(t *testing.T) {
	f := newFixture(t)
	sid := "0c0000000000000000000001"
	f.seed(t, sid, "viewer", healthyViewerStats(10), nil)

	notes, err := annotations.New(annotations.Options{
		Root: f.store.Root(), Now: func() time.Time { return f.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionsBefore := snapshotTree(t, filepath.Join(f.store.Root(), "sessions"))

	saved, err := notes.Add(annotations.Annotation{
		SessionID: sid, AtMs: f.now.UnixMilli(), Text: "switched to WiFi here",
	})
	if err != nil {
		t.Fatal(err)
	}
	if saved.ID == "" {
		t.Fatal("no id assigned")
	}
	// The raw partition stays exactly what the client sent.
	if after := snapshotTree(t, filepath.Join(f.store.Root(), "sessions")); after != sessionsBefore {
		t.Error("adding an annotation modified the session tree")
	}

	// It survives the prune that takes the session it describes — which is the
	// normal case and the entire point (UD16).
	if _, err := f.store.Prune(f.now.AddDate(0, 0, 1)); err != nil {
		t.Fatal(err)
	}
	live, err := notes.List(annotations.Query{SessionID: sid})
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 {
		t.Fatalf("annotation count after prune = %d, want 1", len(live))
	}

	// Deleting one deletes nothing else.
	second, err := notes.Add(annotations.Annotation{SessionID: sid, Text: "and back again"})
	if err != nil {
		t.Fatal(err)
	}
	if err := notes.Delete(saved.ID); err != nil {
		t.Fatal(err)
	}
	live, err = notes.List(annotations.Query{SessionID: sid})
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 || live[0].ID != second.ID {
		t.Fatalf("after deleting one note, %d remain: %+v", len(live), live)
	}
	if err := notes.Delete(saved.ID); err == nil {
		t.Error("deleting an already-deleted note succeeded; 'gone' and 'never there' must differ")
	}
}

func TestAnnotationsAreReturnedForARangeCoveringTheirMoment(t *testing.T) {
	f := newFixture(t)
	notes, err := annotations.New(annotations.Options{
		Root: f.store.Root(), Now: func() time.Time { return f.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	at := f.now.UnixMilli()
	if _, err := notes.Add(annotations.Annotation{AtMs: at, Text: "the R30 regression"}); err != nil {
		t.Fatal(err)
	}
	covering, err := notes.List(annotations.Query{FromMs: at - 60000, ToMs: at + 60000})
	if err != nil {
		t.Fatal(err)
	}
	if len(covering) != 1 {
		t.Errorf("a range covering the note returned %d", len(covering))
	}
	outside, err := notes.List(annotations.Query{FromMs: at + 60000, ToMs: at + 120000})
	if err != nil {
		t.Fatal(err)
	}
	if len(outside) != 0 {
		t.Errorf("a range not covering the note returned %d", len(outside))
	}
}

// --- TH9: dip explainer ----------------------------------------------------

func TestDipExplanationNamesTheCountersThatMoved(t *testing.T) {
	f := newFixture(t)
	sid := "0d0000000000000000000001"
	f.seed(t, sid, "viewer", dippingViewerStats(60), nil)

	rep, err := f.api.ExplainDips(sid, f.now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Episodes) == 0 {
		t.Fatalf("no episodes explained; note = %q", rep.Note)
	}
	ep := rep.Episodes[0]
	if len(ep.Movers) == 0 {
		t.Fatal("an episode with no movers explains nothing")
	}
	named := false
	for _, m := range ep.Movers {
		if m.Signal == "framesDroppedIncomplete" {
			named = true
			if m.To <= m.From {
				t.Errorf("%s reported as a mover but did not move: %v → %v", m.Signal, m.From, m.To)
			}
		}
		if m.Signal == rep.Primary {
			t.Error("the primary rate is listed as evidence of its own dip; that is circular")
		}
	}
	if !named {
		t.Errorf("the counter that actually advanced inside the dip is not named; movers = %+v", ep.Movers)
	}
}

// UD21's honesty rule, and the one worth a dedicated test: with nobody else
// reporting, the output must SAY so rather than implying a correlation.
func TestASingleReportingViewerSaysSoInsteadOfImplyingCorrelation(t *testing.T) {
	f := newFixture(t)
	sid := "0e0000000000000000000001"
	f.seed(t, sid, "viewer", dippingViewerStats(60), nil)

	rep, err := f.api.ExplainDips(sid, f.now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Episodes) == 0 {
		t.Fatal("no episodes")
	}
	c := rep.Episodes[0].Correlation
	if c.PeersReporting != 0 {
		t.Fatalf("fixture has peers: %+v", c.Peers)
	}
	if c.Confidence != 0 {
		t.Errorf("confidence = %v with nobody to correlate against", c.Confidence)
	}
	if !strings.Contains(c.Statement, "no other session") {
		t.Errorf("statement does not say there was nobody to compare with: %q", c.Statement)
	}
	// The wording rule, checked as wording: co-occurrence, never cause.
	for _, banned := range []string{"caused", "because of", "due to"} {
		if strings.Contains(strings.ToLower(c.Statement), banned) {
			t.Errorf("correlation statement makes a causal claim (%q): %q", banned, c.Statement)
		}
	}
}

func TestABroadcastWideDipReportsCoOccurrenceWithConfidence(t *testing.T) {
	f := newFixture(t)
	f.seed(t, "0f0000000000000000000001", "viewer", dippingViewerStats(60), nil)
	f.seed(t, "0f0000000000000000000002", "viewer", dippingViewerStats(60), nil)

	rep, err := f.api.ExplainDips("0f0000000000000000000001", f.now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Episodes) == 0 {
		t.Fatal("no episodes")
	}
	c := rep.Episodes[0].Correlation
	if c.PeersReporting == 0 {
		t.Fatal("the peer session was not seen at all")
	}
	if c.PeersDipped == 0 {
		t.Errorf("the peer dipped in the same window and was not counted: %+v", c.Peers)
	}
	if c.Confidence <= 0 || c.Confidence >= 1 {
		t.Errorf("confidence = %v; co-occurrence is never certainty and never nothing here", c.Confidence)
	}
	if !strings.Contains(c.Statement, "not proof") {
		t.Errorf("statement does not disclaim proof: %q", c.Statement)
	}
}

// --- TH10: the SQL console -------------------------------------------------

// A build with no engine must say so plainly and must not look like a query
// error. `go build ./...` on a fresh clone is exactly this configuration.
func TestQueryConsoleWithoutAnEngineExplainsItself(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(f.api.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/query")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var status QueryStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	if status.Enabled {
		t.Fatal("a build with no engine reports the console as enabled")
	}
	if status.Reason == "" {
		t.Error("no reason given; the console would render a broken editor")
	}
	// It still says what WOULD be queryable: "no engine here" and "nothing to
	// query" are different facts.
	if len(status.Views) == 0 {
		t.Error("no view catalogue offered")
	}

	post, err := http.Post(srv.URL+"/v1/query", "application/json", strings.NewReader(`{"sql":"SELECT 1"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer post.Body.Close()
	if post.StatusCode != http.StatusNotImplemented {
		t.Errorf("POST /v1/query = %d, want 501 (the deployment is missing, not the request)", post.StatusCode)
	}
}

// --- meta ------------------------------------------------------------------

func TestMetaReportsTheBoundariesTheUINeeds(t *testing.T) {
	f := newFixture(t)
	api, err := New(Options{
		Store: f.store, Now: func() time.Time { return f.now }, RetentionDays: 30,
		ScrapeInterval: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	m := api.Meta()
	if m.RetentionDays != 30 || m.RawFromMs == 0 {
		t.Errorf("meta does not state the raw window: %+v", m)
	}
	if m.ServerNowMs == 0 {
		t.Error("meta carries no server clock; a browser cannot detect its own skew")
	}
	if m.ScrapeMs != 5000 {
		t.Errorf("scrapeIntervalMs = %d, want 5000", m.ScrapeMs)
	}
}

// --- SSE -------------------------------------------------------------------

func TestLiveStreamSendsASnapshotAndThenGoesQuiet(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(f.api.Handler())
	defer srv.Close()

	req, err := http.NewRequest(http.MethodGet, srv.URL+"/live/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	resp, err := http.DefaultClient.Do(req.WithContext(ctx))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	// Nginx buffering would hold every event until the buffer filled, turning
	// a live stream into a batched one. The internal Ingress is the deployment.
	if resp.Header.Get("X-Accel-Buffering") != "no" {
		t.Error("proxy buffering is not disabled; events would arrive in batches")
	}
	buf := make([]byte, 4096)
	n, err := resp.Body.Read(buf)
	if err != nil && n == 0 {
		t.Fatalf("no first frame: %v", err)
	}
	first := string(buf[:n])
	if !strings.Contains(first, "retry:") {
		t.Error("no retry hint; a dropped stream would reconnect at the browser's own pace")
	}
	if !strings.Contains(first, "event: snapshot") {
		t.Errorf("first frame is not a snapshot: %q", first)
	}
}

// UD22's entire justification, as a test.
//
// The transport alone would not have earned an endpoint: what earns it is that
// an IDLE fleet costs a heartbeat instead of a full payload — findings and
// metrics for every session, every 2 s, times every open view. That saving
// depends on recognising an unchanged projection, and `Snapshot.AtMs` is a
// fresh clock reading on every call, so hashing the response verbatim would
// make every snapshot look new and quietly buy nothing at all.
func TestStreamHashIgnoresTheSnapshotClock(t *testing.T) {
	base := live.Snapshot{AtMs: 1_000_000, Live: []live.BroadcastView{{
		BroadcastKey: bkey, Lifecycle: "live", Severity: rules.SeverityOK, Viewers: 2,
	}}}
	later := base
	later.AtMs = base.AtMs + 2000

	if streamFingerprint(base) != streamFingerprint(later) {
		t.Error("two snapshots differing only in atMs hash differently; an idle fleet would " +
			"be re-sent every 2 s and UD22 would have bought nothing")
	}

	changed := base
	changed.Live = []live.BroadcastView{{
		BroadcastKey: bkey, Lifecycle: "live", Severity: rules.SeverityBad, Viewers: 2,
	}}
	if streamFingerprint(base) == streamFingerprint(changed) {
		t.Error("a severity change did not change the fingerprint; the stream would sit on it")
	}
}

// --- helpers ---------------------------------------------------------------

func samplesOf(stats []map[string]any) []ingest.Sample {
	out := make([]ingest.Sample, 0, len(stats))
	for i, s := range stats {
		out = append(out, ingest.Sample{TMs: float64(i) * 2000, Stats: s})
	}
	return out
}

func healthyBroadcasterStats(n int) []map[string]any {
	out := make([]map[string]any, 0, n)
	for i := range n {
		out = append(out, map[string]any{
			"captureFps": 60.0, "encoderFps": 60.0, "sentFps": 60.0,
			"encoderQueueDepth": 1.0, "viewerCount": 3.0,
			"encodedFrames": float64(i * 60), "targetFps": 60.0,
		})
	}
	return out
}

// dippingViewerStats is a stream that collapses every so often, with the
// delivery counters advancing inside the collapses — the shape TH9 has to be
// able to explain rather than merely detect.
func dippingViewerStats(n int) []map[string]any {
	out := make([]map[string]any, 0, n)
	incomplete, resyncs, keyframes := 0.0, 0.0, 0.0
	for i := range n {
		fps := 60.0
		// A dip every 20 samples, three samples long, and never at index 0 —
		// the detector correctly refuses to call a warmup a fall.
		if i > 0 && (i%20 == 0 || i%20 == 1 || i%20 == 2) {
			fps = 2.0
			incomplete += 30
			resyncs += 2
			keyframes += 2
		} else {
			keyframes += 0.5
		}
		out = append(out, map[string]any{
			"receivedFps": fps, "decoderFps": fps, "renderedFps": fps,
			"timeSinceLastFrameMs": 16.0, "capToRenderMs": 90.0,
			"framesDroppedIncomplete": incomplete,
			"reorderGapResyncs":       resyncs,
			"keyframeStreamsReceived": keyframes,
			"deliveryMode":            "datagrams", "isHardwareAccelerated": true,
		})
	}
	return out
}

// snapshotTree renders a directory tree's paths and sizes, so a test can assert
// that nothing under it changed.
func snapshotTree(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		b.WriteString(p)
		b.WriteString(":")
		b.WriteString(time.Duration(info.Size()).String())
		b.WriteString("\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}
