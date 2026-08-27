package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// BanCounter is the metrics package's slice of moderation.Set (R39 AP2,
// docs/42 §4.3). An interface so this package keeps its one-way dependency
// shape: the hub and the ban set stay free of Prometheus imports, and the
// snapshot is taken per scrape rather than mirrored into a gauge that could
// drift from the set it describes.
type BanCounter interface {
	// ActiveCounts returns the number of UNEXPIRED records per target type.
	// Evaluated at scrape time, so the gauge falls on its own when a ban
	// lapses — with or without a janitor.
	ActiveCounts(now time.Time) map[string]int
}

// ModerationCollector exposes gawk_moderation_bans_active.
type ModerationCollector struct {
	bans BanCounter
	desc *prometheus.Desc
	now  func() time.Time // injectable for tests
}

// NewModerationCollector builds the collector. A nil BanCounter (or a nil
// *moderation.Set inside it) reports zeros, which is exactly what
// -moderation-source=off means — the series exist so an operator can tell
// "no bans" from "no metric".
func NewModerationCollector(bans BanCounter) *ModerationCollector {
	return &ModerationCollector{
		bans: bans,
		desc: prometheus.NewDesc(
			"gawk_moderation_bans_active",
			"Ban records currently in force on this pod, by target type (R39).",
			[]string{"target_type"}, nil),
		now: time.Now,
	}
}

// Describe implements prometheus.Collector.
func (c *ModerationCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

// Collect implements prometheus.Collector.
func (c *ModerationCollector) Collect(ch chan<- prometheus.Metric) {
	counts := map[string]int{"broadcastId": 0, "ip": 0}
	if c.bans != nil {
		for k, v := range c.bans.ActiveCounts(c.now()) {
			counts[k] = v
		}
	}
	for targetType, n := range counts {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(n), targetType)
	}
}
