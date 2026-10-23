package serve

import (
	"context"
	"fmt"

	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/internal/roomcluster"
	"github.com/Tuhis/gawk/gawk-server/internal/roomsrv"
	"github.com/Tuhis/gawk/gawk-server/rooms"
)

// wireRoomClusterSeams installs the registry's cluster seams (docs/44 §4.5)
// on top of a store that may not exist yet: the CR create is the code
// reservation (Unreserve gives it back on the local re-check race), the
// attach secret is read from the room's Secret per join, and the status
// writes follow the room's life. Before the store exists the gates fail
// closed and the notifications are no-ops. One function so a test can prove
// every seam is wired.
//
// It also sets UnknownIsExpired: with the broadcast source fleet-wide,
// "unknown" means no hub here and no origin lease anywhere, and the poll must
// remove the attachment because the hub's expiry hook fires on the origin
// pod, not on the room's home.
func wireRoomClusterSeams(ro *roomsrv.Options, store func() *roomcluster.Store) {
	ro.UnknownIsExpired = true
	ro.Reserve = func(ctx context.Context, room *rooms.Room) error {
		st := store()
		if st == nil {
			return roomsrv.ErrUnavailable
		}
		return st.Reserve(ctx, room)
	}
	ro.Unreserve = func(ctx context.Context, code string) {
		if st := store(); st != nil {
			st.Unreserve(ctx, code)
		}
	}
	ro.AttachSecret = func(code string) (string, bool, error) {
		st := store()
		if st == nil {
			return "", false, fmt.Errorf("%w: room store not installed", roomsrv.ErrUnavailable)
		}
		return st.AttachSecret(code)
	}
	ro.OnRoomEnded = func(code string, reason uint8) {
		if st := store(); st != nil {
			st.RoomEnded(code, reason)
		}
	}
	ro.OnRoomEmpty = func(code string, empty bool) {
		if st := store(); st != nil {
			st.RoomEmpty(code, empty)
		}
	}
	ro.OnAttachmentsChanged = func(code string, list []rooms.Attachment) {
		if st := store(); st != nil {
			st.AttachmentsChanged(code, list)
		}
	}
}

// installRooms puts the room registry on the transport and returns the rooms
// stats source for /statusz and the metrics collector (the transport's
// "proxy" rows merged over the registry's "home" rows). Nil with -rooms off.
// One function because the ORDER is the contract: the source is nil until
// the registry is installed.
func installRooms(srv roomsServer, reg *roomsrv.Registry) metrics.RoomStatsSource {
	if reg == nil {
		return nil
	}
	srv.SetRooms(reg)
	return srv.RoomStatsSource()
}

// roomsServer is the transport slice installRooms needs.
type roomsServer interface {
	SetRooms(*roomsrv.Registry)
	RoomStatsSource() metrics.RoomStatsSource
}
