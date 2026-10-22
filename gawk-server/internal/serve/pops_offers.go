package serve

import (
	"context"
	"log/slog"

	"github.com/Tuhis/gawk/gawk-server/internal/popsrv"
)

// wirePopOffers wires LocalRelayOffer (0x19) and the lr=1 demand deferral onto the PoP manager (docs/76). wirePops calls it with
// -pops on. A stub until W3-offers fills it.
func wirePopOffers(ctx context.Context, w Wired, m *popsrv.Manager, log *slog.Logger) error {
	return nil
}
