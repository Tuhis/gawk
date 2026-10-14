package api

import (
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-server/screening"
)

// docs/72 D7, D8: the timeline's bucketing, runs, pattern and suggestion,
// without a database.

var tlT0 = time.Date(2026, 10, 15, 20, 0, 0, 0, time.UTC)

const tlLabel = "MALE_GENITALIA_EXPOSED"

// tlSession is a session sampled every 5 s from tlT0 for `taken` samples,
// with the given rule (nil = an older sampler).
func tlSession(epoch string, taken int64, rule *screening.BatchRule) store.ScreeningSession {
	return store.ScreeningSession{
		PublisherEpoch: epoch, BroadcastID: "ABC234", BroadcastKey: "0123456789ab",
		SamplesTaken: taken, FirstSampleAt: tlT0, LastSampleAt: tlT0.Add(time.Duration(taken-1) * 5 * time.Second),
		Floor: 0.3, Labels: []string{tlLabel}, Mode: screening.ModeObserve, SamplerID: "gawk-sampler-0", Rule: rule,
	}
}

// tlSamples stores score at every sequence number in [from, to], 5 s apart.
func tlSamples(epoch string, from, to int64, score float64) []store.TimelineSample {
	var out []store.TimelineSample
	for seq := from; seq <= to; seq++ {
		out = append(out, store.TimelineSample{PublisherEpoch: epoch, Seq: seq,
			At: tlT0.Add(time.Duration(seq-1) * 5 * time.Second), Scores: map[string]float64{tlLabel: score}})
	}
	return out
}

var tlRule = &screening.BatchRule{Threshold: 0.5, Window: 6, WindowHits: 3}

func tlInput(sessions []store.ScreeningSession, samples []store.TimelineSample, flags ...store.ContentFlag) timelineInput {
	return timelineInput{
		from: tlT0, to: tlT0.Add(4 * time.Hour), bucket: time.Minute,
		critical: 0.7, sustainedAfter: 3 * time.Minute,
		sessions: sessions, samples: samples, flags: flags,
	}
}

func TestTimelineBuckets(t *testing.T) {
	samples := append(tlSamples("e1", 1, 3, 0.4), tlSamples("e1", 4, 4, 0.75)...) // 0:00–0:15, one bucket
	samples = append(samples, tlSamples("e1", 13, 13, 0.55)...)                   // 1:00, the next bucket
	tl := buildTimeline(tlInput([]store.ScreeningSession{tlSession("e1", 30, tlRule)}, samples))
	if len(tl.buckets) != 2 {
		t.Fatalf("buckets = %+v, want 2 (an empty bucket is omitted)", tl.buckets)
	}
	b := tl.buckets[0]
	if b.Start != stamp(tlT0) || b.Max != 0.75 || b.Stored != 4 || b.Above != 1 || b.Critical != 1 || b.Scores[tlLabel] != 0.75 {
		t.Fatalf("first bucket = %+v", b)
	}
	if b := tl.buckets[1]; b.Start != stamp(tlT0.Add(time.Minute)) || b.Above != 1 || b.Critical != 0 {
		t.Fatalf("second bucket = %+v", b)
	}
}

func TestTimelinePatterns(t *testing.T) {
	sess := []store.ScreeningSession{tlSession("e1", 1000, tlRule)}
	cases := []struct {
		name    string
		samples []store.TimelineSample
		flags   []store.ContentFlag
		pattern string
		verdict string
		ban     int
	}{
		{name: "none: nothing at the threshold", samples: tlSamples("e1", 1, 10, 0.4), pattern: patternNone},
		{name: "isolated: one short run", samples: tlSamples("e1", 1, 2, 0.6), pattern: patternIsolated, verdict: verdictNotViolation},
		{name: "flicker: three short runs",
			samples: concat(tlSamples("e1", 1, 2, 0.6), tlSamples("e1", 10, 11, 0.6), tlSamples("e1", 20, 21, 0.6)),
			pattern: patternFlicker},
		{name: "flicker: an incident that tripped twice", samples: tlSamples("e1", 1, 2, 0.6),
			flags: []store.ContentFlag{{Mode: screening.ModeObserve, Trips: 2}}, pattern: patternFlicker},
		{name: "sustained below critical", samples: tlSamples("e1", 1, 40, 0.6), pattern: patternSustained},
		{name: "sustained at critical", samples: tlSamples("e1", 1, 40, 0.8), pattern: patternSustained,
			verdict: verdictViolation, ban: 86_400},
		{name: "recurring: two sustained runs", samples: concat(tlSamples("e1", 1, 40, 0.6), tlSamples("e1", 100, 140, 0.6)),
			pattern: patternRecurring, verdict: verdictViolation, ban: 604_800},
		{name: "recurring: returned after review", samples: tlSamples("e1", 1, 2, 0.6),
			flags:   []store.ContentFlag{{Mode: screening.ModeFlag, Trips: 1, ReturnedAfterReview: true}},
			pattern: patternRecurring, verdict: verdictViolation, ban: 604_800},
		// A missing sequence number is a sample below the floor: it breaks
		// the run, so two 20-sample halves are not one sustained run.
		{name: "a gap breaks a run", samples: concat(tlSamples("e1", 1, 20, 0.6), tlSamples("e1", 22, 41, 0.6)),
			pattern: patternIsolated, verdict: verdictNotViolation},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tl := buildTimeline(tlInput(sess, tc.samples, tc.flags...))
			if tl.summary.Pattern != tc.pattern {
				t.Fatalf("pattern = %q, want %q (summary %+v)", tl.summary.Pattern, tc.pattern, tl.summary)
			}
			if tl.suggestion.Verdict != tc.verdict {
				t.Fatalf("verdict = %q, want %q", tl.suggestion.Verdict, tc.verdict)
			}
			if got := 0; tl.suggestion.BanSeconds != nil {
				got = *tl.suggestion.BanSeconds
				if got != tc.ban {
					t.Fatalf("ban = %d, want %d", got, tc.ban)
				}
			} else if tc.ban != 0 {
				t.Fatalf("no ban suggested, want %d", tc.ban)
			}
		})
	}
}

func TestTimelineSummaryNumbers(t *testing.T) {
	// 40 samples at 0.8 (a 200 s run), then 20 at 0.55 after a gap.
	samples := concat(tlSamples("e1", 1, 40, 0.8), tlSamples("e1", 50, 69, 0.55))
	tl := buildTimeline(tlInput([]store.ScreeningSession{tlSession("e1", 100, tlRule)}, samples))
	s := tl.summary
	if !s.ThresholdKnown || s.Runs != 2 || s.SustainedRuns != 1 || s.LongestRunSeconds != 200 ||
		s.AboveSeconds != 300 || s.CriticalSeconds != 200 || s.SampledSeconds != 500 || s.Peak != 0.8 || s.PeakLabel != tlLabel {
		t.Fatalf("summary = %+v", s)
	}
}

// D1: with no session reporting a rule the timeline knows no threshold, and
// says so instead of guessing a pattern.
func TestTimelineWithoutARule(t *testing.T) {
	tl := buildTimeline(tlInput([]store.ScreeningSession{tlSession("e1", 100, nil)}, tlSamples("e1", 1, 40, 0.9)))
	if tl.summary.ThresholdKnown || tl.summary.Pattern != patternUnknown || tl.suggestion.Verdict != "" {
		t.Fatalf("without a rule: summary %+v, suggestion %+v", tl.summary, tl.suggestion)
	}
	if b := tl.buckets[0]; b.Above != 0 || b.Max != 0.9 {
		t.Fatalf("bucket without a rule = %+v; want scores but no above count", b)
	}
	if tl.summary.Peak != 0.9 || tl.summary.CriticalSeconds != 200 {
		t.Fatalf("critical time is still known without a threshold: %+v", tl.summary)
	}
}

func TestTimelineBucketSize(t *testing.T) {
	for _, tc := range []struct {
		span, asked, want time.Duration
	}{
		{4 * time.Hour, 0, 48 * time.Second},             // ~300 across the range
		{10 * time.Minute, 0, 5 * time.Second},           // never below 5 s
		{48 * time.Hour, time.Second, 116 * time.Second}, // capped at 1 500 buckets
		{time.Hour, 30 * time.Second, 30 * time.Second},  // honoured
	} {
		if got := bucketSize(tc.span, tc.asked); got != tc.want {
			t.Errorf("bucketSize(%v, %v) = %v, want %v", tc.span, tc.asked, got, tc.want)
		}
	}
}

func concat(parts ...[]store.TimelineSample) []store.TimelineSample {
	var out []store.TimelineSample
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}
