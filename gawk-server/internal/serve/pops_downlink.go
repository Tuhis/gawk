package serve

import (
	"context"
	"log/slog"

	"github.com/Tuhis/gawk/gawk-server/internal/popsrv"
)

// wirePopDownlink wires the downlink legs onto the PoP manager (docs/76). wirePops calls it with
// -pops on. A stub until W3-downlink fills it.
func wirePopDownlink(ctx context.Context, w Wired, m *popsrv.Manager, log *slog.Logger) error {
	return nil
}
