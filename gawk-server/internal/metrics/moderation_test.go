package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

type fakeBanCounter struct {
	counts map[string]int
	sawNow time.Time
}

func (f *fakeBanCounter) ActiveCounts(now time.Time) map[string]int {
	f.sawNow = now
	return f.counts
}

// R39 AP2 (docs/42 §4.3): gawk_moderation_bans_active, by target type.
func TestModerationCollector(t *testing.T) {
	bans := &fakeBanCounter{counts: map[string]int{"broadcastId": 3, "ip": 1}}
	c := NewModerationCollector(bans)
	reg := prometheus.NewRegistry()
	reg.MustRegister(c)

	const want = `
# HELP gawk_moderation_bans_active Ban records currently in force on this pod, by target type (R39).
# TYPE gawk_moderation_bans_active gauge
gawk_moderation_bans_active{target_type="broadcastId"} 3
gawk_moderation_bans_active{target_type="ip"} 1
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "gawk_moderation_bans_active"); err != nil {
		t.Fatal(err)
	}
	if bans.sawNow.IsZero() {
		t.Error("the collector did not pass a scrape-time clock to ActiveCounts")
	}
}

// With no source configured the series must still EXIST at zero: an operator
// has to be able to tell "no bans" from "no moderation metric at all".
func TestModerationCollectorReportsZerosWithoutASource(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewModerationCollector(nil))

	const want = `
# HELP gawk_moderation_bans_active Ban records currently in force on this pod, by target type (R39).
# TYPE gawk_moderation_bans_active gauge
gawk_moderation_bans_active{target_type="broadcastId"} 0
gawk_moderation_bans_active{target_type="ip"} 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "gawk_moderation_bans_active"); err != nil {
		t.Fatal(err)
	}
}

// A source that reports only one target type must not make the other series
// vanish mid-flight — a disappearing gauge reads as a scrape failure.
func TestModerationCollectorAlwaysEmitsBothTargetTypes(t *testing.T) {
	reg := prometheus.NewRegistry()
	reg.MustRegister(NewModerationCollector(&fakeBanCounter{counts: map[string]int{"ip": 2}}))

	const want = `
# HELP gawk_moderation_bans_active Ban records currently in force on this pod, by target type (R39).
# TYPE gawk_moderation_bans_active gauge
gawk_moderation_bans_active{target_type="broadcastId"} 0
gawk_moderation_bans_active{target_type="ip"} 2
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(want), "gawk_moderation_bans_active"); err != nil {
		t.Fatal(err)
	}
}

// R39 AP3 (docs/42 §4.3): the kill counter. It exists at zero from process
// start — an operator has to be able to tell "no kills" from "no metric",
// which is the same rule the bans gauge follows.
func TestModerationTerminationsCounter(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := NewServerMetrics(reg)

	const zero = `
# HELP gawk_moderation_terminations_total Broadcasts this pod terminated on an operator ban (R39, close code 4006).
# TYPE gawk_moderation_terminations_total counter
gawk_moderation_terminations_total 0
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(zero), "gawk_moderation_terminations_total"); err != nil {
		t.Fatal(err)
	}

	m.Termination()
	m.Termination()
	if got := m.TerminationCount(); got != 2 {
		t.Errorf("TerminationCount = %v, want 2", got)
	}
	const two = `
# HELP gawk_moderation_terminations_total Broadcasts this pod terminated on an operator ban (R39, close code 4006).
# TYPE gawk_moderation_terminations_total counter
gawk_moderation_terminations_total 2
`
	if err := testutil.GatherAndCompare(reg, strings.NewReader(two), "gawk_moderation_terminations_total"); err != nil {
		t.Fatal(err)
	}

	// Nil-receiver safety, like every other ServerMetrics method: the
	// transport runs unwired in tests.
	var nilMetrics *ServerMetrics
	nilMetrics.Termination()
	if got := nilMetrics.TerminationCount(); got != 0 {
		t.Errorf("nil TerminationCount = %v, want 0", got)
	}
}
