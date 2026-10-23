package popsrv

import (
	"context"
	"errors"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Tuhis/gawk/gawk-server/internal/popsrc"
	"github.com/Tuhis/gawk/gawk-server/pops"
)

// renewLoop renews status.lease every LeaseRenew with a CAS on this link's
// generation (the R42 room-lease semantics, docs/76 item 12). A renewal
// that finds another generation means another pod force-took the link:
// this one is stale and closes. Each tick also renews the PoP's dormant
// reservations and re-checks the Pop, so an end date that passes while
// linked ends the link within one tick.
func (l *link) renewLoop(ctx context.Context) {
	t := time.NewTicker(l.m.opts.LeaseRenew)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.ended:
			return
		case <-t.C:
		}
		if err := l.renew(ctx); errors.Is(err, popsrc.ErrLeaseLost) {
			l.log.Info("pop link lease superseded; closing the stale link", "generation", l.gen)
			l.close(closeSuperseded, "link superseded")
			return
		} else if err != nil {
			l.log.Warn("pop link lease renewal failed", "err", err)
		}
		if r := l.m.opts.Reservations; r != nil {
			if err := r.RenewAll(ctx, l.name); err != nil {
				l.log.Warn("pop reservation renewal failed", "err", err)
			}
		}
		l.recheck()
	}
}

func (l *link) renew(ctx context.Context) error {
	at := metav1.NewTime(l.m.now())
	return l.m.opts.Status.PatchLease(ctx, l.name,
		pops.Lease{Holder: l.m.opts.Pod, Addr: l.m.opts.PodAddr, RenewedAt: &at}, l.gen)
}
