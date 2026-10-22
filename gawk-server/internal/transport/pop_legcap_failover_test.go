package transport

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
)

// failoverLocator is a link that no pod can see for a while (its holder
// drained, the new holder's status.lease not yet in this pod's cache), then
// shows up held by this pod.
type failoverLocator struct {
	PopHome
	visibleAt time.Time
	holds     atomic.Bool
}

func (f *failoverLocator) LinkHolder(string) (string, int64, bool) { return "", 0, false }
func (f *failoverLocator) HoldsLink(string) (int64, bool) {
	if time.Now().After(f.visibleAt) {
		f.holds.Store(true)
		return 3, true
	}
	return 0, false
}

func newLegCapServer(t *testing.T, loc PopHome) *Server {
	t.Helper()
	srv := New(config.Config{Addr: "127.0.0.1:0"}, hub.NewRegistry(discardLog, hub.Options{}), nil, discardLog,
		metrics.NewServerMetrics(prometheus.NewRegistry()))
	srv.SetCluster(&fakeCoordinator{leases: map[string]fakeLease{}}, "pod-self")
	srv.SetPopHome(loc)
	return srv
}

// docs/76 item 24: a link moving between pods is briefly invisible. Taking
// a slot keeps trying for popLegSlotTimeout instead of refusing the leg on
// the first look, so a failover does not refuse every leg dialled during it.
func TestLegSlotWaitsOutALinkFailover(t *testing.T) {
	loc := &failoverLocator{visibleAt: time.Now().Add(300 * time.Millisecond)}
	srv := newLegCapServer(t, loc)
	slot, err := srv.acquireFleetPopLeg(context.Background(), testPop, "K7XQ2M", 20)
	if err != nil {
		t.Fatalf("acquire across a 300 ms failover: %v", err)
	}
	slot.release()
	if !loc.holds.Load() {
		t.Fatal("the slot was taken without the link being visible")
	}
}

// With no link at all, the leg is still refused once the window ends: the
// cap fails closed.
func TestLegSlotWithNoLinkFailsClosedAfterTheWindow(t *testing.T) {
	loc := &failoverLocator{visibleAt: time.Now().Add(time.Hour)}
	srv := newLegCapServer(t, loc)
	start := time.Now()
	_, err := srv.acquireFleetPopLeg(context.Background(), testPop, "K7XQ2M", 20)
	if !errors.Is(err, errPopUnlinked) {
		t.Fatalf("acquire with no link = %v, want errPopUnlinked", err)
	}
	if d := time.Since(start); d < popLegSlotTimeout-200*time.Millisecond || d > popLegSlotTimeout+time.Second {
		t.Fatalf("refused after %v, want about %v", d, popLegSlotTimeout)
	}
}
