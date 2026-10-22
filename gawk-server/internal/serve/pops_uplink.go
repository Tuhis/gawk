package serve

import (
	"context"
	"log/slog"

	"github.com/Tuhis/gawk/gawk-server/internal/popsrv"
)

// wirePopUplink wires the uplink: EdgeManager.onReserved, the OnOriginClaimed kick and hub.Options.PopOf onto the PoP manager (docs/76). wirePops calls it with
// -pops on. A stub until W3-uplink fills it.
func wirePopUplink(ctx context.Context, w Wired, m *popsrv.Manager, log *slog.Logger) error {
	return nil
}
