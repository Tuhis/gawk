package serve

import (
	"context"
	"log/slog"

	"github.com/Tuhis/gawk/gawk-server/internal/popsrv"
)

// wirePopModeration wires the moderation reach onto the PoP manager
// (docs/76 §6.9): the forwarder learns the linked PoPs' broadcasts, is
// chained on the transport's ban callback (so every record the ban source
// applies reaches it, on every resync) and releases a revoked, deleted or
// expired PoP's reservations. wirePops calls it with -pops on, after
// wireSubsystems has started the ban source; a record applied in between is
// caught by the forwarder's sweep, which re-reads the ban set.
func wirePopModeration(ctx context.Context, w Wired, m *popsrv.Manager, log *slog.Logger) error {
	var res popsrv.ModerationReservations
	if w.Reservations != nil {
		res = w.Reservations
	}
	fw := m.EnableModeration(ctx, popsrv.ModerationOptions{Bans: w.Bans, Reservations: res})
	w.Server.AddBanObserver(fw.HandleBan)
	log.Debug("pop moderation reach wired", "reservations", res != nil)
	return nil
}
