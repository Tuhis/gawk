package api

import (
	"math"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-server/screening"
)

// R70's broadcast moderation timeline (docs/72 D7, D8): one read that draws
// the portal's drill-down — the broadcast's sampled sessions with the rule
// each ran with, its stored samples bucketed, its incidents and moderation
// events, and a pattern and suggestion computed here, once, so the phone and
// the desktop never disagree. Nothing here acts: a suggestion pre-selects a
// choice, and a person confirms every action.

// SchemaTimeline names the GET /api/v1/screening/timeline response.
const SchemaTimeline = "gawk.screening.timeline.v1"

// Timeline range and bucket limits (D7).
const (
	timelineDefaultRange = 24 * time.Hour
	timelineMaxRange     = 48 * time.Hour
	timelineMinBucket    = 5 * time.Second
	timelineMaxBuckets   = 1500
	timelineAutoBuckets  = 300
)

// Patterns (D8).
const (
	patternNone      = "none"
	patternIsolated  = "isolated"
	patternFlicker   = "flicker"
	patternSustained = "sustained"
	patternRecurring = "recurring"
	// patternUnknown: there are samples, but no session reported its
	// threshold (a sampler older than R70), so runs cannot be told.
	patternUnknown = "unknown"
)

// Suggested verdicts (D8).
const (
	verdictViolation    = "violation"
	verdictNotViolation = "not_violation"
)

// Suggested ban lengths (D8).
const (
	suggestBanRecurring = 7 * 24 * time.Hour
	suggestBanSustained = 24 * time.Hour
)

// ---------------------------------------------------------------- wire shapes

type timelineJSON struct {
	Schema       string `json:"schema"`
	BroadcastKey string `json:"broadcastKey"`
	// BroadcastID is the newest session's raw ID; empty when nothing was
	// ever sampled for the key.
	BroadcastID string `json:"broadcastId,omitempty"`
	// Live is true while the fleet scan sees the broadcast.
	Live          bool                   `json:"live"`
	From          string                 `json:"from"`
	To            string                 `json:"to"`
	BucketSeconds int                    `json:"bucketSeconds"`
	Truncated     bool                   `json:"truncated"`
	Levels        timelineLevelsJSON     `json:"levels"`
	Labels        []string               `json:"labels"`
	Sessions      []timelineSessionJSON  `json:"sessions"`
	Buckets       []timelineBucketJSON   `json:"buckets"`
	Incidents     []contentFlagJSON      `json:"incidents"`
	Events        []timelineEventJSON    `json:"events"`
	Summary       timelineSummaryJSON    `json:"summary"`
	Suggestion    timelineSuggestionJSON `json:"suggestion"`
	Links         *watchLinkJSON         `json:"links,omitempty"`
}

// timelineLevelsJSON is every level the view draws (docs/72 §4). Threshold
// and the window rule are the newest session's that reported one; each
// session carries its own as well.
type timelineLevelsJSON struct {
	Floor                 float64  `json:"floor"`
	Threshold             *float64 `json:"threshold,omitempty"`
	Window                *int     `json:"window,omitempty"`
	WindowHits            *int     `json:"windowHits,omitempty"`
	Critical              float64  `json:"critical"`
	SustainedAfterSeconds int      `json:"sustainedAfterSeconds"`
	RejoinSeconds         int      `json:"rejoinSeconds"`
}

type timelineSessionJSON struct {
	PublisherEpoch string   `json:"publisherEpoch"`
	BroadcastID    string   `json:"broadcastId"`
	FirstSampleAt  string   `json:"firstSampleAt"`
	LastSampleAt   string   `json:"lastSampleAt"`
	SamplesTaken   int64    `json:"samplesTaken"`
	SamplesStored  int64    `json:"samplesStored"`
	Floor          float64  `json:"floor"`
	Threshold      *float64 `json:"threshold,omitempty"`
	Window         *int     `json:"window,omitempty"`
	WindowHits     *int     `json:"windowHits,omitempty"`
	Mode           string   `json:"mode"`
	SamplerID      string   `json:"samplerId"`
}

// timelineBucketJSON is one time bucket that holds stored samples. A bucket
// inside a session's span with none is "sampled, below the floor"; outside
// every span, "not sampled".
type timelineBucketJSON struct {
	Start string `json:"start"`
	// Max is the highest score among the flagged labels; Scores the
	// highest per label.
	Max    float64            `json:"max"`
	Scores map[string]float64 `json:"scores"`
	// Stored counts the bucket's stored samples, Above those at or above
	// their session's threshold, Critical those at or above the critical
	// level.
	Stored   int `json:"stored"`
	Above    int `json:"above"`
	Critical int `json:"critical"`
}

type timelineEventJSON struct {
	ID         int64  `json:"id"`
	Type       string `json:"type"`
	OccurredAt string `json:"occurredAt"`
	Actor      string `json:"actor"`
	Summary    string `json:"summary"`
}

type timelineSummaryJSON struct {
	Pattern        string `json:"pattern"`
	ThresholdKnown bool   `json:"thresholdKnown"`
	// The durations are sample counts times each session's mean interval.
	SampledSeconds    int     `json:"sampledSeconds"`
	AboveSeconds      int     `json:"aboveSeconds"`
	CriticalSeconds   int     `json:"criticalSeconds"`
	LongestRunSeconds int     `json:"longestRunSeconds"`
	Runs              int     `json:"runs"`
	SustainedRuns     int     `json:"sustainedRuns"`
	Peak              float64 `json:"peak"`
	PeakLabel         string  `json:"peakLabel,omitempty"`
}

type timelineSuggestionJSON struct {
	// Verdict is violation, not_violation, or empty for no suggestion.
	Verdict string `json:"verdict,omitempty"`
	// BanSeconds is the suggested ban length with a violation.
	BanSeconds *int `json:"banSeconds,omitempty"`
	// Why says what the suggestion rests on, in one sentence.
	Why string `json:"why,omitempty"`
}

// ------------------------------------------------------------- computation

type timelineInput struct {
	from, to       time.Time
	bucket         time.Duration
	critical       float64
	sustainedAfter time.Duration
	sessions       []store.ScreeningSession
	samples        []store.TimelineSample
	flags          []store.ContentFlag
}

type timelineResult struct {
	buckets    []timelineBucketJSON
	summary    timelineSummaryJSON
	suggestion timelineSuggestionJSON
}

// bucketSize picks the bucket: the caller's, or about timelineAutoBuckets
// across the span, never under timelineMinBucket and never more than
// timelineMaxBuckets across the span; whole seconds.
func bucketSize(span, asked time.Duration) time.Duration {
	b := asked
	if b <= 0 {
		b = span / timelineAutoBuckets
	}
	if floor := time.Duration(math.Ceil(float64(span)/timelineMaxBuckets/float64(time.Second))) * time.Second; b < floor {
		b = floor
	}
	if b < timelineMinBucket {
		b = timelineMinBucket
	}
	return b.Truncate(time.Second)
}

// sessionInterval is a session's mean sample interval: its sampled span over
// the gaps between its samples. A one-sample session counts the default 5 s.
func sessionInterval(s store.ScreeningSession) time.Duration {
	if s.SamplesTaken < 2 || !s.LastSampleAt.After(s.FirstSampleAt) {
		return 5 * time.Second
	}
	return s.LastSampleAt.Sub(s.FirstSampleAt) / time.Duration(s.SamplesTaken-1)
}

func sampleMax(scores map[string]float64) (float64, string) {
	max, label := 0.0, ""
	keys := make([]string, 0, len(scores))
	for l := range scores {
		keys = append(keys, l)
	}
	sort.Strings(keys) // ties resolve the same way every time
	for _, l := range keys {
		if v := scores[l]; v > max {
			max, label = v, l
		}
	}
	return max, label
}

func buildTimeline(in timelineInput) timelineResult {
	type sessInfo struct {
		threshold float64
		known     bool
		interval  time.Duration
	}
	info := map[string]sessInfo{}
	var sum timelineSummaryJSON
	for _, s := range in.sessions {
		si := sessInfo{interval: sessionInterval(s)}
		if s.Rule != nil {
			si.threshold, si.known = s.Rule.Threshold, true
			sum.ThresholdKnown = true
		}
		info[s.PublisherEpoch] = si
		lo, hi := s.FirstSampleAt, s.LastSampleAt.Add(si.interval)
		if lo.Before(in.from) {
			lo = in.from
		}
		if hi.After(in.to) {
			hi = in.to
		}
		if hi.After(lo) {
			sum.SampledSeconds += int(hi.Sub(lo).Seconds())
		}
	}

	buckets := map[int64]*timelineBucketJSON{}
	var order []int64
	// Runs per session: consecutive sequence numbers at or above the
	// session's threshold. Samples arrive ordered by time; sort each
	// session's by sequence for the run walk.
	bySession := map[string][]store.TimelineSample{}
	for _, sm := range in.samples {
		si := info[sm.PublisherEpoch]
		max, label := sampleMax(sm.Scores)
		if max > sum.Peak {
			sum.Peak, sum.PeakLabel = max, label
		}
		idx := int64(sm.At.Sub(in.from) / in.bucket)
		b := buckets[idx]
		if b == nil {
			b = &timelineBucketJSON{Start: stamp(in.from.Add(time.Duration(idx) * in.bucket)), Scores: map[string]float64{}}
			buckets[idx] = b
			order = append(order, idx)
		}
		b.Stored++
		if max > b.Max {
			b.Max = max
		}
		for l, v := range sm.Scores {
			if v > b.Scores[l] {
				b.Scores[l] = v
			}
		}
		if si.known && max >= si.threshold {
			b.Above++
			sum.AboveSeconds += int(si.interval.Seconds())
		}
		if max >= in.critical {
			b.Critical++
			sum.CriticalSeconds += int(si.interval.Seconds())
		}
		bySession[sm.PublisherEpoch] = append(bySession[sm.PublisherEpoch], sm)
	}
	slices.Sort(order)
	out := timelineResult{buckets: make([]timelineBucketJSON, 0, len(order))}
	for _, idx := range order {
		out.buckets = append(out.buckets, *buckets[idx])
	}

	for epoch, list := range bySession {
		si := info[epoch]
		if !si.known {
			continue
		}
		sort.Slice(list, func(i, j int) bool { return list[i].Seq < list[j].Seq })
		run, prev := 0, int64(-1)
		flush := func() {
			if run == 0 {
				return
			}
			d := time.Duration(run) * si.interval
			sum.Runs++
			if d >= in.sustainedAfter {
				sum.SustainedRuns++
			}
			if s := int(d.Seconds()); s > sum.LongestRunSeconds {
				sum.LongestRunSeconds = s
			}
			run = 0
		}
		for _, sm := range list {
			max, _ := sampleMax(sm.Scores)
			if max < si.threshold || (run > 0 && sm.Seq != prev+1) {
				flush()
			}
			if max >= si.threshold {
				run++
			}
			prev = sm.Seq
		}
		flush()
	}

	returned, multiTrip := false, false
	for _, f := range in.flags {
		if f.Mode == screening.ModeFlag && f.ReturnedAfterReview {
			returned = true
		}
		if f.Trips >= 2 {
			multiTrip = true
		}
	}
	switch {
	case !sum.ThresholdKnown && len(in.samples) > 0:
		sum.Pattern = patternUnknown
	case sum.SustainedRuns >= 2 || returned:
		sum.Pattern = patternRecurring
	case sum.SustainedRuns == 1:
		sum.Pattern = patternSustained
	case sum.Runs >= 3 || (multiTrip && sum.Runs > 0):
		sum.Pattern = patternFlicker
	case sum.Runs > 0:
		sum.Pattern = patternIsolated
	default:
		sum.Pattern = patternNone
	}
	out.summary = sum
	out.suggestion = suggest(sum, in.critical)
	return out
}

// suggest is D8's table. It pre-selects a choice; it never acts.
func suggest(s timelineSummaryJSON, critical float64) timelineSuggestionJSON {
	ban := func(d time.Duration) *int {
		v := int(d.Seconds())
		return &v
	}
	switch {
	case s.Pattern == patternRecurring:
		return timelineSuggestionJSON{Verdict: verdictViolation, BanSeconds: ban(suggestBanRecurring),
			Why: "the signal came back: two sustained runs, or a return after review"}
	case s.Pattern == patternSustained && s.Peak >= critical:
		return timelineSuggestionJSON{Verdict: verdictViolation, BanSeconds: ban(suggestBanSustained),
			Why: "one sustained run that reached the critical level"}
	case s.Pattern == patternIsolated:
		return timelineSuggestionJSON{Verdict: verdictNotViolation,
			Why: "a short, isolated signal — often a game scene"}
	}
	return timelineSuggestionJSON{}
}

// ---------------------------------------------------------------- handler

// handleScreeningTimeline is GET /api/v1/screening/timeline (docs/72 D7).
func (a *API) handleScreeningTimeline(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	key := q.Get("key")
	if !validBroadcastKey(key) {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "key must be a broadcast key (lower-case hex)")
		return
	}
	now := a.now()
	to, err := parseTimelineTime(q.Get("to"), now)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "to: "+err.Error())
		return
	}
	var askedBucket time.Duration
	if raw := q.Get("bucket"); raw != "" {
		secs, err := strconv.Atoi(raw)
		if err != nil || secs <= 0 {
			writeError(w, http.StatusBadRequest, CodeBadRequest, "bucket must be a positive number of seconds")
			return
		}
		askedBucket = time.Duration(secs) * time.Second
	}
	ctx := r.Context()

	from := to.Add(-timelineDefaultRange)
	explicitFrom := q.Get("from") != ""
	if explicitFrom {
		if from, err = parseTimelineTime(q.Get("from"), now); err != nil {
			writeError(w, http.StatusBadRequest, CodeBadRequest, "from: "+err.Error())
			return
		}
	}
	if !from.Before(to) || to.Sub(from) > timelineMaxRange {
		writeError(w, http.StatusBadRequest, CodeBadRequest, "from must be before to, at most 48h apart")
		return
	}
	sessions, err := a.opts.Store.ScreeningSessionsForKey(ctx, key, from, to)
	if err != nil {
		a.fail(w, r, "read the screening sessions", err)
		return
	}
	// Without an explicit from, the range starts at the first sampled
	// session in the last day, so a two-hour stream fills the chart.
	if !explicitFrom && len(sessions) > 0 && sessions[0].FirstSampleAt.After(from) {
		from = sessions[0].FirstSampleAt
		if to.Sub(from) < 10*time.Minute {
			from = to.Add(-10 * time.Minute)
		}
	}
	bucket := bucketSize(to.Sub(from), askedBucket)
	from = from.Truncate(bucket)

	samples, truncated, err := a.opts.Store.TimelineSamples(ctx, key, from, to)
	if err != nil {
		a.fail(w, r, "read the samples", err)
		return
	}
	ids := []string{}
	for _, s := range sessions {
		if !slices.Contains(ids, s.BroadcastID) {
			ids = append(ids, s.BroadcastID)
		}
	}
	flags, err := a.opts.Store.ContentFlagsForKey(ctx, key, ids, from, to)
	if err != nil {
		a.fail(w, r, "read the content flags", err)
		return
	}
	evs, err := a.opts.Store.EventsForBroadcast(ctx, key, ids, from, to)
	if err != nil {
		a.fail(w, r, "read the events", err)
		return
	}
	mutes, err := a.opts.Store.ActiveMutes(ctx, now)
	if err != nil {
		a.log.Warn("reading mutes for the timeline failed", "err", err)
	}

	cfg := a.opts.Config
	res := buildTimeline(timelineInput{
		from: from, to: to, bucket: bucket, critical: cfg.ScreeningCritical, sustainedAfter: cfg.ScreeningSustainedAfter,
		sessions: sessions, samples: samples, flags: flags,
	})
	out := timelineJSON{
		Schema: SchemaTimeline, BroadcastKey: key, From: stamp(from), To: stamp(to),
		BucketSeconds: int(bucket.Seconds()), Truncated: truncated,
		Levels: timelineLevelsJSON{
			Critical: cfg.ScreeningCritical, SustainedAfterSeconds: int(cfg.ScreeningSustainedAfter.Seconds()),
			RejoinSeconds: int(cfg.ContentFlagRejoin.Seconds()),
		},
		Labels: []string{}, Sessions: []timelineSessionJSON{}, Buckets: res.buckets,
		Incidents: []contentFlagJSON{}, Events: []timelineEventJSON{},
		Summary: res.summary, Suggestion: res.suggestion,
	}
	for _, s := range sessions {
		js := timelineSessionJSON{
			PublisherEpoch: s.PublisherEpoch, BroadcastID: s.BroadcastID,
			FirstSampleAt: stamp(s.FirstSampleAt), LastSampleAt: stamp(s.LastSampleAt),
			SamplesTaken: s.SamplesTaken, SamplesStored: s.SamplesStored, Floor: s.Floor, Mode: s.Mode, SamplerID: s.SamplerID,
		}
		if s.Rule != nil {
			th, win, hits := s.Rule.Threshold, s.Rule.Window, s.Rule.WindowHits
			js.Threshold, js.Window, js.WindowHits = &th, &win, &hits
			out.Levels.Threshold, out.Levels.Window, out.Levels.WindowHits = &th, &win, &hits
		}
		if s.Floor > out.Levels.Floor {
			out.Levels.Floor = s.Floor
		}
		for _, l := range s.Labels {
			if !slices.Contains(out.Labels, l) {
				out.Labels = append(out.Labels, l)
			}
		}
		out.BroadcastID = s.BroadcastID
		out.Sessions = append(out.Sessions, js)
	}
	for _, f := range flags {
		out.Incidents = append(out.Incidents, a.renderFlag(f, mutes))
	}
	for _, e := range evs {
		out.Events = append(out.Events, timelineEventJSON{ID: e.ID, Type: e.Type, OccurredAt: stamp(e.OccurredAt),
			Actor: e.Actor, Summary: e.PayloadString(store.PayloadSummary)})
	}
	if a.opts.Fleet != nil && out.BroadcastID != "" {
		if snap, err := a.opts.Fleet.Snapshot(ctx); err == nil {
			_, out.Live = snap.Broadcast(out.BroadcastID)
		}
	}
	if base := cfg.AppBaseURL; base != "" && out.BroadcastID != "" {
		out.Links = &watchLinkJSON{Watch: base + "/#/view/" + out.BroadcastID}
	}
	writeJSON(w, http.StatusOK, out)
}

func parseTimelineTime(raw string, def time.Time) (time.Time, error) {
	if raw == "" {
		return def.UTC(), nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

// validBroadcastKey accepts what the relay mints: 1–64 lower-case hex.
func validBroadcastKey(k string) bool {
	if len(k) == 0 || len(k) > screening.MaxBroadcastKeyLen {
		return false
	}
	for i := 0; i < len(k); i++ {
		c := k[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
