package readapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
			SessionID: sessionID, BroadcastKey: bkey, Role: role, Seq: seq, Final: final,
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
