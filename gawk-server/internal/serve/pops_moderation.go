package serve

import (
	"context"
	"log/slog"

	"github.com/Tuhis/gawk/gawk-server/internal/popsrv"
)

// wirePopModeration wires ban forwarding and revoke onto the PoP manager (docs/76). wirePops calls it with
// -pops on. A stub until W3-moderation fills it.
func wirePopModeration(ctx context.Context, w Wired, m *popsrv.Manager, log *slog.Logger) error {
	return nil
}
