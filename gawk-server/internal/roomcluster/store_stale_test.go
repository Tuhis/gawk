package roomcluster

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Tuhis/gawk/gawk-server/rooms"
)

// An informer hands back what it has, not what is true now. Its initial list
// can predate this pod's own adopt, a watch event can arrive late, and a
// relist after a watch error replays whatever the API server had at that
// moment. So `observe` sees views of a room that are OLDER than the lease
// this pod is holding, and the question these tests pin is what it may
// conclude from one.
//
// It must conclude nothing. Dropping a held lease on a stale view cancels the
// renew loop and fires OnLeaseLost, so the pod abandons a room it owns, for
// no reason and with nothing to put it back: R42's fencing then hands the
// room to whoever takes it next. Generations are the ordering — every take is
// `home.Generation + 1` (docs/44 §4.5) — so a view older than ours is
// recognisable, and the renew loop's own read is authoritative for everything
// a view cannot settle.
//
// Found on 2026-09-20 through TestRenewLoopKeepsTheLeaseLive, which timed out
// on CI (run 35539921560) and reproduces under GOMAXPROCS=1: the informer
// delivered its pre-adopt list entry after Adopt returned, the store declared
// the lease lost, and no renew ever happened.
func TestAStaleInformerViewDoesNotDropAHeldLease(t *testing.T) {
	// Seeded with pod-b's abandoned lease at generation 1, so this pod's own
	// take is generation 2 and there are older views to feed in.
	for _, tc := range []struct {
		name string
		view *rooms.Lease
	}{{
		// What the informer listed before the room had any lease at all.
		name: "no lease yet",
		view: nil,
	}, {
		// The pre-adopt view: pod-b still holding, one generation back.
		name: "an older generation, another holder",
		view: &rooms.Lease{Holder: "pod-b", Addr: "pod-b:4433", Generation: 1},
	}, {
		// A view of our own take, one generation stale — what a relist
		// replays after a watch error.
		name: "an older generation, ours",
		view: &rooms.Lease{Holder: "pod-a", Addr: "pod-a:4433", Generation: 1},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			a := heldRoom(t)
			a.observe(nil, roomObject(t, dynamicCR("k7xq2m", tc.view, nil)))

			a.mu.Lock()
			_, held := a.held["k7xq2m"]
			a.mu.Unlock()
			if !held {
				t.Error("a stale view dropped a lease this pod holds")
			}
			select {
			case code := <-a.lost:
				t.Errorf("OnLeaseLost fired for %q on a stale view", code)
			default:
			}
		})
	}
}

// The fence itself still has to work: a view NEWER than ours means the room
// was force-taken, and this pod must stop renewing immediately rather than
// fight the new home (docs/44 §4.5).
func TestAForceTakeStillDropsTheLease(t *testing.T) {
	a := heldRoom(t)
	taken := &rooms.Lease{Holder: "pod-b", Addr: "pod-b:4433", Generation: 99}
	a.observe(nil, roomObject(t, dynamicCR("k7xq2m", taken, nil)))

	a.mu.Lock()
	_, held := a.held["k7xq2m"]
	a.mu.Unlock()
	if held {
		t.Error("the lease survived a force-take by a newer generation")
	}
	select {
	case code := <-a.lost:
		if code != "k7xq2m" {
			t.Errorf("OnLeaseLost fired for %q, want k7xq2m", code)
		}
	case <-time.After(time.Second):
		t.Error("a force-take did not report the lease lost")
	}
}

// heldRoom is a store that has adopted k7xq2m and is holding its lease, with
// no renew loop in the way of the test.
func heldRoom(t *testing.T) *testStore {
	t.Helper()
	withListWatchReflector(t)
	// An abandoned pod-b lease, long unrenewed, so this pod adopts it as
	// generation 2 rather than 1 and a generation-1 view is genuinely older.
	stale := metav1.NewTime(time.Date(2026, 9, 3, 11, 0, 0, 0, time.UTC))
	seed := &rooms.Lease{Holder: "pod-b", Addr: "pod-b:4433", Generation: 1, RenewedAt: &stale}
	client := newFakeDynamic(t, roomObject(t, dynamicCR("k7xq2m", seed, nil)))
	a := newTestStore(t, client, "pod-a", newFakeClock(), func(o *Options) {
		o.RenewInterval = time.Hour
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	runSynced(t, ctx, client, a.Store)
	if err := a.Adopt(ctx, "k7xq2m"); err != nil {
		t.Fatal(err)
	}
	// Drain what adopting itself produced, so the assertions are about the
	// view each case feeds in.
	for len(a.lost) > 0 {
		<-a.lost
	}
	return a
}
