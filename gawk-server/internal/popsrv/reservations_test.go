package popsrv

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	coordv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Tuhis/gawk/gawk-server/internal/cluster"
)

// R74 PP8 (docs/76 item 14): the Reservations a link-home pod drives, over
// the cluster Lease registry and, single-pod, in memory. Both honour one
// contract; it runs against each.

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// leaseWatchRegistered: the fake tracker replays nothing to a late watch,
// so the cache tests wait for the informer's watch before writing.
func leaseWatchRegistered(cs *fake.Clientset) <-chan struct{} {
	registered := make(chan struct{})
	var once sync.Once
	gvr := coordv1.SchemeGroupVersion.WithResource("leases")
	cs.PrependWatchReactor("leases", func(action k8stesting.Action) (bool, watch.Interface, error) {
		w, err := cs.Tracker().Watch(gvr, action.GetNamespace())
		if err != nil {
			return false, nil, err
		}
		once.Do(func() { close(registered) })
		return true, w, nil
	})
	return registered
}

type reservationsFixture struct {
	res Reservations
	// cs is the cluster's control plane; nil for the in-memory set.
	cs *fake.Clientset
	// settle waits until the non-blocking reads reflect the writes so far.
	settle func(t *testing.T, cond func() bool, what string)
}

func newClusterFixture(t *testing.T) reservationsFixture {
	t.Helper()
	cs := fake.NewClientset()
	watching := leaseWatchRegistered(cs)
	coord, err := cluster.New(cluster.Options{
		Client:         cs,
		Namespace:      "gawk",
		PodName:        "pod-a",
		AdvertiseAddr:  "pod-a:4433",
		BroadcastGrace: 5 * time.Minute,
		Log:            slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("cluster.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go coord.Run(ctx)
	<-watching
	if !coord.WaitLeaseSync(ctx) {
		t.Fatal("WaitLeaseSync")
	}
	return reservationsFixture{
		res: NewClusterReservations(coord),
		cs:  cs,
		settle: func(t *testing.T, cond func() bool, what string) {
			t.Helper()
			waitFor(t, 15*time.Second, cond, what)
		},
	}
}

func newMemFixture(*testing.T) reservationsFixture {
	return reservationsFixture{
		res: NewMemReservations(),
		settle: func(t *testing.T, cond func() bool, what string) {
			t.Helper()
			if !cond() {
				t.Fatalf("in-memory reservations: %s", what)
			}
		},
	}
}

func eachReservations(t *testing.T, fn func(t *testing.T, f reservationsFixture)) {
	t.Run("cluster", func(t *testing.T) { fn(t, newClusterFixture(t)) })
	t.Run("memory", func(t *testing.T) { fn(t, newMemFixture(t)) })
}

func reserve(t *testing.T, r Reservations, id, pop string, limit int, want string) {
	t.Helper()
	got, err := r.Reserve(context.Background(), id, pop, limit)
	if err != nil {
		t.Fatalf("Reserve(%s, %s): %v", id, pop, err)
	}
	if got != want {
		t.Fatalf("Reserve(%s, %s, limit %d) = %q, want %q", id, pop, limit, got, want)
	}
}

func TestReservationsReserveConflictAndLimit(t *testing.T) {
	eachReservations(t, func(t *testing.T, f reservationsFixture) {
		reserve(t, f.res, "K7XQ2M", "lan-party", 2, ReserveOK)
		// The same ID again — this PoP's or another's — collides.
		reserve(t, f.res, "K7XQ2M", "lan-party", 2, ReserveConflict)
		reserve(t, f.res, "K7XQ2M", "attic", 2, ReserveConflict)
		reserve(t, f.res, "ABC234", "lan-party", 2, ReserveOK)
		// The PoP's own limit binds on its count, not on anyone else's.
		reserve(t, f.res, "DEF567", "lan-party", 2, ReserveLimit)
		reserve(t, f.res, "DEF567", "attic", 2, ReserveOK)
		// 0 is unlimited.
		reserve(t, f.res, "GHJ789", "lan-party", 0, ReserveOK)

		f.settle(t, func() bool {
			return slices.Equal(f.res.List("lan-party"), []string{"ABC234", "GHJ789", "K7XQ2M"})
		}, "List(lan-party) sorted")
		f.settle(t, func() bool { pop, ok := f.res.Owner("DEF567"); return ok && pop == "attic" }, "Owner(DEF567)")
		if _, ok := f.res.Owner("MNP234"); ok {
			t.Error("Owner of an unreserved ID")
		}
	})
}

func TestReservationsReleaseAndReleaseAll(t *testing.T) {
	eachReservations(t, func(t *testing.T, f reservationsFixture) {
		ctx := context.Background()
		for _, id := range []string{"K7XQ2M", "ABC234", "GHJ789"} {
			reserve(t, f.res, id, "lan-party", 0, ReserveOK)
		}
		reserve(t, f.res, "DEF567", "attic", 0, ReserveOK)

		// Another PoP cannot release it; a missing one is a no-op.
		if err := f.res.Release(ctx, "DEF567", "lan-party"); !errors.Is(err, cluster.ErrReserved) {
			t.Errorf("cross-PoP Release = %v, want cluster.ErrReserved", err)
		}
		if err := f.res.Release(ctx, "MNP234", "lan-party"); err != nil {
			t.Errorf("Release of a missing reservation = %v, want nil", err)
		}
		if err := f.res.Release(ctx, "K7XQ2M", "lan-party"); err != nil {
			t.Fatalf("Release: %v", err)
		}
		f.settle(t, func() bool { _, ok := f.res.Owner("K7XQ2M"); return !ok }, "released ID unowned")

		// Revoke: every reservation of the PoP goes, the other PoP's stays.
		if err := f.res.ReleaseAll(ctx, "lan-party"); err != nil {
			t.Fatalf("ReleaseAll: %v", err)
		}
		f.settle(t, func() bool { return len(f.res.List("lan-party")) == 0 }, "lan-party emptied")
		f.settle(t, func() bool { pop, ok := f.res.Owner("DEF567"); return ok && pop == "attic" }, "attic's reservation kept")
		// The freed IDs are reservable again.
		reserve(t, f.res, "ABC234", "attic", 0, ReserveOK)
	})
}

// RenewAll is what keeps a dormant reservation off the janitor (cluster
// side: TestJanitorKeepsRenewedReservations); here, that it reaches the
// Lease and leaves other PoPs' alone.
func TestClusterReservationsRenewAll(t *testing.T) {
	f := newClusterFixture(t)
	ctx := context.Background()
	reserve(t, f.res, "K7XQ2M", "lan-party", 0, ReserveOK)
	reserve(t, f.res, "DEF567", "attic", 0, ReserveOK)
	renewTime := func(id string) time.Time {
		l, err := f.cs.CoordinationV1().Leases("gawk").Get(ctx, "gawk-bc-"+strings.ToLower(id), metav1.GetOptions{})
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		return l.Spec.RenewTime.Time
	}
	before, other := renewTime("K7XQ2M"), renewTime("DEF567")
	time.Sleep(10 * time.Millisecond)
	if err := f.res.RenewAll(ctx, "lan-party"); err != nil {
		t.Fatalf("RenewAll: %v", err)
	}
	if !renewTime("K7XQ2M").After(before) {
		t.Error("RenewAll did not bump the PoP's reservation")
	}
	if !renewTime("DEF567").Equal(other) {
		t.Error("RenewAll touched another PoP's reservation")
	}
	if err := NewMemReservations().RenewAll(ctx, "lan-party"); err != nil {
		t.Errorf("in-memory RenewAll = %v, want nil", err)
	}
}
