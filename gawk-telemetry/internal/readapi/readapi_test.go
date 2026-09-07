package readapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/ingest"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/rollup"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/rules"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/sessions"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/store"
)

const bkey = "1a2b3c4d5e6f"

type fixture struct {
	api    *API
	store  *store.Store
	writer *sessions.Writer
	now    time.Time
	// roomKey, when set, is stamped on every session seed thereafter (R42).
	roomKey string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{now: time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)}
	st, err := store.New(store.Options{Root: t.TempDir(), Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	f.store = st
	t.Cleanup(func() { _ = st.Close() })

	api, err := New(Options{Store: st, Now: func() time.Time { return f.now }, DashboardBase: "http://dash"})
	if err != nil {
		t.Fatal(err)
	}
	f.api = api
	f.writer = sessions.NewWriter(sessions.Options{
		Store: st, Now: func() time.Time { return f.now },
		// No verdicter: TM3 alone runs without one, and the row then carries no
		// stored verdict — which every reader must tolerate anyway (D4).
		Finalize: sessions.RollupFinalizer(st, nil, nil, func() time.Time { return f.now }),
	})
	return f
}

// seed writes one complete session with the given per-sample stats and events.
func (f *fixture) seed(t *testing.T, sessionID, role string, stats []map[string]any, events []ingest.Event) {
	t.Helper()
	samples := make([]ingest.Sample, 0, len(stats))
	for i, s := range stats {
		samples = append(samples, ingest.Sample{TMs: float64(i) * 2000, Stats: s})
	}
	// Batched to stay under the per-batch structural bound, exactly as a real
	// client would.
	const per = 200
	seq := 0
	for start := 0; start < len(samples) || seq == 0; start += per {
		end := min(start+per, len(samples))
		var chunk []ingest.Sample
		if start < len(samples) {
			chunk = samples[start:end]
		}
		final := end >= len(samples)
		a := ingest.Accepted{
			SessionID: sessionID, BroadcastKey: bkey, RoomKey: f.roomKey, Role: role, Seq: seq, Final: final,
			App:         ingest.AppInfo{Version: "0.33.2", Surface: role, Browser: "Chrome 152", OS: "Windows"},
			StartedAtMs: f.now.UnixMilli(), ReceivedAt: f.now,
			Samples: chunk,
		}
		if final {
			a.Events = events
		}
		if err := f.writer.Accept(a); err != nil {
			t.Fatalf("seed: %v", err)
		}
		seq++
		if final {
			break
		}
	}
}

func healthyViewerStats(n int) []map[string]any {
	out := make([]map[string]any, 0, n)
	for i := range n {
		out = append(out, map[string]any{
			"receivedFps": 60.0, "decoderFps": 60.0, "renderedFps": 60.0,
			"timeSinceLastFrameMs": 16.0, "capToRenderMs": 90.0,
			"decoderQueueDepth": 1.0, "keyframeStreamsReceived": float64(i / 2),
			"reorderGapResyncs": 0.0, "deliveryMode": "datagrams",
			"playoutOffsetMs": 0.0, "isHardwareAccelerated": true,
		})
	}
	return out
}

func TestDiagnoseHealthySessionIsPositiveNotEmpty(t *testing.T) {
	f := newFixture(t)
	sid := "000102030405060708090a0b"
	f.seed(t, sid, "viewer", healthyViewerStats(30), []ingest.Event{{TMs: 1000, Kind: "watching"}})

	rep, err := f.api.Diagnose(sid)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if !rep.Healthy {
		t.Fatalf("healthy session produced findings: %+v", rep.Findings)
	}
	// The owner's §8 decision: a POSITIVE verdict with its basis, so "no
	// issues" is distinguishable from "the analysis never ran".
	if len(rep.Passed) == 0 {
		t.Error("healthy verdict carries no passed checks — its basis is missing")
	}
	if rep.Severity() != rules.SeverityOK {
		t.Errorf("severity = %q, want ok", rep.Severity())
	}
	// A machine must be able to hand a human something to LOOK at.
	if rep.DashboardURL == "" {
		t.Error("no dashboard deep link on the report")
	}
	// And it must say the relay side was not there, rather than resting on it
	// silently (D5/D7).
	if len(rep.Caveats) == 0 {
		t.Error("no caveat about the missing relay-side observation")
	}
}

func TestDiagnoseFiresOnADegradedSession(t *testing.T) {
	f := newFixture(t)
	sid := "0a0b0c0d0e0f101112131415"
	bad := make([]map[string]any, 0, 40)
	for range 40 {
		bad = append(bad, map[string]any{
			"receivedFps": 60.0, "decoderFps": 8.0, // the decoder cannot keep up
			"timeSinceLastFrameMs":  4200.0, // and playback is frozen
			"isHardwareAccelerated": false,
			"deliveryMode":          "datagrams",
		})
	}
	f.seed(t, sid, "viewer", bad, nil)

	rep, err := f.api.Diagnose(sid)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Healthy {
		t.Fatal("a frozen, software-decoding session diagnosed as healthy")
	}
	ids := map[string]bool{}
	for _, fd := range rep.Findings {
		ids[fd.ID] = true
		if len(fd.Evidence) == 0 {
			t.Errorf("finding %q has no evidence", fd.ID)
		}
		for _, e := range fd.Evidence {
			if e.From == "" {
				t.Errorf("finding %q has evidence with no provenance", fd.ID)
			}
		}
	}
	if !ids["decoder-choking"] || !ids["stall-attribution"] {
		t.Errorf("expected decoder-choking and stall-attribution, got %v", ids)
	}
	// Worst first.
	if rep.Findings[0].Severity != rules.SeverityBad {
		t.Errorf("first finding = %q, want bad first", rep.Findings[0].Severity)
	}
}

// D10's structural criterion: EVERY default response stays under the ceiling,
// asserted against a synthetic 4-hour session. This is the failure the whole
// item exists to prevent, so it is a test rather than a habit.
func TestDefaultResponsesStayUnderTheCeiling(t *testing.T) {
	f := newFixture(t)
	sid := "aabbccddeeff001122334455"
	// 4 hours at the 2 s reporting cadence.
	const fourHourSamples = 4 * 60 * 60 / 2
	f.seed(t, sid, "viewer", healthyViewerStats(fourHourSamples), nil)

	measure := func(name string, v any, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("%s: marshal: %v", name, err)
		}
		if len(b) > ResponseCeilingBytes {
			t.Errorf("%s default response is %d bytes, over the %d ceiling", name, len(b), ResponseCeilingBytes)
		}
		t.Logf("%s: %d bytes", name, len(b))
	}

	tl, err := f.api.GetSession(sid, nil, 0)
	measure("get_session", tl, err)
	if !tl.Downsampled {
		t.Error("a 7200-sample session was not downsampled")
	}
	if tl.TotalSample != fourHourSamples {
		t.Errorf("totalSamples = %d, want %d — the caller must know what it is a sample OF", tl.TotalSample, fourHourSamples)
	}

	rep, err := f.api.Diagnose(sid)
	measure("diagnose", rep, err)

	rows, err := f.api.ListSessions(ListSessionsQuery{Since: f.now.Add(-24 * time.Hour)})
	measure("list_sessions", rows, err)

	bl, err := f.api.ListBroadcasts(f.now.Add(-24*time.Hour), 0)
	measure("list_broadcasts", bl, err)

	fs, err := f.api.FleetSummary(f.now.Add(-24*time.Hour), "deliveryMode")
	measure("fleet_summary", fs, err)

	cmp, err := f.api.Compare(sid, f.now.Add(-24*time.Hour))
	measure("compare", cmp, err)
}

// diagnose() returns verdicts and evidence, NEVER the underlying series. An
// MCP surface that dumps 80-field series is worse than today's copy-paste.
func TestDiagnoseNeverReturnsRawSeries(t *testing.T) {
	f := newFixture(t)
	sid := "112233445566778899aabbcc"
	f.seed(t, sid, "viewer", healthyViewerStats(500), nil)

	rep, err := f.api.Diagnose(sid)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(rep)
	var generic map[string]any
	if err := json.Unmarshal(b, &generic); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"samples", "points", "series", "stats"} {
		if _, present := generic[forbidden]; present {
			t.Errorf("diagnose() response carries %q — it must return verdicts, not series", forbidden)
		}
	}
}

func TestGetSessionHonoursExplicitFieldsAndPoints(t *testing.T) {
	f := newFixture(t)
	sid := "ccddeeff00112233445566aa"
	f.seed(t, sid, "viewer", healthyViewerStats(400), nil)

	tl, err := f.api.GetSession(sid, []string{"receivedFps", "capToRenderMs"}, 400)
	if err != nil {
		t.Fatal(err)
	}
	if tl.Downsampled {
		t.Error("an explicit point budget covering the session still downsampled")
	}
	if len(tl.Points) != 400 {
		t.Errorf("points = %d, want 400", len(tl.Points))
	}
	for _, p := range tl.Points {
		if _, ok := p["decoderFps"]; ok {
			t.Error("a field outside the requested set was returned")
		}
	}
}

// Events are never downsampled: they are the story, and there are few of them.
func TestGetSessionKeepsEveryEvent(t *testing.T) {
	f := newFixture(t)
	sid := "ddeeff00112233445566aabb"
	events := make([]ingest.Event, 0, 20)
	for i := range 20 {
		events = append(events, ingest.Event{
			TMs: float64(i) * 1000, Kind: "reconnect", Detail: fmt.Sprintf("attempt %d close 4002", i),
		})
	}
	f.seed(t, sid, "viewer", healthyViewerStats(2000), events)

	tl, err := f.api.GetSession(sid, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !tl.Downsampled {
		t.Error("samples were not downsampled")
	}
	if len(tl.Events) != 20 {
		t.Errorf("events = %d, want all 20 — events are never downsampled", len(tl.Events))
	}
}

func TestListSessionsFiltersAndFlagsDistrust(t *testing.T) {
	f := newFixture(t)
	f.seed(t, "000000000000000000000001", "viewer", healthyViewerStats(10), nil)
	f.seed(t, "000000000000000000000002", "broadcaster", []map[string]any{
		{"captureFps": 60.0, "encoderFps": 60.0, "sentFps": 60.0},
	}, nil)

	all, err := f.api.ListSessions(ListSessionsQuery{Since: f.now.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("sessions = %d, want 2", len(all))
	}
	viewers, err := f.api.ListSessions(ListSessionsQuery{Since: f.now.Add(-time.Hour), Role: "viewer"})
	if err != nil {
		t.Fatal(err)
	}
	if len(viewers) != 1 || viewers[0].Role != "viewer" {
		t.Errorf("role filter returned %+v", viewers)
	}
	// Relay coverage is always stated, never silently assumed.
	for _, s := range all {
		if s.RelayCoverage == "" {
			t.Errorf("session %s has no relayCoverage", s.SessionID)
		}
	}
}

func TestListSessionsRespectsLimit(t *testing.T) {
	f := newFixture(t)
	for i := range 10 {
		f.seed(t, fmt.Sprintf("00000000000000000000000%d", i), "viewer", healthyViewerStats(2), nil)
	}
	rows, err := f.api.ListSessions(ListSessionsQuery{Since: f.now.Add(-time.Hour), Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Errorf("rows = %d, want the limit of 3", len(rows))
	}
}

func TestUnknownSessionIs404(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(f.api.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/sessions/ffffffffffffffffffffffff/diagnose")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// A path-traversal identifier must never reach the filesystem.
func TestBadIdentifierIs400(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(f.api.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/sessions/not-a-session-id/diagnose")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// The HTTP façade and the Go handlers must return the same data — TM7's MCP
// wrapper depends on there being exactly one implementation.
func TestHTTPAndGoHandlersAgree(t *testing.T) {
	f := newFixture(t)
	sid := "aa00bb11cc22dd33ee44ff55"
	f.seed(t, sid, "viewer", healthyViewerStats(20), nil)

	srv := httptest.NewServer(f.api.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/sessions/" + sid + "/diagnose")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var viaHTTP rules.Report
	if err := json.NewDecoder(resp.Body).Decode(&viaHTTP); err != nil {
		t.Fatal(err)
	}
	direct, err := f.api.Diagnose(sid)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(viaHTTP)
	b, _ := json.Marshal(direct)
	if string(a) != string(b) {
		t.Errorf("HTTP and Go paths disagree:\n http: %s\n  go: %s", a, b)
	}
}

func TestFleetSummaryGroupsAndCompares(t *testing.T) {
	f := newFixture(t)
	for i := range 6 {
		f.seed(t, fmt.Sprintf("0000000000000000000000a%d", i), "viewer", healthyViewerStats(10), nil)
	}
	sum, err := f.api.FleetSummary(f.now.Add(-time.Hour), "deliveryMode")
	if err != nil {
		t.Fatal(err)
	}
	if sum.Overall.Sessions != 6 {
		t.Errorf("overall sessions = %d, want 6", sum.Overall.Sessions)
	}
	if sum.Groups["datagrams"] == nil {
		t.Errorf("no datagrams group: %+v", sum.Groups)
	}

	cmp, err := f.api.Compare("0000000000000000000000a0", f.now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if cmp.Class != "datagrams" {
		t.Errorf("class = %q", cmp.Class)
	}
	if _, ok := cmp.Deltas["receivedFps"]; !ok {
		t.Errorf("no fps delta: %+v", cmp.Deltas)
	}
}

// A thin fleet baseline must SAY it is thin rather than let a comparison read
// as meaningful.
func TestCompareFlagsAWeakBaseline(t *testing.T) {
	f := newFixture(t)
	sid := "00ff00ff00ff00ff00ff00ff"
	f.seed(t, sid, "viewer", healthyViewerStats(10), nil)
	cmp, err := f.api.Compare(sid, f.now.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if cmp.Note == "" {
		t.Error("a one-session fleet baseline was presented without a caveat")
	}
}

// TM4's join, which the rollup could not do on its own: relayCoverage stayed
// at its "none" default forever, so every verdict was caveated as client-only
// even when the relay watched the whole session. Found by the e2e pass.
func TestJoinRelayFillsCoverageAndCounters(t *testing.T) {
	const sid = "000102030405060708090a0b"
	obs := func(sessionID string) []byte {
		return []byte(`{"kind":"subscriber","pod":"pod-a","role":"origin",` +
			`"broadcastKey":"` + bkey + `","sessionId":"` + sessionID + `",` +
			`"subscriber":{"dropped":7,"keyframesDropped":1,"carrierQueueOverflow":0,"dvrResyncs":0}}`)
	}
	broadcastObs := []byte(`{"kind":"broadcast","pod":"pod-a","role":"origin",` +
		`"broadcastKey":"` + bkey + `","broadcast":{"publisherActive":true,"framesRelayed":9000,"ingressFramesLost":3}}`)

	base := func(durationMs int64) rollup.Row {
		return rollup.Row{
			SessionID: sid, BroadcastKey: bkey, Role: "viewer",
			StartedAt: 1785715200000, EndedAt: 1785715200000 + durationMs,
			RelayCoverage: "none",
		}
	}

	t.Run("a watched session is joined and covered", func(t *testing.T) {
		row := base(60_000)
		JoinRelay(&row, [][]byte{obs(sid), broadcastObs}, 5*time.Second)
		if row.RelayCoverage != "full" {
			t.Errorf("relayCoverage = %q, want full", row.RelayCoverage)
		}
		if row.RelayPod != "pod-a" || row.RelayRole != "origin" {
			t.Errorf("pod/role = %q/%q", row.RelayPod, row.RelayRole)
		}
		if row.Relay["dropped"] != 7 || row.Relay["ingressFramesLost"] != 3 {
			t.Errorf("joined counters = %v", row.Relay)
		}
	})

	t.Run("a session the relay never saw stays none", func(t *testing.T) {
		row := base(60_000)
		JoinRelay(&row, [][]byte{obs("ffffffffffffffffffffffff")}, 5*time.Second)
		if row.RelayCoverage != "none" {
			t.Errorf("relayCoverage = %q for an unobserved session, want none", row.RelayCoverage)
		}
	})

	t.Run("a session shorter than two intervals is only partial", func(t *testing.T) {
		row := base(4_000)
		JoinRelay(&row, [][]byte{obs(sid)}, 5*time.Second)
		if row.RelayCoverage != "partial" {
			t.Errorf("relayCoverage = %q, want partial — one scrape cannot cover a 4 s session", row.RelayCoverage)
		}
	})
}

// A healthy session with a WARMUP RAMP must not be diagnosed as broken.
//
// Every real session starts with a sample or two before the decoder is
// running, so a funnel rule that compares one stage's median against another's
// p05 fires on every clean stream. That is exactly what happened in the e2e
// pass: a 30 fps loopback stream came back as "decoder choking". Both sides of
// a ratio have to be measured the same way.
func TestWarmupRampDoesNotFakeAFunnelGap(t *testing.T) {
	f := newFixture(t)
	sid := "beefbeefbeefbeefbeefbeef"

	stats := make([]map[string]any, 0, 40)
	// The first two ticks: frames arriving, decoder not yet producing.
	for i := range 2 {
		stats = append(stats, map[string]any{
			"receivedFps": 30.0, "decoderFps": float64(i) * 5,
			"timeSinceLastFrameMs": 33.0, "deliveryMode": "datagrams",
			"keyframeStreamsReceived": 1.0, "reorderGapResyncs": 0.0,
		})
	}
	// Then a long, entirely healthy run.
	for range 38 {
		stats = append(stats, map[string]any{
			"receivedFps": 30.0, "decoderFps": 30.0,
			"timeSinceLastFrameMs": 33.0, "deliveryMode": "datagrams",
			"keyframeStreamsReceived": 10.0, "reorderGapResyncs": 0.0,
		})
	}
	f.seed(t, sid, "viewer", stats, nil)

	rep, err := f.api.Diagnose(sid)
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Healthy {
		t.Errorf("a healthy session with a warmup ramp was diagnosed unhealthy: %+v", rep.Findings)
	}
}

// The converse still fires: a decoder that genuinely cannot keep up for most
// of the session is caught. Fixing the false positive must not buy silence.
func TestSustainedDecoderStarvationStillFires(t *testing.T) {
	f := newFixture(t)
	sid := "feedfeedfeedfeedfeedfeed"
	stats := make([]map[string]any, 0, 40)
	for range 40 {
		stats = append(stats, map[string]any{
			"receivedFps": 30.0, "decoderFps": 6.0,
			"timeSinceLastFrameMs": 33.0, "deliveryMode": "datagrams",
			"keyframeStreamsReceived": 10.0, "reorderGapResyncs": 0.0,
		})
	}
	f.seed(t, sid, "viewer", stats, nil)

	rep, err := f.api.Diagnose(sid)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, fd := range rep.Findings {
		if fd.ID == "decoder-choking" {
			found = true
		}
	}
	if !found {
		t.Errorf("sustained decoder starvation was not caught: %+v", rep.Findings)
	}
}

// --- producer coverage (review finding 5) ----------------------------------

// The read path's half of the two-sided contract in rules.ProducibleFacts. A
// rule may only require what a producer emits; playbook row 10 required
// `relay.framesRelayedPerSec`, which neither path set, so it read `unavailable`
// on every session forever while looking like a working rule.
func TestReadPathEmitsExactlyItsShareOfTheInventory(t *testing.T) {
	f := newFixture(t)
	// BOTH roles, unioned. The inventory spans viewer and broadcaster config,
	// and a single-role fixture can only ever cover half of it — the previous
	// version papered over that by hand-writing a row.Config that mixed keys
	// from both, which is how it claimed coverage it did not have while hiding
	// six emitted-but-unlisted facts.
	emitted := map[string]bool{}
	for _, n := range f.maximalViewerFacts(t).Names() {
		emitted[n] = true
	}
	for _, n := range f.maximalBroadcasterFacts(t).Names() {
		emitted[n] = true
	}

	want := rules.ProducedBy("readapi")
	for n := range emitted {
		if !want[n] {
			t.Errorf("the read path emits %q, which rules.ProducibleFacts does not list", n)
		}
	}
	for n := range want {
		if !emitted[n] {
			t.Errorf("rules.ProducibleFacts claims the read path emits %q, but a maximal session produced no such fact", n)
		}
	}
}

// maximalViewerFacts builds the richest viewer session the read path can see,
// deriving row.Config from the SAMPLES rather than substituting one — the
// assertion above can only be as honest as the row it runs against.
func (f *fixture) maximalViewerFacts(t *testing.T) *rules.Facts {
	t.Helper()
	// Bools are listed by hand: factClientFields cannot carry them, and the
	// emitter reads each one explicitly.
	stats := map[string]any{"isHardwareAccelerated": true, "documentHidden": true,
		"audioBuffer": map[string]any{"overflowDrops": 1.0, "gapsConcealed": 2.0},
		// R29 finding 3: the receive-buffer verdict is nested, like audioBuffer.
		"datagramBuffer": map[string]any{
			"defaultDepth": 1.0, "effective": 256.0, "governsDrops": false, "property": "incomingHighWaterMark"}}
	for _, n := range factClientFields {
		stats[n] = 30.0
	}
	for k, v := range map[string]string{
		"deliveryMode": "datagrams", "playoutMode": "adaptive", "renderer": "webgl",
		"pipelineContext": "worker", "transport": "worker", "audioState": "playing",
		"interpolation": "on", "presentation": "paced-webgl", "avMaster": "video",
	} {
		stats[k] = v
	}
	stats["frameWidth"], stats["frameHeight"] = 1920.0, 1080.0

	// A window with a real dip in it, so the D16 facts are producible at all —
	// a single sample has no baseline to dip from. The maximal sample stays
	// last, because that is the one the per-signal "last observed value" facts
	// are read from.
	samples := make([]rollup.Sample, 0, 13)
	for i, s := range stutteringViewerStats(12) {
		samples = append(samples, rollup.Sample{TMs: float64(i) * 2000, Stats: s})
	}
	samples = append(samples, rollup.Sample{TMs: 24000, Stats: stats})
	in := rollup.Input{
		SessionID: "aa11aa11aa11aa11aa11aa11", BroadcastKey: bkey, Role: "viewer",
		StartedAtMs: 1000, EndedAtMs: 61000,
		Samples: samples,
	}
	row := rollup.Compute(in)
	row.LongestStallMs = 900
	if row.Series == nil {
		row.Series = map[string]*rollup.Stat{}
	}
	for _, n := range []string{"liveEdgeDriftMs", "capToRenderMs", "arrivalJitterMs",
		"avSkewMs", "lastKeyframeAgeMs", "viewerCount", "EncoderFps", "SentFps"} {
		row.Series[n] = &rollup.Stat{Median: 5, P95: 9}
	}
	return f.api.factsFor(row, in, maximalRelayLines(t, in.SessionID))
}

// maximalBroadcasterFacts is the other half: every config key a broadcaster row
// carries, across BOTH producers' spellings (browser and native engine).
func (f *fixture) maximalBroadcasterFacts(t *testing.T) *rules.Facts {
	t.Helper()
	stats := map[string]any{
		"captureFps": 30.0, "encoderFps": 30.0, "sentFps": 30.0, "encoderQueueDepth": 1.0,
		"targetWidth": 1920.0, "targetHeight": 1080.0, "targetFps": 60.0,
		"targetBitrateBps": 8_000_000.0,
		"autoRung":         "1080p", "autoCeiling": "1080p", "pipelineContext": "worker",
		"audioState": "active", "audioCodec": "opus",
		"codec": "avc1.640028", "acceleration": "hardware",
		// The native engine's capitalized spellings, so one row covers both.
		"Encoder": "nvh264enc", "Codec": "avc1.42E02A", "CapturePath": "portal",
	}
	samples := []rollup.Sample{
		{TMs: 0, Stats: stats},
		{TMs: 2000, Stats: stats},
	}
	in := rollup.Input{
		SessionID: "bb22bb22bb22bb22bb22bb22", BroadcastKey: bkey, Role: "broadcaster",
		StartedAtMs: 1000, EndedAtMs: 61000,
		Samples: samples,
	}
	row := rollup.Compute(in)
	return f.api.factsFor(row, in, maximalRelayLines(t, in.SessionID))
}

// Ingest is at-least-once and the rollup store is append-only, so one session
// CAN legitimately produce two rows — a client that resumes after an outage
// longer than the finalize tombstone, or a crash-recovery sweep rolling up a
// session a previous process had already finalized. Duplicate rows double
// every count derived from them, which is what the writer's own comment calls
// corrupting the permanent artifact (review finding 8).
func TestDuplicateRollupRowsCollapseToTheLatest(t *testing.T) {
	f := newFixture(t)
	date := f.now.UTC().Format(store.DateLayout)
	row := func(stalls int) []byte {
		b, err := json.Marshal(rollup.Row{
			SessionID: "aa11aa11aa11aa11aa11aa11", BroadcastKey: bkey, Role: "viewer",
			StartedAt: f.now.UnixMilli(), EndedAt: f.now.UnixMilli() + 60000,
			Samples: 100, Stalls: stalls,
		})
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	// The second write saw more of the session than the first.
	if err := f.store.AppendRollup(date, row(0)); err != nil {
		t.Fatal(err)
	}
	if err := f.store.AppendRollup(date, row(3)); err != nil {
		t.Fatal(err)
	}

	rows, err := f.api.ListSessions(ListSessionsQuery{Since: f.now.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("sessions = %d, want 1 — one session must not appear twice", len(rows))
	}
	if rows[0].Stalls != 3 {
		t.Errorf("stalls = %d, want the later row's 3", rows[0].Stalls)
	}

	// And the broadcast summary counts it once, not twice.
	bs, err := f.api.ListBroadcasts(f.now.Add(-time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(bs) != 1 || bs[0].Sessions != 1 {
		t.Errorf("broadcast summary counted %d sessions, want 1", bs[0].Sessions)
	}
}

// --- D16: dip episodes ------------------------------------------------------

// stutteringViewerStats is the question R28 exists to answer, as data: a viewer
// holding a healthy 30 fps that periodically collapses to the 500 ms GOP
// cadence — 2 fps — because delta loss is eating whole GOPs and only keyframes
// survive. Every dip is 3 samples (~6 s) out of every 30 (~60 s).
//
// The numbers are chosen so that EVERY session-level statistic looks fine:
// the median is 30, and at 2 fps frames arrive every ~500 ms, which never
// crosses the 1000 ms stall threshold. That is not incidental — 2 fps IS the
// GOP cadence, so this failure mode sits under the freeze detector by
// construction (D16).
func stutteringViewerStats(n int) []map[string]any {
	out := make([]map[string]any, 0, n)
	resyncs, keyframes := 0.0, 0.0
	for i := range n {
		dipping := i%30 >= 10 && i%30 < 13
		fps := 30.0
		// Two keyframes per 2 s sample at a 500 ms GOP, always.
		keyframes += 4
		if dipping {
			fps = 2.0
			// The signature: one gap resync per keyframe — every GOP broken.
			resyncs += 4
		}
		out = append(out, map[string]any{
			"receivedFps": fps, "decoderFps": fps, "renderedFps": fps,
			// Well under the stall threshold even at 2 fps.
			"timeSinceLastFrameMs": 500.0, "capToRenderMs": 90.0,
			"decoderQueueDepth": 1.0, "keyframeStreamsReceived": keyframes,
			"reorderGapResyncs": resyncs, "deliveryMode": "datagrams",
			"playoutOffsetMs": 0.0, "isHardwareAccelerated": true,
		})
	}
	return out
}

// The headline TM10 criterion. A stream that stutters visibly must not
// diagnose as healthy.
func TestDiagnoseSeesIntermittentFpsCollapse(t *testing.T) {
	f := newFixture(t)
	sid := "0d0d0d0d0d0d0d0d0d0d0d0d"
	f.seed(t, sid, "viewer", stutteringViewerStats(120), []ingest.Event{{TMs: 1000, Kind: "watching"}})

	rep, err := f.api.Diagnose(sid)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if rep.Healthy {
		t.Fatalf("a stream collapsing to 2 fps every ~20 s diagnosed as HEALTHY — "+
			"passed=%v unavailable=%v", rep.Passed, rep.Unavailable)
	}
	if !hasFinding(rep, "intermittent-fps-dips") {
		t.Errorf("no intermittent-fps-dips finding; got %s", findingIDs(rep))
	}
	// Saying *that* it dipped is half the answer; saying WHY is the half that
	// makes it actionable.
	if !hasFinding(rep, "keyframe-only-delivery") {
		t.Errorf("no keyframe-only-delivery finding; got %s", findingIDs(rep))
	}
}

// The guard against the fix: the funnel ratios must be untouched by the
// presence of dips. An earlier version of the read path compared one side's
// median against the other's p05 and falsely fired `decoder-choking` on a
// clean stream; D16 exists BESIDE that machinery, never inside it.
func TestDipsDoNotDisturbTheFunnelRatios(t *testing.T) {
	f := newFixture(t)
	sid := "0e0e0e0e0e0e0e0e0e0e0e0e"
	f.seed(t, sid, "viewer", stutteringViewerStats(120), nil)

	rep, err := f.api.Diagnose(sid)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	// receivedFps and decoderFps dip together — the decoder is keeping up
	// perfectly with what arrives, so this must NOT read as a decoder problem.
	if hasFinding(rep, "decoder-choking") {
		t.Error("dips made the funnel ratio fire decoder-choking — the medians were contaminated")
	}
}

// A genuinely steady session must stay healthy: a detector that accuses
// everything is worth nothing.
func TestSteadySessionHasNoEpisodes(t *testing.T) {
	f := newFixture(t)
	sid := "0f0f0f0f0f0f0f0f0f0f0f0f"
	f.seed(t, sid, "viewer", healthyViewerStats(120), nil)

	rep, err := f.api.Diagnose(sid)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if hasFinding(rep, "intermittent-fps-dips") {
		t.Error("a steady 60 fps session was accused of dipping")
	}
}

func hasFinding(rep *rules.Report, id string) bool {
	for _, f := range rep.Findings {
		if f.ID == id {
			return true
		}
	}
	return false
}

func findingIDs(rep *rules.Report) string {
	ids := make([]string, 0, len(rep.Findings))
	for _, f := range rep.Findings {
		ids = append(ids, f.ID)
	}
	return fmt.Sprint(ids)
}

// Decimation drops transients silently, which for a stuttering stream means the
// timeline agrees with the median that nothing is wrong — and is just as wrong.
// Envelope-preserving downsampling is what makes the default view usable for
// the question it is most often opened for (D16).
func TestDefaultTimelineNeverDownsamplesADipAway(t *testing.T) {
	f := newFixture(t)
	sid := "1c1c1c1c1c1c1c1c1c1c1c1c"

	// One short dip in a long session — the case decimation loses. At the
	// default 40 points a 900-sample session buckets ~23 samples apiece, so a
	// single dipping sample survives only by luck.
	stats := healthyViewerStats(900)
	stats[500]["receivedFps"] = 2.0
	f.seed(t, sid, "viewer", stats, nil)

	tl, err := f.api.GetSession(sid, nil, 0)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if !tl.Downsampled {
		t.Fatal("900 samples were not downsampled; the test is not exercising the path")
	}
	var lowest float64 = 1e9
	for _, pt := range tl.Points {
		if v, ok := pt["receivedFps"]; ok && v < lowest {
			lowest = v
		}
	}
	if lowest != 2 {
		t.Errorf("lowest receivedFps in the default timeline = %v, want 2 — the dip was decimated away", lowest)
	}
}

// --- D17: the configured target ---------------------------------------------

// The steady-state miss, and the exact complement of the dip tests above. A
// broadcaster that asked for 60 fps and delivered 30 for the WHOLE session has
// a flat baseline (so zero dip episodes) and a perfect funnel (capture 30 →
// encode 30 → sent 30). Before the target was recorded, nothing in the system
// could tell this from a healthy 30 fps stream.
func TestDiagnoseSeesASustainedShortfallAgainstTheTarget(t *testing.T) {
	f := newFixture(t)
	sid := "1717171717171717171717aa"
	stats := make([]map[string]any, 0, 60)
	for range 60 {
		stats = append(stats, map[string]any{
			// Asked for 1080p60 at 8 Mbps.
			"targetWidth": 1920.0, "targetHeight": 1080.0,
			"targetFps": 60.0, "targetBitrateBps": 8_000_000.0,
			"codec": "avc1.640028", "acceleration": "prefer-hardware",
			// Got a flawless 30 — the funnel has no gap anywhere.
			"captureFps": 60.0, "encoderFps": 30.0, "sentFps": 30.0,
			"encoderQueueDepth": 1.0,
		})
	}
	f.seed(t, sid, "broadcaster", stats, nil)

	rep, err := f.api.Diagnose(sid)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if !hasFinding(rep, "delivered-below-target") {
		t.Fatalf("a 60 fps target delivering 30 all session was not reported; got %s (healthy=%v)",
			findingIDs(rep), rep.Healthy)
	}
	// The dip rules cannot and must not claim this: nothing dipped.
	if hasFinding(rep, "intermittent-fps-dips") {
		t.Error("a flat shortfall was reported as intermittent dips")
	}
}

// gawk's capture is damage-driven, so a motionless screen legitimately
// produces far fewer frames than the target. That must be SAID, but it must
// not read as a fault — otherwise every quiet stream on the fleet is accused.
func TestSourceLimitedShortfallIsWordedAsSuchAndIsNotBad(t *testing.T) {
	f := newFixture(t)
	sid := "1717171717171717171717bb"
	stats := make([]map[string]any, 0, 40)
	for range 40 {
		stats = append(stats, map[string]any{
			"targetFps": 60.0,
			// Capture itself is not producing frames — a static screen.
			"captureFps": 4.0, "encoderFps": 4.0, "sentFps": 4.0,
		})
	}
	f.seed(t, sid, "broadcaster", stats, nil)

	rep, err := f.api.Diagnose(sid)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	var found *rules.Finding
	for i := range rep.Findings {
		if rep.Findings[i].ID == "delivered-below-target" {
			found = &rep.Findings[i]
		}
	}
	if found == nil {
		t.Fatalf("shortfall not reported at all; got %s", findingIDs(rep))
	}
	if found.Severity == rules.SeverityBad {
		t.Error("a static screen was reported as a broken stream")
	}
	if !strings.Contains(found.Verdict, "source-limited") {
		t.Errorf("verdict does not name the source as the limit: %q", found.Verdict)
	}
	// The evidence must carry captureFps, or a reader cannot check the claim.
	var hasCapture bool
	for _, e := range found.Evidence {
		if e.Signal == "captureFps" {
			hasCapture = true
		}
	}
	if !hasCapture {
		t.Error("captureFps missing from the evidence — the discriminator is not shown")
	}
}

// A broadcast delivering what it was configured for is healthy, whatever that
// number happens to be. A 30 fps target met at 30 fps is not a shortfall.
func TestMeetingTheTargetIsNotAShortfall(t *testing.T) {
	f := newFixture(t)
	sid := "1717171717171717171717cc"
	stats := make([]map[string]any, 0, 40)
	for range 40 {
		stats = append(stats, map[string]any{
			"targetFps": 30.0, "captureFps": 30.0, "encoderFps": 30.0, "sentFps": 30.0,
		})
	}
	f.seed(t, sid, "broadcaster", stats, nil)

	rep, err := f.api.Diagnose(sid)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if hasFinding(rep, "delivered-below-target") {
		t.Error("a stream meeting its target was reported as falling short of it")
	}
}

// The telemetry E2E (docs/25) asserts diagnose() calls a clean loopback
// session HEALTHY, and its harness settles for "pre-decode zeros" — the
// viewer reports 0 fps until the first frame decodes. A detector reading that
// ramp as a collapse turns every healthy session's first seconds into a
// finding, which is exactly the false-positive class that step exists to
// catch. This pins the shape here, where it is cheap.
func TestWarmupZerosDoNotMakeACleanSessionUnhealthy(t *testing.T) {
	f := newFixture(t)
	sid := "5a5a5a5a5a5a5a5a5a5a5a5a"

	stats := healthyViewerStats(40)
	// The first three samples land before anything has decoded.
	for i := range 3 {
		stats[i]["receivedFps"] = 0.0
		stats[i]["decoderFps"] = 0.0
		stats[i]["renderedFps"] = 0.0
	}
	f.seed(t, sid, "viewer", stats, []ingest.Event{{TMs: 500, Kind: "watching"}})

	rep, err := f.api.Diagnose(sid)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if !rep.Healthy {
		t.Fatalf("a clean session was called unhealthy over its warmup: %+v", rep.Findings)
	}
}

// maximalRelayLines is the relay side of a maximal session: one subscriber
// record and two broadcast observations (two, so framesRelayedPerSec has a
// window to be computed over — the signal review finding 5 found nothing
// produced).
func maximalRelayLines(t *testing.T, sessionID string) [][]byte {
	t.Helper()
	line := func(v any) []byte {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	return [][]byte{
		line(map[string]any{"kind": "subscriber", "atMs": 2000, "pod": "pod-a", "role": "origin",
			"broadcastKey": bkey, "sessionId": sessionID,
			"subscriber": map[string]any{"dropped": 3, "queueDepth": 2, "keyframesDropped": 1,
				"carrierQueueOverflow": 1, "carrierRecordsDropped": 2, "dvrResyncs": 1, "dvrLagMs": 800}}),
		line(map[string]any{"kind": "broadcast", "atMs": 2000, "pod": "pod-a", "role": "origin",
			"broadcastKey": bkey,
			"broadcast": map[string]any{"publisherActive": true, "framesRelayed": 100,
				"ingressFramesLost": 2, "subscribers": 3, "viewersGlobal": 3,
				"datagramsDropped": 4, "bandwidthDroppedDatagrams": 1, "keyframeStreamsIn": 5}}),
		line(map[string]any{"kind": "broadcast", "atMs": 12000, "pod": "pod-a", "role": "origin",
			"broadcastKey": bkey,
			"broadcast": map[string]any{"publisherActive": true, "framesRelayed": 400,
				"ingressFramesLost": 3, "subscribers": 3, "viewersGlobal": 3,
				"datagramsDropped": 6, "bandwidthDroppedDatagrams": 2, "keyframeStreamsIn": 9,
				"subscriberDetails": []map[string]any{
					{"sessionId": sessionID, "dropped": 3},
					{"sessionId": "cc33cc33cc33cc33cc33cc33", "dropped": 1},
					{"key": "edge", "internal": true, "dropped": 0},
				}}}),
	}
}

// R37 (docs/40 D17 / SP12): wildcard CORS lives on the INGEST listener only.
// The read surface stays never-public and must serve no CORS headers at all —
// a browser on a foreign origin gets nothing here, preflight included. This
// pins the split so a future "just add CORS to the mux" change fails loudly.
func TestReadSurfaceServesNoCORS(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(f.api.Handler())
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodOptions, srv.URL+"/v1/sessions", nil)
	req.Header.Set("Origin", "https://any-ui.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("read listener answered CORS (allow-origin %q); it must serve none", got)
	}

	getReq, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/sessions", nil)
	getReq.Header.Set("Origin", "https://any-ui.example")
	resp, err = http.DefaultClient.Do(getReq)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("read listener GET carried allow-origin %q; it must serve none", got)
	}
}
