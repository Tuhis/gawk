package serve

import (
	"context"
	"log/slog"
	"reflect"

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
	if !setDemandDeferrer(w.Server, offers) {
		log.Warn("pop offer deferral not wired: the relay has no demand deferrer seam", "pop_offer_defer", w.Cfg.PopOfferDefer)
	}
	return nil
}

// setDemandDeferrer calls target.SetDemandDeferrer(d) when target has it.
// The setter's parameter is transport's unexported demandDeferrer
// interface (W3-uplink's pop_demand.go, contract §4), which this package
// cannot name, so the call is made by reflection: it compiles whether or
// not the seam exists and checks d implements the parameter type. Replace
// it with a direct w.Server.SetDemandDeferrer(offers) once both land.
func setDemandDeferrer(target, d any) bool {
	if target == nil || d == nil {
		return false
	}
	meth := reflect.ValueOf(target).MethodByName("SetDemandDeferrer")
	if !meth.IsValid() {
		return false
	}
	mt := meth.Type()
	if mt.NumIn() != 1 || mt.NumOut() != 0 || !reflect.TypeOf(d).AssignableTo(mt.In(0)) {
		return false
	}
	meth.Call([]reflect.Value{reflect.ValueOf(d)})
	return true
}
