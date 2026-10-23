package serve

import (
	"context"
	"log/slog"

	"github.com/Tuhis/gawk/gawk-server/internal/popsrv"
)

// wirePopNamed turns on the named tier's link records (docs/76 §6.11 item
// 38): a PoP's CSR reaches status.tls.csr, and the chain gawk-admin issues
// for it goes back down the link. It costs nothing for a Pop without the
// named address: gawk-admin never assigns it a host, so its CSRs are
// ignored. wirePops calls it with -pops on.
func wirePopNamed(ctx context.Context, w Wired, m *popsrv.Manager, log *slog.Logger) error {
	stop := m.EnableNamed()
	go func() {
		<-ctx.Done()
		stop()
	}()
	return nil
}
