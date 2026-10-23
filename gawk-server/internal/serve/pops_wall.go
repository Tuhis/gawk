package serve

import (
	"context"
	"log/slog"

	"github.com/Tuhis/gawk/gawk-server/internal/popsrv"
)

// wirePopWall wires the party wall onto the PoP manager (docs/76 §6.10):
// wall records become vouched attaches on the wall's home pod, checked
// against the fleet's reservations, and the links' extra streams carry the
// PoPs' room joins. wirePops calls it with -pops on; with home -rooms off
// there is no wall (config tells the PoP, and its rooms stay local).
func wirePopWall(ctx context.Context, w Wired, m *popsrv.Manager, log *slog.Logger) error {
	if w.Rooms == nil || w.Server == nil {
		return nil
	}
	var owner popsrv.ReservationOwner
	if w.Reservations != nil {
		owner = w.Reservations
	}
	popsrv.StartWall(ctx, m, popsrv.WallOptions{Host: w.Server, Owner: owner, Log: log})
	return nil
}
