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
