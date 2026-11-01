package repair

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/relayscrape"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/rollup"
)

const (
	bkey = "1a2b3c4d5e6f"
	sidX = "aaaaaaaaaaaaaaaaaaaaaaaa" // striped: primary + 3 legs
	sidY = "bbbbbbbbbbbbbbbbbbbbbbbb" // unstriped
	sidZ = "cccccccccccccccccccccccc" // striped, already marked (post-#657 shape)
	sidT = "dddddddddddddddddddddddd" // striped, every member tied at zero keyframes
)

func write(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeGz(t *testing.T, path string, data []byte) {
	t.Helper()
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	if _, err := zw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	write(t, path, b.Bytes())
}

func read(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasSuffix(path, ".gz") {
		zr, err := gzip.NewReader(bytes.NewReader(b))
		if err != nil {
			t.Fatalf("%s is no longer gzip: %v", path, err)
		}
		if b, err = io.ReadAll(zr); err != nil {
			t.Fatal(err)
		}
	}
	return b
}

func lines(b []byte) []string {
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func run(t *testing.T, root string) Summary {
	t.Helper()
	s, ran, err := Run(root)
	if err != nil {
		t.Fatal(err)
	}
	if !ran {
		t.Fatal("Run reported already done on a fresh data dir")
	}
	return s
}

// --- rollups ---------------------------------------------------------------

const (
	// A viewer poisoned by #376, striped, joined to a leg's relay figures, and
	// written by a NEWER release: an unknown top-level field and an unknown
	// series key, both of which must survive the repair untouched.
	rowPoisoned = `{"sessionId":"aaaaaaaaaaaaaaaaaaaaaaaa","broadcastKey":"1a2b3c4d5e6f","role":"viewer","relayCoverage":"full",` +
		`"schemaAnomalies":{},"samples":30,"events":0,"seqGaps":0,"endedCleanly":true,"stalls":0,"longestStallMs":0,"totalStallMs":0,"reconnects":0,` +
		`"series":{"capToRenderMs":{"n":30,"min":40,"p05":41,"median":900000,"p95":2880000,"max":2880000},` +
		`"liveEdgeDriftMs":{"n":30,"min":5,"p05":5,"median":12,"p95":20,"max":25},` +
		`"receivedFps":{"n":30,"min":59,"p05":59,"median":60,"p95":60,"max":60},` +
		`"futureSeries":{"n":1,"min":1,"p05":1,"median":1,"p95":1,"max":1}},` +
		`"counters":{"stripeLegDials":4,"framesAssembled":1800},` +
		`"relay":{"dropped":99,"keyframesDropped":3,"carrierQueueOverflow":1,"carrierRecordsDropped":2,"dvrResyncs":0,"ingressFramesLost":5,"framesRelayed":9000},` +
		`"futureField":{"nested":[1,2,{"x":"<&>"}]}}`
	// Healthy and unstriped: must come out byte-identical.
	rowHealthy = `{"sessionId":"bbbbbbbbbbbbbbbbbbbbbbbb","broadcastKey":"1a2b3c4d5e6f","role":"viewer","relayCoverage":"full",` +
		`"series":{"capToRenderMs":{"n":3,"min":40,"p05":40,"median":45,"p95":50,"max":50}},` +
		`"relay":{"dropped":7,"framesRelayed":9000}}`
	// Striped by stripeActive only, and its latency is fine except the drift.
	rowStripeActive = `{"sessionId":"cccccccccccccccccccccccc","broadcastKey":"1a2b3c4d5e6f","role":"viewer","relayCoverage":"full",` +
		`"series":{"stripeActive":{"n":3,"min":0,"p05":0,"median":4,"p95":4,"max":4},` +
		`"liveEdgeDriftMs":{"n":3,"min":10,"p05":10,"median":12,"p95":40000,"max":40000}},` +
		`"relay":{"dropped":12,"ingressFramesLost":1},"repairs":["some-earlier-repair"]}`
	// Striped, but the relay never saw it: nothing to remove, nothing to record.
	rowStripedNoRelay = `{"sessionId":"dddddddddddddddddddddddd","broadcastKey":"1a2b3c4d5e6f","role":"viewer","relayCoverage":"none",` +
		`"counters":{"stripeLegDials":2}}`
	// A broadcaster's relay map is broadcast-derived: never touched.
	rowBroadcaster = `{"sessionId":"eeeeeeeeeeeeeeeeeeeeeeee","broadcastKey":"1a2b3c4d5e6f","role":"broadcaster","relayCoverage":"full",` +
		`"counters":{"stripeLegDials":2},"relay":{"dropped":1,"ingressFramesLost":1}}`
	rowGarbage = `{"sessionId":"ffff` // truncated by a crash mid-write
)

func TestRollupRepair(t *testing.T) {
	root := t.TempDir()
	day := filepath.Join(root, "rollups", "2026-09-20.ndjson")
	write(t, day, []byte(strings.Join([]string{
		rowPoisoned, rowHealthy, rowStripeActive, rowStripedNoRelay, rowBroadcaster, "not json{",
	}, "\n")+"\n"+rowGarbage))
	clean := filepath.Join(root, "rollups", "2026-09-21.ndjson")
	write(t, clean, []byte(rowHealthy+"\n"))
	cleanInfo, _ := os.Stat(clean)

	s := run(t, root)

	got := lines(read(t, day))
	if len(got) != 7 {
		t.Fatalf("got %d lines, want 7 (every line kept)", len(got))
	}
	for i, want := range map[int]string{1: rowHealthy, 3: rowStripedNoRelay, 4: rowBroadcaster, 5: "not json{", 6: rowGarbage} {
		if got[i] != want {
			t.Errorf("line %d changed:\n got %s\nwant %s", i, got[i], want)
		}
	}

	// Poisoned row.
	var p rollup.Row
	if err := json.Unmarshal([]byte(got[0]), &p); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Series["capToRenderMs"]; ok {
		t.Error("poisoned capToRenderMs survived")
	}
	if p.Series["liveEdgeDriftMs"] == nil || p.Series["receivedFps"] == nil {
		t.Error("a plausible series was removed")
	}
	for _, k := range subscriberRelayKeys {
		if _, ok := p.Relay[k]; ok {
			t.Errorf("relay.%s survived on a striped row", k)
		}
	}
	if p.Relay["ingressFramesLost"] != 5 || p.Relay["framesRelayed"] != 9000 {
		t.Errorf("broadcast-derived relay keys lost: %v", p.Relay)
	}
	if strings.Join(p.Repairs, ",") != LatencyRepair+","+StripeLegRelayRepair {
		t.Errorf("repairs = %v", p.Repairs)
	}
	// Fields the current struct does not know survive verbatim in meaning.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(got[0]), &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["futureField"]) != `{"nested":[1,2,{"x":"<&>"}]}` {
		t.Errorf("futureField = %s", raw["futureField"])
	}
	var series map[string]json.RawMessage
	_ = json.Unmarshal(raw["series"], &series)
	if string(series["futureSeries"]) != `{"n":1,"min":1,"p05":1,"median":1,"p95":1,"max":1}` {
		t.Errorf("futureSeries = %s", series["futureSeries"])
	}
	if p.Counters["framesAssembled"] != 1800 || p.Samples != 30 || !p.EndedCleanly {
		t.Errorf("unrelated fields changed: %+v", p)
	}

	// stripeActive row: drift removed, relay dropped removed, earlier repair kept.
	var c rollup.Row
	if err := json.Unmarshal([]byte(got[2]), &c); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Series["liveEdgeDriftMs"]; ok || c.Series["stripeActive"] == nil {
		t.Errorf("series = %v", c.Series)
	}
	if _, ok := c.Relay["dropped"]; ok || c.Relay["ingressFramesLost"] != 1 {
		t.Errorf("relay = %v", c.Relay)
	}
	if strings.Join(c.Repairs, ",") != "some-earlier-repair,"+LatencyRepair+","+StripeLegRelayRepair {
		t.Errorf("repairs = %v", c.Repairs)
	}

	want := RollupSummary{FilesRewritten: 1, RowsChanged: 2, LatencySeriesRemoved: 2,
		StripeLegRelayRows: 2, RelayKeysRemoved: 6, UnparsedLinesKept: 2}
	if s.Rollups != want {
		t.Errorf("summary = %+v, want %+v", s.Rollups, want)
	}

	// An unchanged file is not rewritten at all.
	if info, _ := os.Stat(clean); !info.ModTime().Equal(cleanInfo.ModTime()) || !os.SameFile(info, cleanInfo) {
		t.Error("an unchanged rollup file was rewritten")
	}
}

// --- relay -------------------------------------------------------------------

// sub is one stored subscriber observation line plus its subscriberDetails
// element (the same object, as the scraper writes both).
type sub struct {
	pod, key, sid string
	atMs          int64
	keyframes     int
	extra         string // e.g. `,"stripeLeg":true`
}

func (s sub) detail() string {
	return fmt.Sprintf(`{"key":%q,"sessionId":%q,"dropped":3,"keyframesSent":%d%s}`, s.key, s.sid, s.keyframes, s.extra)
}

func (s sub) line() string {
	return fmt.Sprintf(`{"kind":"subscriber","atMs":%d,"pod":%q,"role":"origin","broadcastKey":%q,"sessionId":%q,"subscriber":%s}`,
		s.atMs, s.pod, bkey, s.sid, s.detail())
}

func bcLine(pod string, atMs int64, subs ...sub) string {
	d := make([]string, 0, len(subs))
	for _, s := range subs {
		d = append(d, s.detail())
	}
	return fmt.Sprintf(`{"kind":"broadcast","atMs":%d,"pod":%q,"role":"origin","broadcastKey":%q,`+
		`"broadcast":{"publisherActive":true,"framesRelayed":9000,"subscriberDetails":[%s],"futureBroadcastField":7},"futureTop":"x"}`,
		atMs, pod, bkey, strings.Join(d, ","))
}

// podFile builds one pod's file for a round: the broadcast line, then one
// subscriber line per AUDIENCE-looking entry, exactly as the pre-#657
// scraper stored them (legs included, edges not).
func podFile(pod string, atMs int64, subs ...sub) []string {
	out := []string{bcLine(pod, atMs, subs...)}
	for _, s := range subs {
		if strings.Contains(s.extra, `"internal":true`) || strings.Contains(s.extra, `"stripeLeg":true`) {
			continue
		}
		out = append(out, s.line())
	}
	return out
}

func audienceCounts(t *testing.T, dir string) (lineCount, detailCount map[string]int) {
	t.Helper()
	lineCount, detailCount = map[string]int{}, map[string]int{}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		for _, ln := range lines(read(t, filepath.Join(dir, e.Name()))) {
			var o relayscrape.Observation
			if json.Unmarshal([]byte(ln), &o) != nil {
				continue
			}
			if o.Subscriber != nil && o.Subscriber.Audience() {
				lineCount[fmt.Sprintf("%d/%s", o.AtMs, o.SessionID)]++
			}
			if o.Broadcast != nil {
				for _, d := range o.Broadcast.SubscriberDetails {
					if d.Audience() && d.SessionID != "" {
						detailCount[fmt.Sprintf("%d/%s", o.AtMs, d.SessionID)]++
					}
				}
			}
		}
	}
	return lineCount, detailCount
}

func TestRelayRepair(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "relay", "date=2026-09-20")
	const at1, at2 = 1790000000000, 1790000005000

	// Round 1. X striped: primary on pod-a (keyframes 40), a leg on pod-a
	// listed FIRST, two legs on pod-b — legs land on any pod behind the LB.
	// Y is an ordinary viewer. An edge session (internal, no sessionId).
	// Z: post-#657 shape — its leg is already marked, so Z is left alone.
	// T: striped, but no member has a keyframe yet — one must still stay.
	primX := sub{pod: "pod-a", key: "a1", sid: sidX, atMs: at1, keyframes: 40}
	legXa := sub{pod: "pod-a", key: "a0", sid: sidX, atMs: at1}
	legXb1 := sub{pod: "pod-b", key: "b1", sid: sidX, atMs: at1}
	legXb2 := sub{pod: "pod-b", key: "b2", sid: sidX, atMs: at1}
	y := sub{pod: "pod-a", key: "a2", sid: sidY, atMs: at1, keyframes: 12}
	edge := sub{pod: "pod-a", key: "a3", atMs: at1, extra: `,"internal":true`}
	z := sub{pod: "pod-b", key: "b3", sid: sidZ, atMs: at1, keyframes: 5}
	zLeg := sub{pod: "pod-b", key: "b4", sid: sidZ, atMs: at1, extra: `,"stripeLeg":true`}
	t1 := sub{pod: "pod-a", key: "a4", sid: sidT, atMs: at1}
	t2 := sub{pod: "pod-b", key: "b5", sid: sidT, atMs: at1}
	// Round 2: X's primary alone on pod-a. Not a duplicate within ITS round,
	// even though the same session had legs in round 1.
	primX2 := sub{pod: "pod-a", key: "a1", sid: sidX, atMs: at2, keyframes: 44}

	podA := append(podFile("pod-a", at1, legXa, primX, y, edge, t1), podFile("pod-a", at2, primX2)...)
	podB := podFile("pod-b", at1, legXb1, legXb2, z, zLeg, t2)
	write(t, filepath.Join(dir, "pod-a.ndjson"), []byte(strings.Join(podA, "\n")+"\n"))
	writeGz(t, filepath.Join(dir, "pod-b.ndjson.gz"), []byte(strings.Join(podB, "\n")+"\n"))
	// Another date with nothing to fix.
	quiet := filepath.Join(root, "relay", "date=2026-09-21", "pod-a.ndjson")
	write(t, quiet, []byte(strings.Join(podFile("pod-a", at2+86_400_000, y), "\n")+"\n"))
	quietBefore := read(t, quiet)

	before, _ := audienceCounts(t, dir)
	if before[fmt.Sprintf("%d/%s", at1, sidX)] != 4 {
		t.Fatalf("fixture: X has %d audience lines before the repair, want 4", before[fmt.Sprintf("%d/%s", at1, sidX)])
	}

	s := run(t, root)

	lc, dc := audienceCounts(t, dir)
	for _, k := range []string{
		fmt.Sprintf("%d/%s", at1, sidX), fmt.Sprintf("%d/%s", at1, sidY), fmt.Sprintf("%d/%s", at1, sidZ),
		fmt.Sprintf("%d/%s", at1, sidT), fmt.Sprintf("%d/%s", at2, sidX),
	} {
		if lc[k] != 1 {
			t.Errorf("subscriber lines for %s: %d audience, want 1", k, lc[k])
		}
		if dc[k] != 1 {
			t.Errorf("subscriberDetails for %s: %d audience, want 1", k, dc[k])
		}
	}

	// The one that stays is the primary: legs never receive keyframes
	// (docs/35 §5.1), so the entry that did is the viewer.
	for _, ln := range lines(read(t, filepath.Join(dir, "pod-a.ndjson"))) {
		var o relayscrape.Observation
		_ = json.Unmarshal([]byte(ln), &o)
		if o.Subscriber != nil && o.SessionID == sidX && o.AtMs == at1 {
			if wantLeg := o.Subscriber.Key != "a1"; o.Subscriber.StripeLeg != wantLeg {
				t.Errorf("X entry %s: stripeLeg=%v, want %v", o.Subscriber.Key, o.Subscriber.StripeLeg, wantLeg)
			}
		}
		// Unknown fields survive.
		if o.Kind == "broadcast" && !strings.Contains(ln, `"futureBroadcastField":7`) || o.Kind == "broadcast" && !strings.Contains(ln, `"futureTop":"x"`) {
			t.Errorf("unknown field lost: %s", ln)
		}
	}

	if !bytes.Equal(read(t, quiet), quietBefore) {
		t.Error("a relay file with nothing to fix was changed")
	}
	want := RelaySummary{DatesScanned: 2, FilesRewritten: 2, SubscriberLinesMarked: 4, SubscriberDetailsMarked: 4}
	if s.Relay != want {
		t.Errorf("summary = %+v, want %+v", s.Relay, want)
	}
}

// --- run-once, crash safety ------------------------------------------------

func TestRunOnceAndIdempotent(t *testing.T) {
	root := t.TempDir()
	day := filepath.Join(root, "rollups", "2026-09-20.ndjson")
	write(t, day, []byte(rowPoisoned+"\n"+rowHealthy+"\n"))
	relayFile := filepath.Join(root, "relay", "date=2026-09-20", "pod-a.ndjson")
	write(t, relayFile, []byte(strings.Join(podFile("pod-a", 1, sub{pod: "pod-a", key: "p", sid: sidX, atMs: 1, keyframes: 1},
		sub{pod: "pod-a", key: "l", sid: sidX, atMs: 1}), "\n")+"\n"))
	// Residue of a crash mid-rewrite: readers list every file in a relay
	// partition, so a leftover temp file would be read as a second pod.
	stale := filepath.Join(root, "relay", "date=2026-09-20", ".pod-a.ndjson"+tmpSuffix)
	write(t, stale, []byte("partial"))

	first := run(t, root)
	if first.Rollups.RowsChanged != 1 || first.Relay.SubscriberLinesMarked != 1 {
		t.Fatalf("first run did nothing: %+v", first)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("a stale temp file survived")
	}
	marker, err := os.ReadFile(MarkerPath(root))
	if err != nil {
		t.Fatalf("no marker: %v", err)
	}
	var recorded Summary
	if err := json.Unmarshal(marker, &recorded); err != nil || recorded.ID != ID || recorded.Rollups != first.Rollups || recorded.Relay != first.Relay {
		t.Errorf("marker = %s (err %v), want the run's summary", marker, err)
	}

	// With the marker present, Run does nothing at all.
	afterFirst := read(t, day)
	if _, ran, err := Run(root); err != nil || ran {
		t.Errorf("second Run: ran=%v err=%v, want skipped", ran, err)
	}

	// An operator re-run after a partial attempt (both markers removed)
	// covers everything again: the repaired data must be a fixed point.
	for _, p := range []string{MarkerPath(root), StartedPath(root)} {
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	relayAfterFirst := read(t, relayFile)
	again := run(t, root)
	if again.Rollups.RowsChanged != 0 || again.Rollups.FilesRewritten != 0 ||
		again.Relay.SubscriberLinesMarked != 0 || again.Relay.SubscriberDetailsMarked != 0 || again.Relay.FilesRewritten != 0 {
		t.Errorf("re-run changed data: %+v", again)
	}
	if !bytes.Equal(read(t, day), afterFirst) || !bytes.Equal(read(t, relayFile), relayAfterFirst) {
		t.Error("re-run rewrote repaired files")
	}
}

// A relay file no reader can read whole (a .gz cut short by a crash) must not
// block the marker forever, and must never be rewritten from its readable
// prefix — that would drop its tail.
func TestUnreadableRelayFileIsSkippedNotFatal(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "relay", "date=2026-09-20")
	body := strings.Join(podFile("pod-b", 1, sub{pod: "pod-b", key: "p", sid: sidX, atMs: 1, keyframes: 1},
		sub{pod: "pod-b", key: "l", sid: sidX, atMs: 1}), "\n") + "\n"
	var b bytes.Buffer
	zw := gzip.NewWriter(&b)
	_, _ = zw.Write([]byte(body))
	_ = zw.Close()
	cut := b.Bytes()[:b.Len()-12] // no gzip trailer
	write(t, filepath.Join(dir, "pod-b.ndjson.gz"), cut)

	s := run(t, root)
	if s.Relay.UnreadableFilesSkipped != 1 || s.Relay.FilesRewritten != 0 {
		t.Errorf("summary = %+v, want the file skipped and nothing rewritten", s.Relay)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "pod-b.ndjson.gz")); !bytes.Equal(got, cut) {
		t.Error("an unreadable file was rewritten")
	}
	if _, err := os.Stat(MarkerPath(root)); err != nil {
		t.Errorf("an unreadable relay file blocked the marker: %v", err)
	}
}

// One attempt, ever. A boot that finds the in-progress marker without the
// done marker knows a previous attempt died (OOM, liveness kill) or failed,
// and must not try again: that is the crash loop. It changes nothing and
// says so with ErrPreviousAttempt.
func TestAPreviousAttemptIsNeverRetried(t *testing.T) {
	root := t.TempDir()
	day := filepath.Join(root, "rollups", "2026-09-20.ndjson")
	write(t, day, []byte(rowPoisoned+"\n"))
	write(t, StartedPath(root), []byte("{}\n"))

	_, ran, err := Run(root)
	if ran || !errors.Is(err, ErrPreviousAttempt) {
		t.Fatalf("Run with a stale in-progress marker: ran=%v err=%v, want ErrPreviousAttempt", ran, err)
	}
	if string(read(t, day)) != rowPoisoned+"\n" {
		t.Error("an abandoned repair ran again")
	}
	if _, err := os.Stat(MarkerPath(root)); !os.IsNotExist(err) {
		t.Error("an abandoned repair was marked done")
	}

	// Deleting the in-progress marker is how an operator asks for a re-run.
	if err := os.Remove(StartedPath(root)); err != nil {
		t.Fatal(err)
	}
	if s := run(t, root); s.Rollups.RowsChanged != 1 {
		t.Errorf("re-run after deleting the marker: %+v", s.Rollups)
	}
	if _, err := os.Stat(StartedPath(root)); err != nil {
		t.Errorf("in-progress marker gone after a completed run: %v (it records the attempt)", err)
	}
}

// The in-progress marker is on disk BEFORE any file is touched, so a death
// at any point after the first rewrite is recognised at the next boot.
func TestInProgressMarkerPrecedesTheFirstRewrite(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "rollups", "2026-09-20.ndjson"), []byte(rowPoisoned+"\n"))
	sawMarker, rewrites := true, 0
	testHookBeforeRewrite = func(path string) {
		if path == StartedPath(root) || path == MarkerPath(root) {
			return
		}
		rewrites++
		if _, err := os.Stat(StartedPath(root)); err != nil {
			sawMarker = false
		}
	}
	t.Cleanup(func() { testHookBeforeRewrite = nil })
	run(t, root)
	if rewrites == 0 {
		t.Fatal("nothing was rewritten; the test proves nothing")
	}
	if !sawMarker {
		t.Error("a file was rewritten before the in-progress marker existed")
	}
}

// An empty or brand-new data dir (no rollups/ or relay/ yet) completes and is
// marked, so a fresh install never runs it again.
func TestRunOnEmptyDataDir(t *testing.T) {
	root := t.TempDir()
	s := run(t, root)
	if s.Rollups != (RollupSummary{}) || s.Relay != (RelaySummary{}) {
		t.Errorf("summary = %+v", s)
	}
	if _, err := os.Stat(MarkerPath(root)); err != nil {
		t.Errorf("no marker: %v", err)
	}
}
