package readapi

// TH7's fleet timeline and trends (docs/36 §4 Wave 3).
//
// Two halves answering two questions no other view can.
//
// **The fleet timeline** puts one row per broadcast on one shared axis, so a
// relay-wide or pod-wide event shows as a VERTICAL STRIPE across unrelated
// broadcasts. That is the only thing in the design that distinguishes "gawk had
// a bad minute" from "that broadcast had a bad minute" (Q5).
//
// **Trends** bucket the permanent rollups. Rollups are never pruned, so a trend
// query can far outrun the raw window — and the answer says so when it does
// (UD10). Everything is bucketed SERVER-side (UD4): a 30-day query must never
// ship per-session rows to a browser.

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/rollup"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/rules"
)

// MaxTimelineRows bounds the fleet timeline. Above this the vertical-stripe
// read stops working anyway — the rows are a pixel tall — so the honest move is
// to bound it and say so rather than render a smear.
const MaxTimelineRows = 300

// MaxBuckets bounds a trend query's resolution.
const MaxBuckets = 500

// FleetTimeline is one row per broadcast on one axis.
type FleetTimeline struct {
	FromMs int64              `json:"fromMs"`
	ToMs   int64              `json:"toMs"`
	Rows   []FleetTimelineRow `json:"rows"`
	// RowsOmitted is stated, never silent: a timeline that quietly drops
	// broadcasts is a timeline that can show "nothing happened" during an
	// incident.
	RowsOmitted int `json:"rowsOmitted,omitempty"`
	Coverage    `json:"coverage"`
}

// FleetTimelineRow is one broadcast's span and severity.
type FleetTimelineRow struct {
	BroadcastKey string         `json:"broadcastKey"`
	FromMs       int64          `json:"fromMs"`
	ToMs         int64          `json:"toMs"`
	Severity     rules.Severity `json:"severity"`
	Sessions     int            `json:"sessions"`
	Viewers      int            `json:"viewers"`
	Live         bool           `json:"live,omitempty"`
	// Bands are the sub-spans where a participant was degraded — per-session
	// spans merged, which is what makes a fleet-wide minute legible as a
	// vertical alignment rather than as a table of times.
	Bands []SeverityBand `json:"bands,omitempty"`
	// RollupOnly marks a row whose raw window is gone: the band detail comes
	// from the stored verdict rather than from samples.
	RollupOnly bool `json:"rollupOnly,omitempty"`
}

// SeverityBand is one degraded span inside a broadcast.
type SeverityBand struct {
	FromMs   int64          `json:"fromMs"`
	ToMs     int64          `json:"toMs"`
	Severity rules.Severity `json:"severity"`
}

// FleetTimelineOf builds TH7's first half.
func (a *API) FleetTimelineOf(q HistoryQuery) (*FleetTimeline, error) {
	rows, err := a.rollups(q.From)
	if err != nil {
		return nil, err
	}
	cov, err := a.coverage(q)
	if err != nil {
		return nil, err
	}
	out := &FleetTimeline{Coverage: cov}
	if !q.From.IsZero() {
		out.FromMs = q.From.UnixMilli()
	}
	if !q.To.IsZero() {
		out.ToMs = q.To.UnixMilli()
	} else {
		out.ToMs = a.now().UnixMilli()
	}

	byKey := map[string]*FleetTimelineRow{}
	for _, r := range rows {
		if r.StartedAt == 0 || (out.FromMs > 0 && r.EndedAt > 0 && r.EndedAt < out.FromMs) {
			continue
		}
		if r.StartedAt > out.ToMs {
			continue
		}
		row, ok := byKey[r.BroadcastKey]
		if !ok {
			row = &FleetTimelineRow{
				BroadcastKey: r.BroadcastKey, FromMs: r.StartedAt, ToMs: r.EndedAt,
				Severity: rules.SeverityOK,
			}
			byKey[r.BroadcastKey] = row
		}
		row.Sessions++
		if r.Role == "viewer" {
			row.Viewers++
		}
		if r.StartedAt < row.FromMs {
			row.FromMs = r.StartedAt
		}
		if r.EndedAt > row.ToMs {
			row.ToMs = r.EndedAt
		}
		sev := severityOfRow(r)
		if sev.Rank() > row.Severity.Rank() {
			row.Severity = sev
		}
		if cov.RawFromMs > 0 && r.StartedAt < cov.RawFromMs {
			row.RollupOnly = true
		}
		// The band is the SESSION's span, not the broadcast's: a viewer that
		// was bad for two minutes of a two-hour broadcast must not paint the
		// whole row, or every long broadcast reads as a disaster.
		if sev.Rank() >= rules.SeverityWarn.Rank() && r.EndedAt > r.StartedAt {
			row.Bands = append(row.Bands, SeverityBand{FromMs: r.StartedAt, ToMs: r.EndedAt, Severity: sev})
		}
	}
	if a.live != nil {
		now := a.now().UnixMilli()
		for _, b := range a.live.Snapshot().Live {
			row, ok := byKey[b.BroadcastKey]
			if !ok {
				row = &FleetTimelineRow{
					BroadcastKey: b.BroadcastKey, FromMs: now - b.UptimeMs, ToMs: now,
					Severity: rules.SeverityOK,
				}
				byKey[b.BroadcastKey] = row
			}
			row.Live = true
			row.ToMs = now
			row.Viewers = max(row.Viewers, b.Viewers)
			if v := maxSeverity(b.Severity, b.WorstViewer); v.Rank() > row.Severity.Rank() {
				row.Severity = v
			}
		}
	}

	for _, row := range byKey {
		row.Bands = mergeBands(row.Bands)
		out.Rows = append(out.Rows, *row)
	}
	// Worst first, then by start: the stripe an operator is looking for is at
	// the top, and rows that started together stay together.
	sort.SliceStable(out.Rows, func(i, j int) bool {
		if out.Rows[i].Severity.Rank() != out.Rows[j].Severity.Rank() {
			return out.Rows[i].Severity.Rank() > out.Rows[j].Severity.Rank()
		}
		return out.Rows[i].FromMs < out.Rows[j].FromMs
	})
	if len(out.Rows) > MaxTimelineRows {
		out.RowsOmitted = len(out.Rows) - MaxTimelineRows
		out.Rows = out.Rows[:MaxTimelineRows]
	}
	if out.FromMs == 0 {
		for _, r := range out.Rows {
			if out.FromMs == 0 || r.FromMs < out.FromMs {
				out.FromMs = r.FromMs
			}
		}
	}
	return out, nil
}

// mergeBands collapses overlapping degraded spans, keeping the worst severity
// over each merged span. Without this a broadcast with eight bad viewers draws
// eight stacked bars saying one thing.
func mergeBands(in []SeverityBand) []SeverityBand {
	if len(in) < 2 {
		return in
	}
	sort.Slice(in, func(i, j int) bool { return in[i].FromMs < in[j].FromMs })
	out := []SeverityBand{in[0]}
	for _, b := range in[1:] {
		last := &out[len(out)-1]
		if b.FromMs <= last.ToMs {
			if b.ToMs > last.ToMs {
				last.ToMs = b.ToMs
			}
			if b.Severity.Rank() > last.Severity.Rank() {
				last.Severity = b.Severity
			}
			continue
		}
		out = append(out, b)
	}
	return out
}

// --- trends ---------------------------------------------------------------

// Trend is a bucketed aggregate over the permanent rollups.
type Trend struct {
	Metric   string        `json:"metric"`
	GroupBy  string        `json:"groupBy,omitempty"`
	Stat     string        `json:"stat"`
	BucketMs int64         `json:"bucketMs"`
	Series   []TrendSeries `json:"series"`
	Coverage `json:"coverage"`
	Note     string `json:"note,omitempty"`
}

// TrendSeries is one group's line.
type TrendSeries struct {
	Group  string       `json:"group"`
	Points []TrendPoint `json:"points"`
	// Sessions is the group's total sample size across the whole range, so a
	// line drawn from three sessions cannot be mistaken for a fleet fact.
	Sessions int `json:"sessions"`
}

// TrendPoint is one bucket.
type TrendPoint struct {
	AtMs     int64   `json:"atMs"`
	Value    float64 `json:"value"`
	Sessions int     `json:"sessions"`
	// Thin marks a bucket computed from too few sessions to claim anything.
	// FleetSummary already refuses to over-claim below 5; that honesty carries
	// over rather than being re-decided per view.
	Thin bool `json:"thin,omitempty"`
}

// ThinSampleFloor is where a bucket stops being worth believing. The same
// number FleetSummary uses, because "how many sessions is enough" cannot have
// two answers on one page.
const ThinSampleFloor = 5

// TrendQuery parameterizes a trend.
type TrendQuery struct {
	From, To time.Time
	// Metric is a rollup series name (receivedFps, capToRenderMs…) or one of
	// the derived names: stalls, badShare, sessions.
	Metric string
	// Stat is median | p95. A p95 of a latency and a median of a rate answer
	// different questions and the caller says which.
	Stat string
	// GroupBy is appVersion | deliveryMode | browser | os | resolution, or
	// empty for one overall line.
	GroupBy string
	// BucketMs is the bucket width. Defaults to one that yields ~60 buckets
	// over the range, because a chart nobody can read is not an answer.
	BucketMs int64
	Role     string
}

// Trends buckets the rollups (TH7's second half).
func (a *API) Trends(q TrendQuery) (*Trend, error) {
	rows, err := a.rollups(q.From)
	if err != nil {
		return nil, err
	}
	cov, err := a.coverage(HistoryQuery{From: q.From})
	if err != nil {
		return nil, err
	}
	if q.Metric == "" {
		q.Metric = "receivedFps"
	}
	if q.Stat == "" {
		q.Stat = "median"
	}
	if q.Role == "" {
		q.Role = "viewer"
	}
	from, to := q.From, q.To
	if to.IsZero() {
		to = a.now()
	}
	if from.IsZero() {
		from = to.AddDate(0, 0, -7)
	}
	bucket := q.BucketMs
	if bucket <= 0 {
		bucket = max64(int64(to.Sub(from)/time.Millisecond)/60, int64(time.Hour/time.Millisecond))
	}
	if n := int64(to.Sub(from)/time.Millisecond) / bucket; n > MaxBuckets {
		bucket = int64(to.Sub(from)/time.Millisecond) / MaxBuckets
	}

	out := &Trend{
		Metric: q.Metric, GroupBy: q.GroupBy, Stat: q.Stat,
		BucketMs: bucket, Coverage: cov, Series: []TrendSeries{},
	}
	if cov.RawFromMs > 0 && from.UnixMilli() < cov.RawFromMs {
		// Rollups are permanent and raw is not, so a long range is answered
		// entirely from rollups. That is a feature — it is why cross-release
		// comparison works at all — but it must be SAID, because the numbers
		// available from a rollup are a subset of what a session holds.
		out.Note = "this range reaches beyond the raw window; it is answered from permanent rollups alone"
	}

	// group -> bucket index -> values
	grouped := map[string]map[int64][]float64{}
	totals := map[string]int{}
	for _, r := range rows {
		if q.Role != "any" && r.Role != q.Role {
			continue
		}
		at := r.EndedAt
		if at == 0 {
			at = r.StartedAt
		}
		if at < from.UnixMilli() || at > to.UnixMilli() {
			continue
		}
		v, ok := trendValue(r, q.Metric, q.Stat)
		if !ok {
			continue
		}
		g := "all"
		if q.GroupBy != "" {
			g = trendGroupKey(r, q.GroupBy)
		}
		b := (at - from.UnixMilli()) / bucket
		if grouped[g] == nil {
			grouped[g] = map[int64][]float64{}
		}
		grouped[g][b] = append(grouped[g][b], v)
		totals[g]++
	}

	names := make([]string, 0, len(grouped))
	for g := range grouped {
		names = append(names, g)
	}
	sort.Strings(names)
	for _, g := range names {
		s := TrendSeries{Group: g, Sessions: totals[g]}
		idx := make([]int64, 0, len(grouped[g]))
		for b := range grouped[g] {
			idx = append(idx, b)
		}
		sort.Slice(idx, func(i, j int) bool { return idx[i] < idx[j] })
		for _, b := range idx {
			vals := grouped[g][b]
			s.Points = append(s.Points, TrendPoint{
				AtMs:     from.UnixMilli() + b*bucket,
				Value:    aggregate(vals, q.Stat),
				Sessions: len(vals),
				Thin:     len(vals) < ThinSampleFloor,
			})
		}
		out.Series = append(out.Series, s)
	}
	return out, nil
}

// Cohort is TH7's A/B: two ranges or two groups, side by side.
type Cohort struct {
	Metric string    `json:"metric"`
	Stat   string    `json:"stat"`
	A      CohortArm `json:"a"`
	B      CohortArm `json:"b"`
	Delta  float64   `json:"delta"`
	Ratio  float64   `json:"ratio,omitempty"`
	// Note states how thin the comparison is. `FleetSummary` already refuses to
	// over-claim below five sessions; a cohort that hides the same weakness
	// would be worse than no cohort, because it looks like a conclusion.
	Note     string `json:"note,omitempty"`
	Coverage `json:"coverage"`
}

// CohortArm is one side of the comparison.
type CohortArm struct {
	Label    string  `json:"label"`
	Value    float64 `json:"value"`
	Sessions int     `json:"sessions"`
	Thin     bool    `json:"thin,omitempty"`
}

// CohortQuery names the two arms.
type CohortQuery struct {
	Metric, Stat, Role string
	// A and B are each a range, optionally narrowed to one group value.
	AFrom, ATo, BFrom, BTo time.Time
	GroupBy                string
	AValue, BValue         string
}

// Compare two cohorts over the permanent rollups.
func (a *API) Cohorts(q CohortQuery) (*Cohort, error) {
	if q.Metric == "" {
		q.Metric = "receivedFps"
	}
	if q.Stat == "" {
		q.Stat = "median"
	}
	if q.Role == "" {
		q.Role = "viewer"
	}
	earliest := q.AFrom
	if q.BFrom.Before(earliest) || earliest.IsZero() {
		earliest = q.BFrom
	}
	rows, err := a.rollups(earliest)
	if err != nil {
		return nil, err
	}
	cov, err := a.coverage(HistoryQuery{From: earliest})
	if err != nil {
		return nil, err
	}
	arm := func(from, to time.Time, value, label string) CohortArm {
		var vals []float64
		for _, r := range rows {
			if q.Role != "any" && r.Role != q.Role {
				continue
			}
			at := r.EndedAt
			if at == 0 {
				at = r.StartedAt
			}
			if !from.IsZero() && at < from.UnixMilli() {
				continue
			}
			if !to.IsZero() && at > to.UnixMilli() {
				continue
			}
			if value != "" && q.GroupBy != "" && trendGroupKey(r, q.GroupBy) != value {
				continue
			}
			if v, ok := trendValue(r, q.Metric, q.Stat); ok {
				vals = append(vals, v)
			}
		}
		return CohortArm{
			Label: label, Value: aggregate(vals, q.Stat),
			Sessions: len(vals), Thin: len(vals) < ThinSampleFloor,
		}
	}
	out := &Cohort{Metric: q.Metric, Stat: q.Stat, Coverage: cov}
	out.A = arm(q.AFrom, q.ATo, q.AValue, cohortLabel(q.AValue, q.AFrom, q.ATo))
	out.B = arm(q.BFrom, q.BTo, q.BValue, cohortLabel(q.BValue, q.BFrom, q.BTo))
	out.Delta = out.B.Value - out.A.Value
	if out.A.Value != 0 {
		out.Ratio = out.B.Value / out.A.Value
	}
	switch {
	case out.A.Sessions == 0 || out.B.Sessions == 0:
		out.Note = "one side of this comparison has no sessions at all — there is nothing to compare, which is not the same as no difference"
	case out.A.Thin || out.B.Thin:
		out.Note = fmt.Sprintf(
			"thin baseline: %d vs %d sessions. Below %d a difference is as likely to be who happened to watch as what changed",
			out.A.Sessions, out.B.Sessions, ThinSampleFloor)
	}
	return out, nil
}

func cohortLabel(value string, from, to time.Time) string {
	if value != "" {
		return value
	}
	switch {
	case !from.IsZero() && !to.IsZero():
		return from.UTC().Format("2006-01-02") + " → " + to.UTC().Format("2006-01-02")
	case !from.IsZero():
		return "since " + from.UTC().Format("2006-01-02")
	default:
		return "all"
	}
}

// trendValue projects one rollup row onto one metric. Derived names are
// handled explicitly rather than being looked up in Series, because "stalls" is
// not a series and silently returning nothing for it would make a legitimate
// query render as an empty chart.
func trendValue(r rollup.Row, metric, stat string) (float64, bool) {
	switch metric {
	case "stalls":
		return float64(r.Stalls), true
	case "reconnects":
		return float64(r.Reconnects), true
	case "longestStallMs":
		return r.LongestStallMs, true
	case "sessions":
		return 1, true
	case "badShare":
		if severityOfRow(r) == rules.SeverityBad {
			return 1, true
		}
		return 0, true
	case "dipEpisodes":
		primary, _ := rollup.PrimarySeries(r.Role)
		if ep := r.Episodes[primary]; ep != nil {
			return float64(ep.Count), true
		}
		return 0, false
	}
	st := r.Series[metric]
	if st == nil {
		if v, ok := r.Counters[metric]; ok {
			return v, true
		}
		if v, ok := r.Relay[metric]; ok {
			return v, true
		}
		return 0, false
	}
	if stat == "p95" {
		return st.P95, true
	}
	return st.Median, true
}

func trendGroupKey(r rollup.Row, by string) string {
	if by == "appVersion" {
		if r.AppVersion == "" {
			return "unknown"
		}
		return r.AppVersion
	}
	return groupKey(r, by)
}

// aggregate reduces a bucket. `badShare` and `sessions` are counts rather than
// distributions, and taking a median of zeroes and ones would answer a question
// nobody asked — so the caller's stat decides, and a share is a mean.
func aggregate(vals []float64, stat string) float64 {
	if len(vals) == 0 {
		return 0
	}
	switch stat {
	case "sum":
		var t float64
		for _, v := range vals {
			t += v
		}
		return t
	case "mean":
		var t float64
		for _, v := range vals {
			t += v
		}
		return t / float64(len(vals))
	case "p95":
		s := append([]float64(nil), vals...)
		sort.Float64s(s)
		idx := int(math.Ceil(0.95*float64(len(s)))) - 1
		if idx < 0 {
			idx = 0
		}
		return s[idx]
	default:
		return median(vals)
	}
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
