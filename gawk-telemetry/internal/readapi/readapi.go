// Package readapi is R28's query surface (docs/33 TM6 + §4.6).
//
// One implementation, two façades: the MCP server (TM7) is a thin wrapper over
// these same handlers, so the two cannot drift.
//
// **Context discipline is an API property, not a habit (D10).** The failure
// this item exists to prevent is reachable by building it carelessly: 80
// fields × 200 samples of JSON is context incineration. So every tool has a
// bounded default response, `diagnose()` returns verdicts and evidence and
// NEVER the underlying series, and a documented 32 KB soft ceiling on any
// default response is asserted against a synthetic 4-hour session.
//
// The read side is never routed publicly (D14/D1): only ingest is. This
// aggregates every broadcast on the fleet and should be no more reachable than
// /statusz is today.
package readapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/live"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/relayscrape"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/rollup"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/rules"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/schema"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/sessions"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/store"
)

// ResponseCeilingBytes is the documented soft ceiling on any DEFAULT response
// (D10). Not a hard truncation of arbitrary output — a bound the default
// projections are designed to sit inside, and which a test asserts against a
// synthetic 4-hour session.
const ResponseCeilingBytes = 32 << 10

// DefaultTimelinePoints is how many points get_session returns without an
// explicit window: enough to see a session's shape, few enough to read.
const DefaultTimelinePoints = 40

// DefaultRowLimit bounds the list endpoints.
const DefaultRowLimit = 50

// API serves the read handlers.
type API struct {
	store     *store.Store
	live      *live.Projection
	now       func() time.Time
	dashboard string
	rs        []rules.Rule
}

// Options configure the API.
type Options struct {
	Store *store.Store
	Live  *live.Projection
	Now   func() time.Time
	// DashboardBase is prefixed to the deep links a verdict carries, so a
	// machine can hand a human something to LOOK at rather than a wall of
	// numbers.
	DashboardBase string
}

// New builds the read API.
func New(opts Options) (*API, error) {
	if opts.Store == nil {
		return nil, errors.New("readapi: Store is required")
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &API{
		store: opts.Store, live: opts.Live, now: opts.Now,
		dashboard: opts.DashboardBase, rs: rules.Playbook(),
	}, nil
}

// --- list_broadcasts ------------------------------------------------------

// BroadcastSummary is one row of list_broadcasts.
type BroadcastSummary struct {
	BroadcastKey string         `json:"broadcastKey"`
	Sessions     int            `json:"sessions"`
	Viewers      int            `json:"viewers"`
	FirstSeenMs  int64          `json:"firstSeenMs"`
	LastSeenMs   int64          `json:"lastSeenMs"`
	WorstVerdict rules.Severity `json:"worstVerdict"`
	Live         bool           `json:"live"`
}

// ListBroadcasts summarizes broadcasts since a time, worst-first.
func (a *API) ListBroadcasts(since time.Time, limit int) ([]BroadcastSummary, error) {
	rows, err := a.rollups(since)
	if err != nil {
		return nil, err
	}
	byKey := map[string]*BroadcastSummary{}
	for _, r := range rows {
		s, ok := byKey[r.BroadcastKey]
		if !ok {
			s = &BroadcastSummary{BroadcastKey: r.BroadcastKey, WorstVerdict: rules.SeverityOK}
			byKey[r.BroadcastKey] = s
		}
		s.Sessions++
		if r.Role == "viewer" {
			s.Viewers++
		}
		if s.FirstSeenMs == 0 || (r.StartedAt > 0 && r.StartedAt < s.FirstSeenMs) {
			s.FirstSeenMs = r.StartedAt
		}
		if r.EndedAt > s.LastSeenMs {
			s.LastSeenMs = r.EndedAt
		}
		if v := severityOfRow(r); v.Rank() > s.WorstVerdict.Rank() {
			s.WorstVerdict = v
		}
	}
	if a.live != nil {
		for _, b := range a.live.Snapshot().Live {
			s, ok := byKey[b.BroadcastKey]
			if !ok {
				s = &BroadcastSummary{BroadcastKey: b.BroadcastKey, WorstVerdict: rules.SeverityOK}
				byKey[b.BroadcastKey] = s
			}
			s.Live = true
			if v := maxSeverity(b.Severity, b.WorstViewer); v.Rank() > s.WorstVerdict.Rank() {
				s.WorstVerdict = v
			}
		}
	}

	out := make([]BroadcastSummary, 0, len(byKey))
	for _, s := range byKey {
		out = append(out, *s)
	}
	// Live first, then severity: only a live problem can still be acted on.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Live != out[j].Live {
			return out[i].Live
		}
		if out[i].WorstVerdict.Rank() != out[j].WorstVerdict.Rank() {
			return out[i].WorstVerdict.Rank() > out[j].WorstVerdict.Rank()
		}
		return out[i].LastSeenMs > out[j].LastSeenMs
	})
	return clamp(out, limit, DefaultRowLimit), nil
}

// --- list_sessions --------------------------------------------------------

// SessionSummary is a PROJECTION of a rollup row, not the row itself: a list
// endpoint that returned whole rows would blow the response ceiling on its
// first page (D10).
type SessionSummary struct {
	SessionID     string         `json:"sessionId"`
	BroadcastKey  string         `json:"broadcastKey"`
	Role          string         `json:"role"`
	Browser       string         `json:"browser,omitempty"`
	OS            string         `json:"os,omitempty"`
	StartedAtMs   int64          `json:"startedAtMs,omitempty"`
	DurationMs    int64          `json:"durationMs,omitempty"`
	Severity      rules.Severity `json:"severity"`
	Stalls        int            `json:"stalls"`
	Reconnects    int            `json:"reconnects"`
	RelayCoverage string         `json:"relayCoverage"`
	// Distrust is set when the row's own data quality makes its verdict
	// unreliable — a high anomaly count, a truncated session, dropped batches.
	Distrust string `json:"distrust,omitempty"`
}

// ListSessionsQuery filters list_sessions.
type ListSessionsQuery struct {
	BroadcastKey string
	Role         string
	Verdict      string
	Since        time.Time
	Limit        int
}

// ListSessions returns projected rollup rows.
func (a *API) ListSessions(q ListSessionsQuery) ([]SessionSummary, error) {
	rows, err := a.rollups(q.Since)
	if err != nil {
		return nil, err
	}
	out := make([]SessionSummary, 0, len(rows))
	for _, r := range rows {
		if q.BroadcastKey != "" && r.BroadcastKey != q.BroadcastKey {
			continue
		}
		if q.Role != "" && r.Role != q.Role {
			continue
		}
		sev := severityOfRow(r)
		if q.Verdict != "" && string(sev) != q.Verdict {
			continue
		}
		out = append(out, SessionSummary{
			SessionID: r.SessionID, BroadcastKey: r.BroadcastKey, Role: r.Role,
			Browser: r.Browser, OS: r.OS, StartedAtMs: r.StartedAt, DurationMs: r.DurationMs,
			Severity: sev, Stalls: r.Stalls, Reconnects: r.Reconnects,
			RelayCoverage: r.RelayCoverage, Distrust: distrustReason(r),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].StartedAtMs > out[j].StartedAtMs })
	return clamp(out, q.Limit, DefaultRowLimit), nil
}

// --- get_session ----------------------------------------------------------

// Timeline is get_session's bounded default response: a DOWNSAMPLED
// projection over a curated field set, plus every event. Events are never
// downsampled — they are the story, and there are few of them.
type Timeline struct {
	SessionID   string               `json:"sessionId"`
	Role        string               `json:"role"`
	Fields      []string             `json:"fields"`
	Points      []map[string]float64 `json:"points"`
	Events      []sessions.Record    `json:"events,omitempty"`
	Downsampled bool                 `json:"downsampled"`
	TotalSample int                  `json:"totalSamples"`
	Config      map[string]string    `json:"config,omitempty"`
	Note        string               `json:"note,omitempty"`
}

// curatedViewer / curatedBroadcaster are the default field sets. The full
// firehose requires naming fields AND a window explicitly (D10).
var curatedViewer = []string{
	"tMs", "receivedFps", "decoderFps", "renderedFps",
	"timeSinceLastFrameMs", "capToRenderMs", "playoutOffsetMs", "reorderGapResyncs",
}

var curatedBroadcaster = []string{
	"tMs", "captureFps", "encoderFps", "sentFps", "encoderQueueDepth", "viewerCount",
}

// GetSession returns a bounded timeline. `fields` and `window` opt into more.
func (a *API) GetSession(sessionID string, fields []string, points int) (*Timeline, error) {
	ref, err := a.store.FindSession(sessionID)
	if err != nil {
		return nil, err
	}
	lines, err := a.store.ReadSession(ref)
	if err != nil {
		return nil, err
	}
	in := sessions.ParseTimeline(sessions.Live{Ref: ref}, lines)

	if len(fields) == 0 {
		fields = curatedViewer
		if in.Role == "broadcaster" {
			fields = curatedBroadcaster
		}
	}
	if points <= 0 {
		points = DefaultTimelinePoints
	}

	tl := &Timeline{
		SessionID: sessionID, Role: in.Role, Fields: fields,
		TotalSample: len(in.Samples),
	}
	step := 1
	if len(in.Samples) > points {
		step = (len(in.Samples) + points - 1) / points
		tl.Downsampled = true
		tl.Note = fmt.Sprintf("every %dth of %d samples; name fields and a window for more", step, len(in.Samples))
	}
	for i := 0; i < len(in.Samples); i += step {
		s := in.Samples[i]
		pt := map[string]float64{"tMs": s.TMs}
		for _, f := range fields {
			if f == "tMs" {
				continue
			}
			if v, ok := schema.Number(s.Stats, f); ok {
				pt[f] = v
			}
		}
		tl.Points = append(tl.Points, pt)
	}
	for _, ln := range lines {
		var rec sessions.Record
		if err := json.Unmarshal(ln, &rec); err != nil || rec.Kind != "event" {
			continue
		}
		rec.Stats = nil
		tl.Events = append(tl.Events, rec)
	}
	return tl, nil
}

// --- diagnose -------------------------------------------------------------

// Diagnose runs the playbook over one stored session (docs/33 D6).
//
// It returns verdicts + evidence, never the underlying series. An MCP surface
// that dumps 80-field series is WORSE than today's copy-paste, because it
// spends context to arrive at the same place.
func (a *API) Diagnose(sessionID string) (*rules.Report, error) {
	ref, err := a.store.FindSession(sessionID)
	if err != nil {
		return nil, err
	}
	lines, err := a.store.ReadSession(ref)
	if err != nil {
		return nil, err
	}
	in := sessions.ParseTimeline(sessions.Live{Ref: ref}, lines)
	row := rollup.Compute(in)
	relayObs, _ := a.store.ReadRelay(ref.Date)

	f := a.factsFor(row, in, relayObs)
	rep := rules.Evaluate(f, a.rs)
	rep.Subject = sessionID
	if a.dashboard != "" {
		rep.DashboardURL = a.dashboard + "#/session/" + sessionID
	}
	return &rep, nil
}

// DiagnoseRow builds a report from an already-computed row + timeline. Used at
// finalize so the STORED verdict and a later diagnose() of the same session
// come out of one code path.
func (a *API) DiagnoseRow(row rollup.Row, in rollup.Input, relayObs [][]byte) rules.Report {
	f := a.factsFor(row, in, relayObs)
	rep := rules.Evaluate(f, a.rs)
	rep.Subject = row.SessionID
	if a.dashboard != "" {
		rep.DashboardURL = a.dashboard + "#/session/" + row.SessionID
	}
	return rep
}

// factsFor assembles the fact set: the client's own numbers, the relay's
// observations joined by sessionId, and the caveats that cap how much either
// should be believed.
func (a *API) factsFor(row rollup.Row, in rollup.Input, relayLines [][]byte) *rules.Facts {
	f := rules.NewFacts(row.SessionID, "session", row.Role)

	// Client side: the LAST observed value of each live signal, plus the
	// worst-case of the experiential series (a session's verdict is about how
	// bad it got, not how it ended).
	if n := len(in.Samples); n > 0 {
		last := in.Samples[n-1].Stats
		for _, name := range factClientFields {
			if v, ok := schema.Number(last, name); ok {
				f.SetClient(name, v)
			}
		}
		if hw, ok := schema.Bool(last, "isHardwareAccelerated"); ok {
			f.SetClient("isHardwareAccelerated", boolF(hw))
		}
		if ab := schema.Nested(last, "audioBuffer"); ab != nil {
			if v, ok := schema.Number(ab, "overflowDrops"); ok {
				f.SetClient("audioOverflowDrops", v)
			}
			if v, ok := schema.Number(ab, "gapsConcealed"); ok {
				f.SetClient("audioGapsConcealed", v)
			}
		}
		for name, v := range row.Config {
			f.SetText(name, v)
		}
	}
	// Session-level overrides, computed over the whole timeline rather than
	// whatever the last sample happened to be.
	//
	// Every funnel rule compares two rates, and both sides MUST be measured
	// the same way. An earlier version took the median of one side and the p05
	// of the other, which made the comparison "typical received vs. worst
	// decoded" — guaranteed to look like a gap on any session with a warmup
	// ramp, and it fired `decoder-choking` on a clean 30 fps loopback stream in
	// the e2e pass. A verdict that is confidently wrong is the risk docs/33 §8
	// names; mixing statistics across a ratio is how you manufacture one.
	//
	// Medians on both sides answer the question a session verdict actually
	// asks: did this stage keep up for MOST of the session? The bad tail is
	// still recorded — it is on the rollup row, and the stall fields name the
	// episodes — it just does not get to masquerade as one side of a ratio.
	for _, name := range []string{
		"receivedFps", "decoderFps", "renderedFps",
		"captureFps", "encoderFps", "sentFps",
	} {
		if st := row.Series[name]; st != nil {
			f.SetClient(name, st.Median)
		}
	}
	// The playout offset is not half of a ratio: its rule asks whether the
	// buffer ever pinned at its clamp, which is a tail question.
	if st := row.Series["playoutOffsetMs"]; st != nil {
		f.SetClient("playoutOffsetMs", st.P95)
	}
	if row.LongestStallMs > 0 {
		f.SetClient("timeSinceLastFrameMs", row.LongestStallMs)
	}

	// Relay side, joined by sessionId (TM4).
	coverage := "none"
	for _, ln := range relayLines {
		var o relayObservation
		if err := json.Unmarshal(ln, &o); err != nil {
			continue
		}
		if o.SessionID == row.SessionID && o.Subscriber != nil {
			coverage = "full"
			f.SetRelay("subscriberDropped", float64(o.Subscriber.Dropped))
			f.SetRelay("keyframesDropped", float64(o.Subscriber.KeyframesDropped))
			f.SetRelay("carrierQueueOverflow", float64(o.Subscriber.CarrierQueueOverflow))
			f.SetRelay("carrierRecordsDropped", float64(o.Subscriber.CarrierRecordsDropped))
			f.SetRelay("dvrResyncs", float64(o.Subscriber.DVRResyncs))
			f.SetRelay("dvrLagMs", float64(o.Subscriber.DVRLagMs))
		}
		if o.BroadcastKey == row.BroadcastKey && o.Broadcast != nil {
			f.SetRelay("publisherActive", boolF(o.Broadcast.PublisherActive))
			total := float64(o.Broadcast.IngressFramesLost + o.Broadcast.FramesRelayed)
			if total > 0 {
				f.SetRelay("ingressLossRatio", float64(o.Broadcast.IngressFramesLost)/total)
			}
		}
	}
	if row.RelayCoverage != "" && row.RelayCoverage != "none" {
		coverage = row.RelayCoverage
	}

	// Caveats: reasons to distrust this whole report, surfaced rather than
	// silently reducing confidence.
	if coverage == "none" {
		f.Caveats = append(f.Caveats,
			"no relay-side observation of this session — every verdict below rests on client testimony alone (docs/33 D5/D7)")
	}
	if d := distrustReason(row); d != "" {
		f.Caveats = append(f.Caveats, d)
	}
	return f
}

type relayObservation struct {
	Kind         string `json:"kind"`
	Pod          string `json:"pod"`
	Role         string `json:"role"`
	BroadcastKey string `json:"broadcastKey"`
	SessionID    string `json:"sessionId"`
	Broadcast    *struct {
		PublisherActive   bool   `json:"publisherActive"`
		FramesRelayed     uint64 `json:"framesRelayed"`
		IngressFramesLost uint64 `json:"ingressFramesLost"`
	} `json:"broadcast"`
	Subscriber *struct {
		Dropped               uint64 `json:"dropped"`
		KeyframesDropped      uint64 `json:"keyframesDropped"`
		CarrierQueueOverflow  uint64 `json:"carrierQueueOverflow"`
		CarrierRecordsDropped uint64 `json:"carrierRecordsDropped"`
		DVRResyncs            uint64 `json:"dvrResyncs"`
		DVRLagMs              int64  `json:"dvrLagMs"`
	} `json:"subscriber"`
}

// --- fleet_summary --------------------------------------------------------

// FleetSummary is the "overall average" a session is compared against.
type FleetSummary struct {
	Since   string                 `json:"since"`
	Groups  map[string]*FleetGroup `json:"groups"`
	Overall *FleetGroup            `json:"overall"`
}

// FleetGroup is one class's percentiles.
type FleetGroup struct {
	Sessions  int     `json:"sessions"`
	MedianFps float64 `json:"medianReceivedFps,omitempty"`
	MedianLat float64 `json:"medianCapToRenderMs,omitempty"`
	StallRate float64 `json:"stallsPerSession"`
	BadShare  float64 `json:"badShare"`
}

// FleetSummary groups rollups by delivery mode, browser class or rung.
func (a *API) FleetSummary(since time.Time, groupBy string) (*FleetSummary, error) {
	rows, err := a.rollups(since)
	if err != nil {
		return nil, err
	}
	out := &FleetSummary{Since: since.UTC().Format(time.RFC3339), Groups: map[string]*FleetGroup{}}
	overall := &FleetGroup{}
	var allFps, allLat []float64
	byGroup := map[string][]rollup.Row{}

	for _, r := range rows {
		if r.Role != "viewer" {
			continue
		}
		overall.Sessions++
		overall.StallRate += float64(r.Stalls)
		if severityOfRow(r) == rules.SeverityBad {
			overall.BadShare++
		}
		if st := r.Series["receivedFps"]; st != nil {
			allFps = append(allFps, st.Median)
		}
		if st := r.Series["capToRenderMs"]; st != nil {
			allLat = append(allLat, st.Median)
		}
		byGroup[groupKey(r, groupBy)] = append(byGroup[groupKey(r, groupBy)], r)
	}
	if overall.Sessions > 0 {
		overall.StallRate /= float64(overall.Sessions)
		overall.BadShare /= float64(overall.Sessions)
	}
	overall.MedianFps = median(allFps)
	overall.MedianLat = median(allLat)
	out.Overall = overall

	for key, group := range byGroup {
		g := &FleetGroup{Sessions: len(group)}
		var fps, lat []float64
		for _, r := range group {
			g.StallRate += float64(r.Stalls)
			if severityOfRow(r) == rules.SeverityBad {
				g.BadShare++
			}
			if st := r.Series["receivedFps"]; st != nil {
				fps = append(fps, st.Median)
			}
			if st := r.Series["capToRenderMs"]; st != nil {
				lat = append(lat, st.Median)
			}
		}
		g.StallRate /= float64(len(group))
		g.BadShare /= float64(len(group))
		g.MedianFps = median(fps)
		g.MedianLat = median(lat)
		out.Groups[key] = g
	}
	return out, nil
}

func groupKey(r rollup.Row, by string) string {
	switch by {
	case "browser":
		if r.Browser == "" {
			return "unknown"
		}
		return r.Browser
	case "os":
		if r.OS == "" {
			return "unknown"
		}
		return r.OS
	case "resolution":
		if v, ok := r.Config["resolution"]; ok {
			return v
		}
		return "unknown"
	default: // deliveryMode
		if v, ok := r.Config["deliveryMode"]; ok {
			return v
		}
		return "unknown"
	}
}

// --- compare --------------------------------------------------------------

// Comparison is one session against the fleet median for its own class.
type Comparison struct {
	SessionID string           `json:"sessionId"`
	Class     string           `json:"class"`
	Deltas    map[string]Delta `json:"deltas"`
	Note      string           `json:"note,omitempty"`
}

// Delta is one field's session-vs-fleet reading.
type Delta struct {
	Session float64 `json:"session"`
	Fleet   float64 `json:"fleet"`
	Ratio   float64 `json:"ratio,omitempty"`
}

// Compare places a session against the fleet median for the same class.
func (a *API) Compare(sessionID string, since time.Time) (*Comparison, error) {
	ref, err := a.store.FindSession(sessionID)
	if err != nil {
		return nil, err
	}
	lines, err := a.store.ReadSession(ref)
	if err != nil {
		return nil, err
	}
	row := rollup.Compute(sessions.ParseTimeline(sessions.Live{Ref: ref}, lines))
	class := groupKey(row, "deliveryMode")

	fleet, err := a.FleetSummary(since, "deliveryMode")
	if err != nil {
		return nil, err
	}
	g := fleet.Groups[class]
	if g == nil {
		g = fleet.Overall
	}
	c := &Comparison{SessionID: sessionID, Class: class, Deltas: map[string]Delta{}}
	if st := row.Series["receivedFps"]; st != nil && g.MedianFps > 0 {
		c.Deltas["receivedFps"] = Delta{Session: st.Median, Fleet: g.MedianFps, Ratio: st.Median / g.MedianFps}
	}
	if st := row.Series["capToRenderMs"]; st != nil && g.MedianLat > 0 {
		c.Deltas["capToRenderMs"] = Delta{Session: st.Median, Fleet: g.MedianLat, Ratio: st.Median / g.MedianLat}
	}
	c.Deltas["stalls"] = Delta{Session: float64(row.Stalls), Fleet: g.StallRate}
	if g.Sessions < 5 {
		c.Note = fmt.Sprintf("fleet baseline is only %d sessions; treat the comparison as weak", g.Sessions)
	}
	return c, nil
}

// JoinRelay fills a rollup row's relay-side fields from the scraped
// observations (docs/33 TM4). Without this the row's `relayCoverage` would
// stay at its "none" default forever — which is not merely a missing field but
// an active lie: every verdict would be caveated as client-only even when the
// relay watched the whole session.
//
// `interval` is the scrape interval, which is what decides whether the
// observations amount to full coverage or merely partial (D5): a session
// shorter than two intervals cannot have been sampled enough to claim full,
// even if one scrape happened to catch it.
func JoinRelay(row *rollup.Row, relayLines [][]byte, interval time.Duration) {
	var seen bool
	relay := map[string]float64{}
	for _, ln := range relayLines {
		var o relayObservation
		if err := json.Unmarshal(ln, &o); err != nil {
			continue
		}
		if o.SessionID == row.SessionID && o.Subscriber != nil {
			seen = true
			if o.Pod != "" {
				row.RelayPod = o.Pod
			}
			if o.Role != "" {
				row.RelayRole = o.Role
			}
			// Cumulative counters: the LAST observation is the session total
			// the relay saw. Close-time folded totals are missed by
			// construction (D5) — the delta is bounded by the interval.
			relay["dropped"] = float64(o.Subscriber.Dropped)
			relay["keyframesDropped"] = float64(o.Subscriber.KeyframesDropped)
			relay["carrierQueueOverflow"] = float64(o.Subscriber.CarrierQueueOverflow)
			relay["carrierRecordsDropped"] = float64(o.Subscriber.CarrierRecordsDropped)
			relay["dvrResyncs"] = float64(o.Subscriber.DVRResyncs)
		}
		if o.BroadcastKey == row.BroadcastKey && o.Broadcast != nil {
			if row.Role == "broadcaster" && o.SessionID == row.SessionID {
				seen = true
				if o.Pod != "" {
					row.RelayPod = o.Pod
				}
				if o.Role != "" {
					row.RelayRole = o.Role
				}
			}
			relay["ingressFramesLost"] = float64(o.Broadcast.IngressFramesLost)
			relay["framesRelayed"] = float64(o.Broadcast.FramesRelayed)
		}
	}
	if len(relay) > 0 {
		row.Relay = relay
	}
	start := time.UnixMilli(row.StartedAt)
	end := time.UnixMilli(row.EndedAt)
	if row.EndedAt <= row.StartedAt {
		end = start
	}
	row.RelayCoverage = relayscrape.CoverageFor(seen, start, end, interval)
}

// LiveSnapshot is the TM8 projection, or an empty one where no projection is
// wired. Shared by the dashboard's /live endpoint and the MCP `live` tool, so
// the two can never show different fleets.
func (a *API) LiveSnapshot() live.Snapshot {
	if a.live == nil {
		return live.Snapshot{AtMs: a.now().UnixMilli()}
	}
	return a.live.Snapshot()
}

// --- shared helpers -------------------------------------------------------

func (a *API) rollups(since time.Time) ([]rollup.Row, error) {
	lines, err := a.store.ReadRollups(since)
	if err != nil {
		return nil, err
	}
	out := make([]rollup.Row, 0, len(lines))
	for _, ln := range lines {
		var r rollup.Row
		// An older row simply lacks newer fields — that is the additive-forever
		// rule working, not an error (D4).
		if err := json.Unmarshal(ln, &r); err != nil {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// severityOfRow reads a row's STORED verdict rather than recomputing it: the
// stored one was computed with the raw window in hand, which by now may be
// gone.
func severityOfRow(r rollup.Row) rules.Severity {
	if len(r.Verdict) > 0 {
		var rep rules.Report
		if err := json.Unmarshal(r.Verdict, &rep); err == nil {
			return rep.Severity()
		}
	}
	// No stored verdict: fall back to the row's own hard facts rather than
	// claiming health, which would paint an absence of analysis as green.
	if r.Stalls > 0 || r.Reconnects > 2 {
		return rules.SeverityWarn
	}
	if r.Samples == 0 {
		return rules.SeverityUnknown
	}
	return rules.SeverityOK
}

// distrustReason states, in one sentence, why a row's own verdict should be
// taken with a pinch of salt (D15's schemaAnomalies made actionable).
func distrustReason(r rollup.Row) string {
	switch {
	case r.Truncated:
		return "session hit its byte budget and reported events only after that point"
	case r.SeqGaps > 0:
		return fmt.Sprintf("%d batch(es) never arrived — the timeline has holes", r.SeqGaps)
	case r.Samples > 0 && r.SchemaAnomalies.Dropped > r.Samples:
		return fmt.Sprintf("%d stats fields were dropped as unusable across %d samples — a client bug is likely",
			r.SchemaAnomalies.Dropped, r.Samples)
	}
	return ""
}

func clamp[T any](rows []T, limit, def int) []T {
	if limit <= 0 {
		limit = def
	}
	if len(rows) > limit {
		return rows[:limit]
	}
	return rows
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[len(s)/2]
}

func maxSeverity(a, b rules.Severity) rules.Severity {
	if a.Rank() >= b.Rank() {
		return a
	}
	return b
}

func boolF(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

var factClientFields = []string{
	"receivedFps", "decoderFps", "renderedFps", "decoderQueueDepth",
	"timeSinceLastFrameMs", "timeSinceLastInboundMs",
	"reorderGapResyncs", "keyframeStreamsReceived", "playoutOffsetMs",
	"captureFps", "encoderFps", "sentFps", "encoderQueueDepth",
	"audioPacketsReceived",
}

// --- HTTP -----------------------------------------------------------------

// Handler mounts the read API. Never routed publicly (D14).
func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/broadcasts", func(w http.ResponseWriter, r *http.Request) {
		out, err := a.ListBroadcasts(sinceOf(r, a.now()), intOf(r, "limit"))
		writeJSON(w, out, err)
	})
	mux.HandleFunc("GET /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		out, err := a.ListSessions(ListSessionsQuery{
			BroadcastKey: r.URL.Query().Get("broadcast"),
			Role:         r.URL.Query().Get("role"),
			Verdict:      r.URL.Query().Get("verdict"),
			Since:        sinceOf(r, a.now()),
			Limit:        intOf(r, "limit"),
		})
		writeJSON(w, out, err)
	})
	mux.HandleFunc("GET /v1/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		var fields []string
		if f := r.URL.Query().Get("fields"); f != "" {
			fields = splitCSV(f)
		}
		out, err := a.GetSession(r.PathValue("id"), fields, intOf(r, "points"))
		writeJSON(w, out, err)
	})
	mux.HandleFunc("GET /v1/sessions/{id}/diagnose", func(w http.ResponseWriter, r *http.Request) {
		out, err := a.Diagnose(r.PathValue("id"))
		writeJSON(w, out, err)
	})
	mux.HandleFunc("GET /v1/sessions/{id}/compare", func(w http.ResponseWriter, r *http.Request) {
		out, err := a.Compare(r.PathValue("id"), sinceOf(r, a.now()))
		writeJSON(w, out, err)
	})
	mux.HandleFunc("GET /v1/fleet", func(w http.ResponseWriter, r *http.Request) {
		out, err := a.FleetSummary(sinceOf(r, a.now()), r.URL.Query().Get("groupBy"))
		writeJSON(w, out, err)
	})
	mux.HandleFunc("GET /live", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, a.LiveSnapshot(), nil)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, v any, err error) {
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		} else if errors.Is(err, store.ErrBadIdentifier) {
			status = http.StatusBadRequest
		}
		http.Error(w, http.StatusText(status), status)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func sinceOf(r *http.Request, now time.Time) time.Time {
	if v := r.URL.Query().Get("since"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t
		}
		if d, err := time.ParseDuration(v); err == nil {
			return now.Add(-d)
		}
	}
	return now.Add(-24 * time.Hour)
}

func intOf(r *http.Request, key string) int {
	n, _ := strconv.Atoi(r.URL.Query().Get(key))
	return n
}

func splitCSV(s string) []string {
	var out []string
	cur := ""
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			if cur != "" {
				out = append(out, cur)
			}
			cur = ""
			continue
		}
		cur += string(s[i])
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
