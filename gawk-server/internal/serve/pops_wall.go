package serve

import (
	"context"
	"log/slog"

	"github.com/Tuhis/gawk/gawk-server/internal/popsrv"
)

// wirePopWall wires the party wall's vouched attaches onto the PoP manager (docs/76). wirePops calls it with
// -pops on. A stub until W3-wall fills it.
func wirePopWall(ctx context.Context, w Wired, m *popsrv.Manager, log *slog.Logger) error { return nil }
