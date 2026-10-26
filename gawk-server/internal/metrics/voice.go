package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// VoiceCollector exposes R79's room voice gauges (docs/80):
// gawk_rooms_voice_live (voice-live rooms this pod homes) and
// gawk_room_voice_sessions{room} (registered voice sessions per voice-live
// room, keyed by the /statusz HMAC, never the code). Registered only with
// voice configured, so a relay without a bridge exports no voice series.
type VoiceCollector struct {
	src      RoomStatsSource
	live     *prometheus.Desc
	sessions *prometheus.Desc
}

// NewVoiceCollector builds the collector over the rooms stats source.
func NewVoiceCollector(src RoomStatsSource) *VoiceCollector {
	return &VoiceCollector{
		src: src,
		live: prometheus.NewDesc("gawk_rooms_voice_live",
			"Voice-live rooms this pod homes (R79).", nil, nil),
		sessions: prometheus.NewDesc("gawk_room_voice_sessions",
			"Voice sessions registered in a voice-live room this pod homes (R79).", []string{"room"}, nil),
	}
}

// Describe implements prometheus.Collector.
func (c *VoiceCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.live
	ch <- c.sessions
}

// Collect implements prometheus.Collector.
func (c *VoiceCollector) Collect(ch chan<- prometheus.Metric) {
	if c.src == nil {
		ch <- prometheus.MustNewConstMetric(c.live, prometheus.GaugeValue, 0)
		return
	}
	ch <- prometheus.MustNewConstMetric(c.live, prometheus.GaugeValue, float64(c.src.TotalStats().VoiceLive))
	for key, row := range c.src.Stats() {
		if row.Role != "home" || !row.VoiceLive {
			continue
		}
		ch <- prometheus.MustNewConstMetric(c.sessions, prometheus.GaugeValue, float64(row.VoiceSessions), key)
	}
}
