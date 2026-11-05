package readapi

import (
	"strings"
	"testing"
	"time"
)

// R36 TU4: every surface that states its coverage says WHY, so the page can
// tell a range that predates the service (its age: the axis extent) from one
// truncated by raw retention (a caveat) without parsing the sentence — and
// the sentence itself, which other readers parse, does not move.
func TestEveryCoverageCallerStatesItsReason(t *testing.T) {
	type scenario struct {
		reason string
		// seedDaysAgo places the one stored session; <0 seeds nothing.
		seedDaysAgo int
		// fromDaysAgo is the requested range's start.
		fromDaysAgo int
		notePrefix  string
	}
	scenarios := []scenario{
		{reason: CoverageNone, seedDaysAgo: -1, fromDaysAgo: 7,
			notePrefix: "this service has no stored rollups at all"},
		// The service is a day old and the range asks for a week: its age.
		{reason: CoverageBeforeData, seedDaysAgo: 1, fromDaysAgo: 7,
			notePrefix: "nothing is stored before "},
		// Sixty days of rollups, 30 days of raw, a 45-day range: retention.
		{reason: CoverageRawPruned, seedDaysAgo: 60, fromDaysAgo: 45,
			notePrefix: "raw samples are kept for 30 days; rows before "},
		// Inside both edges: nothing to say.
		{reason: "", seedDaysAgo: 60, fromDaysAgo: 1},
	}
	callers := []struct {
		name string
		get  func(a *API, from time.Time, now time.Time) (Coverage, error)
	}{
		{"SearchSessions", func(a *API, from, _ time.Time) (Coverage, error) {
			p, err := a.SearchSessions(HistoryQuery{From: from})
			return covOf(p, err, func() Coverage { return p.Coverage })
		}},
		{"SearchBroadcasts", func(a *API, from, _ time.Time) (Coverage, error) {
			p, err := a.SearchBroadcasts(HistoryQuery{From: from})
			return covOf(p, err, func() Coverage { return p.Coverage })
		}},
		{"GetBroadcast", func(a *API, from, _ time.Time) (Coverage, error) {
			d, err := a.GetBroadcast(bkey, BroadcastQuery{Since: from})
			return covOf(d, err, func() Coverage { return d.Coverage })
		}},
		{"FleetTimelineOf", func(a *API, from, _ time.Time) (Coverage, error) {
			tl, err := a.FleetTimelineOf(HistoryQuery{From: from})
			return covOf(tl, err, func() Coverage { return tl.Coverage })
		}},
		{"Trends", func(a *API, from, _ time.Time) (Coverage, error) {
			tr, err := a.Trends(TrendQuery{From: from})
			return covOf(tr, err, func() Coverage { return tr.Coverage })
		}},
		{"Cohorts", func(a *API, from, now time.Time) (Coverage, error) {
			c, err := a.Cohorts(CohortQuery{AFrom: from, ATo: from.Add(time.Hour), BFrom: now.Add(-time.Hour), BTo: now})
			return covOf(c, err, func() Coverage { return c.Coverage })
		}},
	}

	for _, sc := range scenarios {
		f := newFixture(t)
		api, err := New(Options{Store: f.store, Now: func() time.Time { return f.now }, RetentionDays: 30})
		if err != nil {
			t.Fatal(err)
		}
		if sc.seedDaysAgo >= 0 {
			now := f.now
			f.now = now.AddDate(0, 0, -sc.seedDaysAgo)
			f.seed(t, "0c0000000000000000000001", "viewer", healthyViewerStats(10), nil)
			f.now = now
		}
		from := f.now.AddDate(0, 0, -sc.fromDaysAgo)
		for _, c := range callers {
			cov, err := c.get(api, from, f.now)
			if err != nil {
				t.Fatalf("%s/%q: %v", c.name, sc.reason, err)
			}
			if cov.Reason != sc.reason {
				t.Errorf("%s: reason = %q, want %q (note %q)", c.name, cov.Reason, sc.reason, cov.Note)
			}
			if (cov.Note == "") != (cov.Reason == "") || !strings.HasPrefix(cov.Note, sc.notePrefix) {
				t.Errorf("%s/%q: note = %q, want it to start %q", c.name, sc.reason, cov.Note, sc.notePrefix)
			}
		}

		// Meta asks with no range: it can only ever find no rollups at all,
		// and it states the edges, never a range's note.
		cov, err := api.coverage(HistoryQuery{})
		if err != nil {
			t.Fatal(err)
		}
		m := api.Meta()
		if m.RollupsFromMs != cov.RollupsFromMs || m.RawFromMs != cov.RawFromMs {
			t.Errorf("Meta edges %d/%d, want coverage's %d/%d", m.RollupsFromMs, m.RawFromMs, cov.RollupsFromMs, cov.RawFromMs)
		}
		if want := map[bool]string{true: CoverageNone, false: ""}[sc.seedDaysAgo < 0]; cov.Reason != want {
			t.Errorf("an unranged coverage reason = %q, want %q", cov.Reason, want)
		}
	}
}

func covOf[T any](v *T, err error, get func() Coverage) (Coverage, error) {
	if err != nil || v == nil {
		return Coverage{}, err
	}
	return get(), nil
}
