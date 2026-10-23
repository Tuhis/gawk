package serve

import (
	"context"
	"log/slog"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/moderationsrc"
	"github.com/Tuhis/gawk/gawk-server/internal/transport"
	"github.com/Tuhis/gawk/gawk-server/moderation"
)

// wiredServer is the slice of *transport.Server that startup wiring touches.
// It is an interface so the wiring ORDER can be asserted in a test; nothing
// else in the suite can observe the startup window it guards.
type wiredServer interface {
	SetModeration(*moderation.Set)
	SetCluster(transport.ClusterCoordinator, string)
	SetRoomCluster(transport.RoomCluster, string)
	HandleBanAdded(moderation.Record)
}

// wireSubsystems installs the optional subsystems on srv in the one order
// that is safe, and returns once the ban source is running.
//
// moderationsrc.Start goes LAST. A pod cold-starting in a namespace that
// already holds Ban CRs gets Add events within milliseconds; they reach
// srv.HandleBanAdded -> terminate(), which reads the edge manager and, via
// OnBroadcastExpired, the coordinator. Started before SetCluster, that is a
// data race, and a kill in the window tears the broadcast down locally but
// never deletes its origin Lease — the rest of the fleet keeps routing
// viewers to a dead origin.
//
// buildCoord builds AND publishes the coordinator (see Run), so "cluster
// wiring is complete" is one step. buildRooms is the same seam for the
// cluster room store (nil unless -cluster-mode AND -rooms); its informer
// starts after the ban source for the same reason: its first events call
// into the transport and registry, and SetRoomCluster chains the drain hook
// onto SetCluster's, so both must be installed before anything can fire.
func wireSubsystems(
	ctx context.Context,
	cfg config.Config,
	srv wiredServer,
	bans *moderation.Set,
	log *slog.Logger,
	buildCoord func() (transport.ClusterCoordinator, string, error),
	buildRooms func() (transport.RoomCluster, string, func(), error),
) error {
	srv.SetModeration(bans)

	if cfg.ClusterMode {
		coord, podName, err := buildCoord()
		if err != nil {
			return err
		}
		srv.SetCluster(coord, podName)
	}

	var startRooms func()
	if buildRooms != nil {
		store, podName, start, err := buildRooms()
		if err != nil {
			return err
		}
		srv.SetRoomCluster(store, podName)
		startRooms = start
	}

	if err := moderationsrc.Start(ctx, moderationsrc.Options{
		Source: cfg.ModerationSource,
		Set:    bans,
		Log:    log,
		// Wired for EVERY source and independently of -cluster-mode: each
		// pod acts on its own event.
		OnBanAdded: srv.HandleBanAdded,
	}); err != nil {
		return err
	}
	if startRooms != nil {
		startRooms()
	}
	return nil
}

// chainHook runs both hooks (either may be nil).
func chainHook(a, b func(string)) func(string) {
	if a == nil {
		return b
	}
	return func(id string) {
		a(id)
		b(id)
	}
}
