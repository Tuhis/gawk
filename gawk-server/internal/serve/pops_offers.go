package serve

import (
	"context"
	"log/slog"

	"github.com/Tuhis/gawk/gawk-server/internal/popsrv"
)

// wirePopOffers wires LocalRelayOffer (0x19) and the lr=1 demand deferral
// onto the PoP manager (docs/76 items 21, 41-42). wirePops calls it with
// -pops on: every pod offers, since a guest's session can land on any of
// them, and the matcher reads the links other pods hold from status.link.
func wirePopOffers(ctx context.Context, w Wired, m *popsrv.Manager, log *slog.Logger) error {
	opts := popsrv.OffersOptions{Defer: w.Cfg.PopOfferDefer, Log: log}
	if w.Prom != nil { // a nil *Registry in the interface would not be nil
		opts.Prom = w.Prom
	}
	offers := popsrv.NewOffers(m, opts)
	w.Server.SetOfferer(offers)
	w.Server.SetDemandDeferrer(offers)
	return nil
}
