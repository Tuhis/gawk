package serve

import (
	"context"
	"log/slog"

	"github.com/Tuhis/gawk/gawk-server/internal/popsrv"
)

// wirePopNamed wires the named tier's CSR and certificate delivery onto the PoP manager (docs/76). wirePops calls it with
// -pops on. A stub until W3-named fills it.
func wirePopNamed(ctx context.Context, w Wired, m *popsrv.Manager, log *slog.Logger) error {
	return nil
}
