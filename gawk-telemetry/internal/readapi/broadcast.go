package readapi

// TH4's broadcast timeline (docs/36 §4 Wave 2) — the isolating surface.
//
// One broadcast, one absolute time axis, one lane per participant plus a relay
// lane. It is the only thing in the design that can answer Q4: *did they all
// dip at the same second, or just that one viewer?*
//
// # The honesty that makes a shared axis legitimate
//
// The sources have different clocks AND different cadences: a client batches
// every ~10 s and samples inside it every ~2 s, the relay is scraped every ~5 s,
// the live projection refreshes every 2 s. Drawing them at one resolution would
// imply a precision none of them has. So **each lane carries its own cadence**
// and the UI is required to state it.
//
// The clock problem is separate and is solved by measurement, not assumption.
// Every stored record carries both `tMs` (the client's own monotonic clock,
// relative to session start) and `receivedAtMs` (this service's wall clock).
// The median of their difference is the lane's offset, and it is published so
// the alignment is a stated correction rather than a hidden one.

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/rollup"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/rules"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/schema"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/sessions"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/store"
)

// LanePoints bounds one lane's series. Five lanes at this size is TH11's
// stated budget (5 × 2 000 points), and the bound is per lane so a broadcast
// with twenty viewers costs twenty bounded lanes rather than one unbounded one.
const LanePoints = 2000

// MaxLanes bounds how many participants one response describes. Above UD12's
// scale target the answer is a list, not a timeline — twenty lanes on one axis
// is already unreadable, and pretending otherwise would produce a response
// nobody can use and a page nobody can render.
const MaxLanes = 24

// BroadcastDetail is one broadcast on one axis.
type BroadcastDetail struct {
	BroadcastKey string `json:"broadcastKey"`
	// FromMs/ToMs are the axis: the union of every lane's span.
	FromMs int64  `json:"fromMs"`
	ToMs   int64  `json:"toMs"`
	Live   bool   `json:"live"`
	Lanes  []Lane `json:"lanes"`
	// Rehomes are relay pod/role changes mid-broadcast. R17 makes them routine
	// and no view before this could show one.
	Rehomes []Rehome `json:"rehomes,omitempty"`
	// LanesOmitted is how many participants did not fit under MaxLanes. Stated,
	// never silent: a timeline that quietly drops the viewer you were looking
	// for is worse than one that refuses.
	LanesOmitted int `json:"lanesOmitted,omitempty"`
	Coverage     `json:"coverage"`
}

// Lane is one participant's row on the shared axis.
type Lane struct {
	// Kind is broadcaster | viewer | relay. The relay lane has no session.
	Kind         string         `json:"kind"`
	SessionID    string         `json:"sessionId,omitempty"`
	Browser      string         `json:"browser,omitempty"`
	OS           string         `json:"os,omitempty"`
	AppVersion   string         `json:"appVersion,omitempty"`
	DeliveryMode string         `json:"deliveryMode,omitempty"`
	Severity     rules.Severity `json:"severity"`
	Verdict      string         `json:"verdict,omitempty"`
	// StartedAtMs/EndedAtMs are the SPAN — join to leave. A lane draws its span
	// as a bounded band, so a viewer who joined late reads as "was not here",
	// never as a gap in a line (TH4's criterion).
	StartedAtMs int64 `json:"startedAtMs"`
	EndedAtMs   int64 `json:"endedAtMs,omitempty"`
	// CadenceMs is this source's own reporting interval. UD9's rule as a wire
	// field: no lane may be drawn at a resolution its source cannot support.
	CadenceMs int64 `json:"cadenceMs"`
	// ClockOffsetMs is the measured correction between the service's clock and
	// this client's, already APPLIED to the points below. Published so the
	// alignment can be checked rather than believed.
	ClockOffsetMs float64 `json:"clockOffsetMs,omitempty"`
	// Primary names the field the lane's line is; Unit labels its axis.
	Primary string `json:"primary,omitempty"`
	Unit    string `json:"unit,omitempty"`
	// Points carry ABSOLUTE time (UD5) — atMs, not tMs. The relative form is a
	// session-local convenience and has no meaning on a shared axis.
	Points []LanePoint `json:"points,omitempty"`
	// Dips are this lane's episodes as absolute spans, which is what makes
	// "did they all dip together?" a visual answer rather than an arithmetic one.
	Dips   []DipSpan   `json:"dips,omitempty"`
	Events []LaneEvent `json:"events,omitempty"`
	// Hidden are the spans where the tab was in the background. Shaded, so a
	// collapsed rendered rate is explained rather than left as a cliff.
	Hidden []Interval `json:"hidden,omitempty"`
	// RollupOnly marks a lane whose raw window is gone: it has a span, a
	// severity and a verdict, and no line (UD10 — never an empty chart).
	RollupOnly  bool   `json:"rollupOnly,omitempty"`
	Note        string `json:"note,omitempty"`
	Downsampled bool   `json:"downsampled,omitempty"`
}

// LanePoint is one sample on the shared axis.
type LanePoint struct {
	AtMs  int64   `json:"atMs"`
	Value float64 `json:"value"`
}

// DipSpan is one dip episode placed absolutely.
type DipSpan struct {
	FromMs     int64              `json:"fromMs"`
	ToMs       int64              `json:"toMs"`
	WorstValue float64            `json:"worstValue"`
	Baseline   float64            `json:"baseline"`
	Deltas     map[string]float64 `json:"deltas,omitempty"`
	Before     map[string]float64 `json:"before,omitempty"`
	After      map[string]float64 `json:"after,omitempty"`
}

// Interval is a plain absolute span, for shading.
type Interval struct {
	FromMs int64 `json:"fromMs"`
	ToMs   int64 `json:"toMs"`
}

// LaneEvent is one stored event placed absolutely.
type LaneEvent struct {
	AtMs   int64  `json:"atMs"`
	Kind   string `json:"kind"`
	Detail string `json:"detail,omitempty"`
}

// Rehome is a relay pod or role change during a broadcast.
type Rehome struct {
	AtMs     int64  `json:"atMs"`
	FromPod  string `json:"fromPod,omitempty"`
	ToPod    string `json:"toPod"`
	FromRole string `json:"fromRole,omitempty"`
	ToRole   string `json:"toRole,omitempty"`
}

// BroadcastQuery bounds a broadcast timeline request.
type BroadcastQuery struct {
	FromMs, ToMs int64
	// Since bounds the rollup scan. Defaults to 24 h back like every other list
	// endpoint, so an unqualified request does not walk the whole retention
	// window.
	Since time.Time
}

// GetBroadcast assembles TH4's multi-lane timeline.
func (a *API) GetBroadcast(key string, q BroadcastQuery) (*BroadcastDetail, error) {
	if err := (store.SessionRef{Date: "2000-01-01", BroadcastKey: key, SessionID: "000000000000000000000000"}).Validate(); err != nil {
		return nil, err
	}
	rows, err := a.rollups(q.Since)
	if err != nil {
		return nil, err
	}
	cov, err := a.coverage(HistoryQuery{From: q.Since})
	if err != nil {
		return nil, err
	}
	out := &BroadcastDetail{BroadcastKey: key, Coverage: cov}

	var mine []rollup.Row
	for _, r := range rows {
		if r.BroadcastKey == key {
			mine = append(mine, r)
		}
	}
	// Live sessions have no rollup row yet — they have not ended — and a
	// timeline that only showed finished participants would be blind during the
	// exact incident it exists for. The projection supplies their identity; the
	// SAMPLES still come from disk (§1.1), so a live lane is drawn at full
	// resolution like any other.
	liveRows := a.liveRowsFor(key)
	for _, lr := range liveRows {
		found := false
		for _, r := range mine {
			if r.SessionID == lr.SessionID {
				found = true
				break
			}
		}
		if !found {
			mine = append(mine, lr)
			out.Live = true
		}
	}

	// Broadcaster first, then viewers by join time: the top lane is the one
	// every other lane depends on, so it reads top-down as causality.
	sort.SliceStable(mine, func(i, j int) bool {
		if (mine[i].Role == "broadcaster") != (mine[j].Role == "broadcaster") {
			return mine[i].Role == "broadcaster"
		}
		return mine[i].StartedAt < mine[j].StartedAt
	})
	if len(mine) > MaxLanes {
		out.LanesOmitted = len(mine) - MaxLanes
		mine = mine[:MaxLanes]
	}

	for _, r := range mine {
		lane := a.sessionLane(r, q, cov.RawFromMs)
		out.Lanes = append(out.Lanes, lane)
		if lane.StartedAtMs > 0 && (out.FromMs == 0 || lane.StartedAtMs < out.FromMs) {
			out.FromMs = lane.StartedAtMs
		}
		if lane.EndedAtMs > out.ToMs {
			out.ToMs = lane.EndedAtMs
		}
	}

	relay, rehomes := a.relayLane(key, out.FromMs, out.ToMs)
	if relay != nil {
		out.Lanes = append(out.Lanes, *relay)
	}
	out.Rehomes = rehomes
	return out, nil
}

// sessionLane projects one session onto the shared axis.
func (a *API) sessionLane(r rollup.Row, q BroadcastQuery, rawFromMs int64) Lane {
	primary, counters := rollup.PrimarySeries(r.Role)
	lane := Lane{
		Kind: r.Role, SessionID: r.SessionID,
		Browser: r.Browser, OS: r.OS, AppVersion: r.AppVersion,
		DeliveryMode: r.Config["deliveryMode"],
		Severity:     severityOfRow(r),
		StartedAtMs:  r.StartedAt, EndedAtMs: r.EndedAt,
		Primary: primary, Unit: "fps",
		// A client emits a sample every ~2 s and flushes a batch every ~10 s.
		// The SAMPLE cadence is what the line may be read at; the batch cadence
		// only decides how late it arrives.
		CadenceMs: 2000,
	}
	if rep := storedReport(r); len(rep.Findings) > 0 {
		lane.Verdict = rep.Findings[0].Verdict
	}

	ref, err := a.store.FindSession(r.SessionID)
	if err != nil {
		// No raw file: the session is either pruned or was never written. Both
		// give a lane with a span, a severity and a verdict and no line — which
		// is UD10's "absence is never blank" rather than an error.
		lane.RollupOnly = true
		lane.Note = "raw samples are no longer stored for this session — span and verdict only"
		if r.StartedAt > 0 && rawFromMs > 0 && r.StartedAt >= rawFromMs {
			lane.Note = "no stored samples for this session"
		}
		return lane
	}
	lines, err := a.store.ReadSession(ref)
	if err != nil {
		lane.RollupOnly = true
		lane.Note = "stored samples could not be read"
		return lane
	}
	in := sessions.ParseTimeline(sessions.Live{Ref: ref}, lines)
	if in.StartedAtMs > 0 {
		lane.StartedAtMs = in.StartedAtMs
	}
	if in.EndedAtMs > 0 && in.EndedAtMs > lane.EndedAtMs {
		lane.EndedAtMs = in.EndedAtMs
	}
	// The offset is MEASURED and then applied, so lanes from clients whose
	// clocks disagree still land on one axis.
	lane.ClockOffsetMs = clockOffset(lines, in.StartedAtMs)
	at := func(tMs float64) int64 {
		return in.StartedAtMs + int64(tMs) + int64(lane.ClockOffsetMs)
	}

	samples := in.Samples
	if q.FromMs > 0 || q.ToMs > 0 {
		samples = windowSamples(samples, in.StartedAtMs+int64(lane.ClockOffsetMs), q.FromMs, q.ToMs)
	}
	step := 1
	if len(samples) > LanePoints {
		step = (len(samples) + LanePoints - 1) / LanePoints
		lane.Downsampled = true
		lane.Note = "worst of every " + itoa(step) + " samples, so a dip cannot be downsampled away"
	}
	for i := 0; i < len(samples); i += step {
		end := min(i+step, len(samples))
		s := samples[worstIn(samples[i:end], primary)+i]
		v, ok := schema.Number(s.Stats, primary)
		if !ok {
			// A sample that did not report the rate is a BREAK, not a zero
			// (UD9). Omitting the point is what draws it as one.
			continue
		}
		lane.Points = append(lane.Points, LanePoint{AtMs: at(s.TMs), Value: v})
	}

	// Dips from the SAME detector the verdict used, so a dip on screen and a dip
	// in a verdict can never be different dips.
	if _, spans := rollup.DetectEpisodeSpans(in.Samples, primary, counters); len(spans) > 0 {
		for _, sp := range spans {
			lane.Dips = append(lane.Dips, DipSpan{
				FromMs: at(sp.StartMs), ToMs: at(sp.EndMs),
				WorstValue: sp.WorstValue, Baseline: sp.Baseline,
				Deltas: sp.Deltas, Before: sp.Before, After: sp.After,
			})
		}
	}
	for _, e := range in.Events {
		lane.Events = append(lane.Events, LaneEvent{AtMs: at(e.TMs), Kind: e.Kind, Detail: e.Detail})
	}
	lane.Hidden = hiddenIntervals(in.Samples, at)
	return lane
}

// hiddenIntervals folds documentHidden into contiguous absolute spans.
func hiddenIntervals(samples []rollup.Sample, at func(float64) int64) []Interval {
	var out []Interval
	start := -1.0
	for _, s := range samples {
		on, ok := schema.Bool(s.Stats, "documentHidden")
		if ok && on && start < 0 {
			start = s.TMs
		}
		if (!ok || !on) && start >= 0 {
			out = append(out, Interval{FromMs: at(start), ToMs: at(s.TMs)})
			start = -1
		}
	}
	if start >= 0 && len(samples) > 0 {
		out = append(out, Interval{FromMs: at(start), ToMs: at(samples[len(samples)-1].TMs)})
	}
	return out
}

// relayLane builds the relay's own row plus every re-home inside the window.
//
// `ReadRelay` is whole-date only, so a broadcast spanning midnight needs both
// partitions. Walking the dates the SPAN touches — rather than the whole
// retention window — is what keeps this a click-priced read.
func (a *API) relayLane(key string, fromMs, toMs int64) (*Lane, []Rehome) {
	if fromMs == 0 && toMs == 0 {
		return nil, nil
	}
	lane := Lane{
		Kind: "relay", Primary: "framesRelayedPerSec", Unit: "fps",
		StartedAtMs: fromMs, EndedAtMs: toMs,
		CadenceMs: a.scrapeInterval.Milliseconds(),
		Severity:  rules.SeverityUnknown,
	}
	var obs []relayObservation
	for _, date := range datesBetween(fromMs, toMs) {
		lines, err := a.store.ReadRelay(date)
		if err != nil {
			continue
		}
		for _, ln := range lines {
			var o relayObservation
			if err := json.Unmarshal(ln, &o); err != nil || o.BroadcastKey != key || o.Broadcast == nil {
				continue
			}
			// The ORIGIN only. An edge pod counts a different leg, and mixing
			// the two into one rate would compare unrelated counters — the same
			// trap factsFor already avoids for framesRelayedPerSec.
			if o.Role == "edge" {
				continue
			}
			obs = append(obs, o)
		}
	}
	if len(obs) == 0 {
		lane.Note = "no relay observation of this broadcast — every verdict on the lanes above rests on client testimony alone (D5/D7)"
		return &lane, nil
	}
	sort.Slice(obs, func(i, j int) bool { return obs[i].AtMs < obs[j].AtMs })

	var rehomes []Rehome
	prev := obs[0]
	for i, o := range obs {
		if i > 0 && (o.Pod != prev.Pod || o.Role != prev.Role) {
			rehomes = append(rehomes, Rehome{
				AtMs: o.AtMs, FromPod: prev.Pod, ToPod: o.Pod,
				FromRole: prev.Role, ToRole: o.Role,
			})
		}
		if i > 0 {
			secs := float64(o.AtMs-prev.AtMs) / 1000
			// A counter that went backwards is a restart or a re-home, not
			// negative traffic. A rate computed across one would be a fiction,
			// so the interval is skipped and the line breaks there — which is
			// exactly what happened to the delivery it describes.
			if secs > 0 && o.Broadcast.FramesRelayed >= prev.Broadcast.FramesRelayed && o.Pod == prev.Pod {
				lane.Points = append(lane.Points, LanePoint{
					AtMs:  o.AtMs,
					Value: float64(o.Broadcast.FramesRelayed-prev.Broadcast.FramesRelayed) / secs,
				})
			}
		}
		prev = o
	}
	last := obs[len(obs)-1]
	lane.Severity = rules.SeverityOK
	if total := last.Broadcast.IngressFramesLost + last.Broadcast.FramesRelayed; total > 0 {
		if float64(last.Broadcast.IngressFramesLost)/float64(total) > 0.01 {
			lane.Severity = rules.SeverityWarn
			lane.Verdict = "the relay lost ingress frames on this broadcast"
		}
	}
	return &lane, rehomes
}

// liveRowsFor synthesizes rollup-shaped rows for the sessions the projection
// currently holds open. They have no permanent row yet — that is written when
// they end — and a timeline blind to them would be blind during the incident.
func (a *API) liveRowsFor(key string) []rollup.Row {
	if a.live == nil {
		return nil
	}
	var out []rollup.Row
	for _, b := range a.live.Snapshot().Live {
		if b.BroadcastKey != key {
			continue
		}
		for _, s := range b.Sessions {
			out = append(out, rollup.Row{
				SessionID: s.SessionID, BroadcastKey: key, Role: s.Role,
				Browser: s.Browser, OS: s.OS, AppVersion: s.AppVersion,
				StartedAt: s.StartedAtMs, Config: s.Config,
			})
		}
	}
	return out
}

// datesBetween lists the UTC date partitions an absolute range touches.
func datesBetween(fromMs, toMs int64) []string {
	if fromMs <= 0 {
		fromMs = toMs
	}
	if toMs < fromMs {
		toMs = fromMs
	}
	start := time.UnixMilli(fromMs).UTC().Truncate(24 * time.Hour)
	end := time.UnixMilli(toMs).UTC()
	var out []string
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		out = append(out, d.Format(store.DateLayout))
		if len(out) > 40 {
			// A span longer than this is not a broadcast; refusing to walk it is
			// cheaper than discovering why later.
			break
		}
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// DiagnoseBroadcast runs the playbook at BROADCAST scope over stored data.
//
// `Report.Scope` has documented "broadcast" since R28, but only the live path
// ever computed one — so the question "was this broadcast bad, or was that one
// viewer bad?" was answerable in real time and never afterwards. This closes
// that, using the same rule set, so a live broadcast verdict and a stored one
// come out of one engine (docs/33 §8's standing risk, and its mitigation).
func (a *API) DiagnoseBroadcast(key string, since time.Time) (*rules.Report, error) {
	rows, err := a.rollups(since)
	if err != nil {
		return nil, err
	}
	f := rules.NewFacts(key, "broadcast", "")
	var viewers, dropping int
	var worstFps float64 = -1
	var relayLines [][]byte
	seenDate := map[string]bool{}
	for _, r := range rows {
		if r.BroadcastKey != key {
			continue
		}
		if r.StartedAt > 0 {
			date := time.UnixMilli(r.StartedAt).UTC().Format(store.DateLayout)
			if !seenDate[date] {
				seenDate[date] = true
				if lines, err := a.store.ReadRelay(date); err == nil {
					relayLines = append(relayLines, lines...)
				}
			}
		}
		if r.Role == "viewer" {
			viewers++
			if st := r.Series["receivedFps"]; st != nil {
				if worstFps < 0 || st.Median < worstFps {
					worstFps = st.Median
				}
			}
			if severityOfRow(r).Rank() >= rules.SeverityWarn.Rank() {
				dropping++
			}
		}
		if r.Role == "broadcaster" {
			for _, name := range []string{"captureFps", "encoderFps", "sentFps", "encoderQueueDepth"} {
				if st := r.Series[name]; st != nil {
					f.SetClient(name, st.Median)
				}
			}
			for name, v := range r.Config {
				f.SetText(name, v)
			}
		}
	}
	if viewers == 0 {
		return nil, store.ErrNotFound
	}
	f.SetClient("viewerCount", float64(viewers))
	if worstFps >= 0 {
		f.SetClient("receivedFps", worstFps)
	}
	setBroadcastRelayFacts(f, relayLines, key)
	if dropping > 0 && viewers > 1 {
		f.Caveats = append(f.Caveats,
			fmt.Sprintf("%d of %d viewers on this broadcast were degraded or worse", dropping, viewers))
	}
	rep := rules.Evaluate(f, a.rs)
	rep.Subject = key
	if a.dashboard != "" {
		rep.DashboardURL = a.dashboard + "#/broadcast/" + key
	}
	return &rep, nil
}

// setBroadcastRelayFacts publishes the broadcast-scope relay anchors from the
// stored observations — the same names the live path derives, so a rule that
// fires live also fires here.
func setBroadcastRelayFacts(f *rules.Facts, lines [][]byte, key string) {
	var last *relayObservation
	for _, ln := range lines {
		var o relayObservation
		if err := json.Unmarshal(ln, &o); err != nil || o.BroadcastKey != key || o.Broadcast == nil || o.Role == "edge" {
			continue
		}
		if last == nil || o.AtMs > last.AtMs {
			cur := o
			last = &cur
		}
	}
	if last == nil {
		f.Caveats = append(f.Caveats,
			"no relay-side observation of this broadcast — every verdict below rests on client testimony alone (docs/33 D5/D7)")
		return
	}
	bc := last.Broadcast
	f.SetRelay("publisherActive", boolF(bc.PublisherActive))
	f.SetRelay("framesRelayed", float64(bc.FramesRelayed))
	f.SetRelay("ingressFramesLost", float64(bc.IngressFramesLost))
	f.SetRelay("viewersGlobal", float64(bc.ViewersGlobal))
	f.SetRelay("datagramsDropped", float64(bc.DatagramsDropped))
	f.SetRelay("bandwidthDroppedDatagrams", float64(bc.BandwidthDroppedDatagrams))
	f.SetRelay("keyframeStreamsIn", float64(bc.KeyframeStreamsIn))
	if total := float64(bc.IngressFramesLost + bc.FramesRelayed); total > 0 {
		f.SetRelay("ingressLossRatio", float64(bc.IngressFramesLost)/total)
	}
	var drops []float64
	dropping := 0
	for _, sub := range bc.SubscriberDetails {
		if sub.Internal {
			continue
		}
		drops = append(drops, float64(sub.Dropped))
		if sub.Dropped > 0 {
			dropping++
		}
	}
	f.SetRelay("subscribers", float64(len(drops)))
	f.SetRelay("subscribersFleetTotal", float64(len(drops)))
	f.SetRelay("subscribersDropping", float64(dropping))
	if len(drops) > 0 {
		sort.Float64s(drops)
		f.SetRelay("peerMedianDropped", drops[len(drops)/2])
	}
}
