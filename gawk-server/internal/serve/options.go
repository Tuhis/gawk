package serve

import (
	"log/slog"

	"github.com/Tuhis/gawk/gawk-server/internal/cluster"
	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/internal/ops"
	"github.com/Tuhis/gawk/gawk-server/internal/popsrv"
	"github.com/Tuhis/gawk/gawk-server/internal/roomsrv"
)

// registryOptions maps the parsed config onto hub.Options. Every knob must be
// plumbed through here (flag + GAWK_* env + Helm value) — a knob that parses
// but isn't mapped is silently inert in production while wired-by-hand tests
// stay green.
func registryOptions(cfg config.Config) hub.Options {
	return hub.Options{
		MaxSubscribers:                cfg.MaxSubscribers,
		BroadcastGrace:                cfg.BroadcastGrace,
		PublisherStallTimeout:         cfg.PublisherStallTimeout,
		PublisherStallEnds:            cfg.PublisherStallEnds,
		MaxBroadcasts:                 cfg.MaxBroadcasts,
		MaxTotalSubscribers:           cfg.MaxTotalSubscribers,
		MaxBandwidthBytes:             cfg.MaxBandwidthBytes,
		MaxKeyframeBytes:              cfg.MaxKeyframeBytes,
		KeyframeWriteTimeout:          cfg.KeyframeWriteTimeout,
		DVR:                           hub.DVROptions{Window: cfg.DVRWindow, MaxBytes: cfg.DVRMaxBytes},
		DVRMaxCatchup:                 cfg.DVRMaxCatchup,
		DVRAudio:                      cfg.DVRAudio,
		LiveEdgeAudioOnReliableStream: cfg.LiveEdgeAudioOnReliableStream,
		// Also the ceiling on any subscriber's parity level.
		ParityDefault: cfg.ParityDefault,
		// The transport reads cfg directly for the dial gate and capability
		// bit; this mirror keeps the hub's options honest and the
		// carry-all-limits test complete.
		StripedDelivery: cfg.StripedDelivery,
		// The same for R73's uplink knobs: the transport advertises the
		// capability, routes carriers and sizes the receive windows from cfg;
		// the hub reads UplinkCarriers to gate the uplink /metrics series.
		UplinkCarriers:        cfg.UplinkCarriers,
		UplinkMaxBitrateBytes: cfg.UplinkMaxBitrateBytes,
		StatsKey:              cfg.StatsKey,
	}
}

// popIDReserved chains the R74 PoP reservation check onto the mint's
// IDReserved (docs/76 item 14): prev(id) || a PoP owns id, so a home mint
// never draws a reserved ID (the claim's ErrReserved redraw is the backstop
// for a reservation the cache has not seen). res is late-bound — cluster
// mode builds the set with the coordinator, after the hub — and nil answers
// false. Owner is non-blocking, as IDReserved runs under the hub's lock.
func popIDReserved(prev func(string) bool, res func() popsrv.Reservations) func(string) bool {
	return func(id string) bool {
		if prev != nil && prev(id) {
			return true
		}
		r := res()
		if r == nil {
			return false
		}
		_, owned := r.Owner(id)
		return owned
	}
}

// popReservationsFor is the reservation set a -pops home runs: the gawk/pop
// Leases in cluster mode (nil until coord exists), an in-memory set on a
// single pod, and none with -pops off.
func popReservationsFor(cfg config.Config, coord *cluster.Coordinator) popsrv.Reservations {
	switch {
	case !cfg.Pops:
		return nil
	case cfg.ClusterMode:
		if coord == nil {
			return nil
		}
		return popsrv.NewClusterReservations(coord)
	default:
		return popsrv.NewMemReservations()
	}
}

// roomOptions maps the parsed config onto roomsrv.Options — registryOptions'
// twin, under the same rule: every -room-* knob crosses here
// (TestRoomOptionsCarryAllKnobs). Wiring-only fields are set by Run.
func roomOptions(cfg config.Config) roomsrv.Options {
	return roomsrv.Options{
		EmptyGrace:      cfg.RoomEmptyGrace,
		MaxRooms:        cfg.MaxRooms,
		MaxBroadcasts:   cfg.MaxRoomBroadcasts,
		MaxParticipants: cfg.MaxRoomParticipants,
		CreateSecret:    cfg.RoomCreateSecret,
	}
}

// screeningOptions maps the parsed config onto the R40 keyframe route
// (docs/71 D1) — the production call site of -screening-token, under the
// registryOptions rule (TestScreeningOptionsCarryTheToken). -screening-notice
// needs no mapping: the transport reads cfg directly.
func screeningOptions(cfg config.Config, r *hub.Registry, log *slog.Logger) *ops.ScreeningOptions {
	return &ops.ScreeningOptions{Registry: r, Token: cfg.ScreeningToken, Log: log}
}

// limits is the configured caps as gawk_limit series (docs/61). The names
// follow the flags; 0 = unlimited, as there.
func limits(cfg config.Config) metrics.Limits {
	return metrics.Limits{
		"max_broadcasts":        float64(cfg.MaxBroadcasts),
		"max_subscribers":       float64(cfg.MaxSubscribers),
		"max_total_subscribers": float64(cfg.MaxTotalSubscribers),
		"max_bandwidth_bytes":   float64(cfg.MaxBandwidthBytes),
		"dvr_max_bytes":         float64(cfg.DVRMaxBytes),
		"max_rooms":             float64(cfg.MaxRooms),
		"max_room_broadcasts":   float64(cfg.MaxRoomBroadcasts),
		"max_room_participants": float64(cfg.MaxRoomParticipants),
		"conn_rate_limit":       cfg.ConnRateLimit,
		"conn_burst_limit":      float64(cfg.ConnBurstLimit),
	}
}
