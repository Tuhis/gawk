package serve

import (
	"context"
	"log/slog"

	"github.com/Tuhis/gawk/gawk-server/internal/popsrv"
)

// wirePopUplink wires the home side of R74's uplink (docs/76 §6.4-6.5,
// PP8) onto the PoP manager: the reservation and want subsystem on the
// link records (reserve, release, refuse), and transport's demand hooks —
// the edge manager's onReserved and /internal/pop, /pop/publish's
// single-pod ownership check. The OnOriginClaimed kick is already wired by
// the cluster step (run.go → Server.HandleOriginClaimed), and the events'
// pop name by popOfFor in Run. wirePops calls it with -pops on.
func wirePopUplink(_ context.Context, w Wired, m *popsrv.Manager, log *slog.Logger) error {
	u := m.StartUplink(popsrv.UplinkOptions{
		Reservations:  w.Reservations,
		DemandTimeout: w.Cfg.PopDemandTimeout,
	})
	w.Server.SetPopDemand(u)
	log.Debug("pop uplink wired", "linger", w.Cfg.PopUplinkLinger, "demand_timeout", w.Cfg.PopDemandTimeout)
	return nil
}

// popOfFor is hub.Options.PopOf on a -pops home: the PoP a broadcast is
// reserved for (and so published through), for the broadcast events'
// additive pop field. res is late-bound like popIDReserved's — cluster
// mode builds the set after the hub — and nil names none. Owner is
// non-blocking, as PopOf runs under the hub's lock.
func popOfFor(res func() popsrv.Reservations) func(string) string {
	return func(id string) string {
		r := res()
		if r == nil {
			return ""
		}
		pop, _ := r.Owner(id)
		return pop
	}
}
