package metrics

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/Tuhis/gawk/gawk-server/internal/roomsrv"
)

// RoomStatsSource is the metrics package's slice of roomsrv.Registry (R42,
// docs/44 §4.10). An interface so the room registry stays free of
// Prometheus imports and the snapshot is taken per scrape.
type RoomStatsSource interface {
	Stats() map[string]roomsrv.RoomStats
	TotalStats() roomsrv.Totals
}

// RoomCollector exposes the room gauges: gawk_rooms_live{kind},
// gawk_room_participants{room}, gawk_room_attachments{room} and
// gawk_room_proxied_sessions. Rooms are labelled by the same HMAC'd key
// /statusz uses (docs/44 D16). Registered only with -rooms on, so a relay
// without rooms exposes no room series at all.
type RoomCollector struct {
	src          RoomStatsSource
	live         *prometheus.Desc
	participants *prometheus.Desc
	attachments  *prometheus.Desc
	proxied      *prometheus.Desc
}

// NewRoomCollector builds the collector.
func NewRoomCollector(src RoomStatsSource) *RoomCollector {
	return &RoomCollector{
		src: src,
		live: prometheus.NewDesc("gawk_rooms_live",
			"Rooms this pod holds, by kind (R42).", []string{"kind"}, nil),
		participants: prometheus.NewDesc("gawk_room_participants",
			"Control sessions in a room this pod holds (R42).", []string{"room"}, nil),
		attachments: prometheus.NewDesc("gawk_room_attachments",
			"Broadcasts attached to a room this pod holds (R42).", []string{"room"}, nil),
		proxied: prometheus.NewDesc("gawk_room_proxied_sessions",
			"Room control sessions this pod forwards to another pod's home room (R42 cluster mode).", nil, nil),
	}
}

// Describe implements prometheus.Collector.
func (c *RoomCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.live
	ch <- c.participants
	ch <- c.attachments
	ch <- c.proxied
}

// Collect implements prometheus.Collector.
func (c *RoomCollector) Collect(ch chan<- prometheus.Metric) {
	tot := c.src.TotalStats()
	ch <- prometheus.MustNewConstMetric(c.live, prometheus.GaugeValue, float64(tot.Static), "static")
	ch <- prometheus.MustNewConstMetric(c.live, prometheus.GaugeValue, float64(tot.Dynamic), "dynamic")
	proxied := 0
	for key, row := range c.src.Stats() {
		if row.Role == "proxy" {
			proxied += row.Participants
			continue
		}
		ch <- prometheus.MustNewConstMetric(c.participants, prometheus.GaugeValue, float64(row.Participants), key)
		ch <- prometheus.MustNewConstMetric(c.attachments, prometheus.GaugeValue, float64(row.Attachments), key)
	}
	ch <- prometheus.MustNewConstMetric(c.proxied, prometheus.GaugeValue, float64(proxied))
}
