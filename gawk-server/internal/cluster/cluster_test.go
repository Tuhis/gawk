// R17 W3 acceptance tests (docs/22): claim semantics (two claimants ⇒ one
// winner, force-take beats a live holder, CAS conflicts retried), lifecycle
// (create / grace / release / delete), the API renew budget, the janitor's
// stale-only deletions, and the informer callbacks — all against the fake
// clientset, no cluster.
package cluster

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coordv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

var discardLog = slog.New(slog.NewTextHandler(io.Discard, nil))

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestCoordinator(t *testing.T, cs *fake.Clientset, pod string, clock *fakeClock, mutate func(*Options)) *Coordinator {
	t.Helper()
	opts := Options{
		Client:         cs,
		Namespace:      "gawk",
		PodName:        pod,
		AdvertiseAddr:  pod + ":4433",
		BroadcastGrace: 5 * time.Minute,
		Log:            discardLog,
		LeaseDuration:  15 * time.Second,
		RenewInterval:  20 * time.Millisecond,
		Now:            clock.Now,
	}
	if mutate != nil {
		mutate(&opts)
	}
	c, err := New(opts)
	if err != nil {
		t.Fatalf("cluster.New: %v", err)
	}
	t.Cleanup(func() {
		c.mu.Lock()
		held := make([]*heldLease, 0, len(c.held))
		for _, h := range c.held {
			held = append(held, h)
		}
		c.mu.Unlock()
		for _, h := range held {
			h.cancel()
		}
	})
	return c
}

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

func TestClaimCreatesLease(t *testing.T) {
	cs := fake.NewClientset()
	clock := newFakeClock()
	a := newTestCoordinator(t, cs, "pod-a", clock, nil)
	ctx := context.Background()

	gen, err := a.Claim(ctx, "K7XQ2M", false)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if gen != 1 {
		t.Errorf("first claim generation = %d, want 1", gen)
	}

	lease, err := cs.CoordinationV1().Leases("gawk").Get(ctx, "gawk-bc-K7XQ2M", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("lease not created: %v", err)
	}
	if lease.Labels[LeaseLabelKey] != LeaseLabelValue {
		t.Errorf("lease label %q = %q, want %q", LeaseLabelKey, lease.Labels[LeaseLabelKey], LeaseLabelValue)
	}

	origin, err := a.Resolve(ctx, "K7XQ2M")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if origin.Holder != "pod-a" || origin.Addr != "pod-a:4433" || origin.Generation != 1 {
		t.Errorf("Resolve = %+v, want holder pod-a addr pod-a:4433 gen 1", origin)
	}
	if !a.Held("K7XQ2M", 1) {
		t.Error("coordinator does not believe it holds the lease it claimed")
	}
}

// Two claimants, one winner: a live (renewing, non-stale) holder beats any
// non-force claim.
func TestSecondClaimantLosesToLiveHolder(t *testing.T) {
	cs := fake.NewClientset()
	clock := newFakeClock()
	a := newTestCoordinator(t, cs, "pod-a", clock, nil)
	b := newTestCoordinator(t, cs, "pod-b", clock, nil)
	ctx := context.Background()

	if _, err := a.Claim(ctx, "K7XQ2M", false); err != nil {
		t.Fatalf("A claim: %v", err)
	}
	if _, err := b.Claim(ctx, "K7XQ2M", false); !errors.Is(err, ErrHeldElsewhere) {
		t.Fatalf("B claim err = %v, want ErrHeldElsewhere", err)
	}
}

// A force-take (verified resume token in hand) beats a live holder, and the
// loser's renew loop notices and reports the loss.
func TestForceTakeBeatsLiveHolderAndLoserNotices(t *testing.T) {
	cs := fake.NewClientset()
	clock := newFakeClock()
	var lostID atomic.Value
	a := newTestCoordinator(t, cs, "pod-a", clock, func(o *Options) {
		o.OnLeaseLost = func(id string, newOrigin Origin) {
			lostID.Store(id + "→" + newOrigin.Holder)
		}
	})
	b := newTestCoordinator(t, cs, "pod-b", clock, nil)
	ctx := context.Background()

	if _, err := a.Claim(ctx, "K7XQ2M", false); err != nil {
		t.Fatalf("A claim: %v", err)
	}
	gen, err := b.Claim(ctx, "K7XQ2M", true)
	if err != nil {
		t.Fatalf("B force-take: %v", err)
	}
	if gen != 2 {
		t.Errorf("force-take generation = %d, want 2", gen)
	}

	// A's renew loop (20 ms interval) discovers the loss.
	waitFor(t, 5*time.Second, func() bool { return lostID.Load() != nil }, "OnLeaseLost")
	if got := lostID.Load().(string); got != "K7XQ2M→pod-b" {
		t.Errorf("OnLeaseLost = %q, want K7XQ2M→pod-b", got)
	}
	if a.Held("K7XQ2M", 1) {
		t.Error("loser still believes it holds the lease")
	}
	if !b.Held("K7XQ2M", 2) {
		t.Error("winner does not believe it holds the lease")
	}
}

// A CAS conflict (another claimant updated between Get and Update) is
// retried with a fresh Get instead of failing or blind-writing.
func TestClaimRetriesThroughCASConflict(t *testing.T) {
	cs := fake.NewClientset()
	clock := newFakeClock()
	a := newTestCoordinator(t, cs, "pod-a", clock, nil)
	ctx := context.Background()

	// Seed a stale-holder lease so the claim goes down the update path.
	if _, err := a.Claim(ctx, "K7XQ2M", false); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
	a.stopRenew("K7XQ2M")
	clock.Advance(time.Minute) // holder now stale

	b := newTestCoordinator(t, cs, "pod-b", clock, nil)
	conflicts := 1
	cs.PrependReactor("update", "leases", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if conflicts > 0 {
			conflicts--
			return true, nil, apierrors.NewConflict(
				schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"},
				"gawk-bc-K7XQ2M", errors.New("simulated CAS loss"))
		}
		return false, nil, nil
	})

	gen, err := b.Claim(ctx, "K7XQ2M", false)
	if err != nil {
		t.Fatalf("Claim through conflict: %v", err)
	}
	if gen != 2 {
		t.Errorf("generation after conflict-retry = %d, want 2", gen)
	}
	if conflicts != 0 {
		t.Error("conflict reactor never fired")
	}
}

// Cluster-wide MaxBroadcasts binds at Lease-create (docs/22 Decision 13).
func TestClaimEnforcesClusterMaxBroadcasts(t *testing.T) {
	cs := fake.NewClientset()
	clock := newFakeClock()
	a := newTestCoordinator(t, cs, "pod-a", clock, func(o *Options) { o.MaxBroadcasts = 1 })
	ctx := context.Background()

	if _, err := a.Claim(ctx, "AAA234", false); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if _, err := a.Claim(ctx, "BBB234", false); !errors.Is(err, ErrMaxBroadcasts) {
		t.Fatalf("second create err = %v, want ErrMaxBroadcasts", err)
	}
	// Re-claiming an EXISTING broadcast is not a create and stays allowed.
	if _, err := a.Claim(ctx, "AAA234", true); err != nil {
		t.Fatalf("re-claim of existing broadcast: %v", err)
	}
}

// API budget (docs/22 Decision 8): at most one renew per lease per interval,
// and none at all during grace.
func TestRenewCadenceAndGraceStopsRenewals(t *testing.T) {
	cs := fake.NewClientset()
	clock := newFakeClock()
	a := newTestCoordinator(t, cs, "pod-a", clock, nil)
	ctx := context.Background()

	countRenews := func() int {
		n := 0
		for _, action := range cs.Actions() {
			if action.Matches("update", "leases") {
				n++
			}
		}
		return n
	}

	if _, err := a.Claim(ctx, "K7XQ2M", false); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	start := time.Now()
	waitFor(t, 5*time.Second, func() bool { return countRenews() >= 3 }, "three renewals")
	elapsed := time.Since(start)
	// ≤ 1 renew per RenewInterval (20 ms), with slack for scheduling.
	if maxAllowed := int(elapsed/(20*time.Millisecond)) + 2; countRenews() > maxAllowed {
		t.Errorf("renews = %d in %v, want ≤ %d (1 per interval)", countRenews(), elapsed, maxAllowed)
	}

	// Grace: renewals stop and the deadline is stamped.
	if err := a.EnterGrace(ctx, "K7XQ2M"); err != nil {
		t.Fatalf("EnterGrace: %v", err)
	}
	lease, err := cs.CoordinationV1().Leases("gawk").Get(ctx, "gawk-bc-K7XQ2M", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Annotations[annotationGraceDeadline] == "" {
		t.Error("EnterGrace did not stamp the grace deadline")
	}
	after := countRenews()
	time.Sleep(100 * time.Millisecond) // five would-be intervals
	if got := countRenews(); got != after {
		t.Errorf("renews continued during grace: %d → %d", after, got)
	}
}

// Release (the drain) clears holdership without deleting the lease, so the
// broadcaster's instant reconnect claims it on another pod with no force.
func TestReleaseClearsHolderForNextClaim(t *testing.T) {
	cs := fake.NewClientset()
	clock := newFakeClock()
	a := newTestCoordinator(t, cs, "pod-a", clock, nil)
	b := newTestCoordinator(t, cs, "pod-b", clock, nil)
	ctx := context.Background()

	if _, err := a.Claim(ctx, "K7XQ2M", false); err != nil {
		t.Fatalf("A claim: %v", err)
	}
	a.ReleaseAll(ctx)

	origin, err := a.Resolve(ctx, "K7XQ2M")
	if err != nil {
		t.Fatalf("Resolve after release: %v", err)
	}
	if origin.Holder != "" {
		t.Errorf("holder after release = %q, want empty", origin.Holder)
	}

	// No force needed: empty holder is takeable.
	gen, err := b.Claim(ctx, "K7XQ2M", false)
	if err != nil {
		t.Fatalf("B claim after release: %v", err)
	}
	if gen != 2 {
		t.Errorf("generation after re-home = %d, want 2", gen)
	}
}

func TestDeleteRemovesLease(t *testing.T) {
	cs := fake.NewClientset()
	clock := newFakeClock()
	a := newTestCoordinator(t, cs, "pod-a", clock, nil)
	ctx := context.Background()

	if _, err := a.Claim(ctx, "K7XQ2M", false); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if err := a.Delete(ctx, "K7XQ2M"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := a.Resolve(ctx, "K7XQ2M"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve after delete err = %v, want ErrNotFound", err)
	}
	// Idempotent.
	if err := a.Delete(ctx, "K7XQ2M"); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
}

// The janitor deletes only leases stale past the grace period: an expired
// grace deadline, or (crash case) a renewTime stale past grace+duration.
func TestJanitorDeletesOnlyStalePastGrace(t *testing.T) {
	cs := fake.NewClientset()
	clock := newFakeClock()
	a := newTestCoordinator(t, cs, "pod-a", clock, nil)
	ctx := context.Background()
	now := clock.Now()

	mk := func(name string, renew time.Time, graceDeadline string) {
		holder := "some-pod"
		secs := int32(15)
		rt := metav1.NewMicroTime(renew)
		lease := &coordv1.Lease{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Labels:      map[string]string{LeaseLabelKey: LeaseLabelValue},
				Annotations: map[string]string{annotationGeneration: "1"},
			},
			Spec: coordv1.LeaseSpec{HolderIdentity: &holder, LeaseDurationSeconds: &secs, RenewTime: &rt},
		}
		if graceDeadline != "" {
			lease.Annotations[annotationGraceDeadline] = graceDeadline
		}
		if _, err := cs.CoordinationV1().Leases("gawk").Create(ctx, lease, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}

	mk("gawk-bc-DEADAA", now.Add(-6*time.Minute), now.Add(-time.Minute).UTC().Format(time.RFC3339)) // grace expired
	mk("gawk-bc-GRACEA", now.Add(-time.Minute), now.Add(4*time.Minute).UTC().Format(time.RFC3339))  // graced, not yet
	mk("gawk-bc-LIVEAA", now.Add(-2*time.Second), "")                                               // renewing fine
	mk("gawk-bc-CRASHA", now.Add(-10*time.Minute), "")                                              // crashed origin

	a.JanitorSweep(ctx)

	list, err := cs.CoordinationV1().Leases("gawk").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, l := range list.Items {
		got[l.Name] = true
	}
	if got["gawk-bc-DEADAA"] || got["gawk-bc-CRASHA"] {
		t.Errorf("janitor kept stale leases: %v", got)
	}
	if !got["gawk-bc-GRACEA"] || !got["gawk-bc-LIVEAA"] {
		t.Errorf("janitor deleted healthy leases: %v", got)
	}
}

// The informer surfaces lease deletions (cluster-wide "broadcast ended") and
// force-takes (holdership loss) as callbacks.
func TestInformerCallbacks(t *testing.T) {
	cs := fake.NewClientset()
	clock := newFakeClock()
	var deleted atomic.Value
	var lost atomic.Value
	a := newTestCoordinator(t, cs, "pod-a", clock, func(o *Options) {
		o.OnLeaseDeleted = func(id string) { deleted.Store(id) }
		o.OnLeaseLost = func(id string, newOrigin Origin) { lost.Store(id + "→" + newOrigin.Holder) }
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.Run(ctx)

	if _, err := a.Claim(ctx, "K7XQ2M", false); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	// Simulate a force-take by another pod writing the lease directly.
	waitFor(t, 5*time.Second, func() bool {
		lease, err := cs.CoordinationV1().Leases("gawk").Get(ctx, "gawk-bc-K7XQ2M", metav1.GetOptions{})
		if err != nil {
			return false
		}
		other := "pod-b"
		updated := lease.DeepCopy()
		updated.Spec.HolderIdentity = &other
		updated.Annotations[annotationGeneration] = "2"
		_, err = cs.CoordinationV1().Leases("gawk").Update(ctx, updated, metav1.UpdateOptions{})
		return err == nil
	}, "force-take write")
	waitFor(t, 5*time.Second, func() bool { return lost.Load() != nil }, "OnLeaseLost via informer")
	if got := lost.Load().(string); got != "K7XQ2M→pod-b" {
		t.Errorf("OnLeaseLost = %q, want K7XQ2M→pod-b", got)
	}

	// Deletion → cluster-wide broadcast end.
	if err := cs.CoordinationV1().Leases("gawk").Delete(ctx, "gawk-bc-K7XQ2M", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool { return deleted.Load() != nil }, "OnLeaseDeleted")
	if got := deleted.Load().(string); got != "K7XQ2M" {
		t.Errorf("OnLeaseDeleted = %q, want K7XQ2M", got)
	}
}

// R17 post-review fix (PR #47, the 5-minute time bomb): a pod whose lease
// was force-taken reaches its local grace expiry later and calls Delete with
// a stale opinion — it must NOT delete the new origin's actively-renewed
// lease (that would end the live broadcast cluster-wide under the new
// holder). Only the holder-of-record — or anyone, once a drain Release
// cleared the holder — may delete.
func TestDeleteOnlyRemovesOwnLease(t *testing.T) {
	cs := fake.NewClientset()
	clock := newFakeClock()
	a := newTestCoordinator(t, cs, "pod-a", clock, nil)
	b := newTestCoordinator(t, cs, "pod-b", clock, nil)
	ctx := context.Background()

	if _, err := a.Claim(ctx, "K7XQ2M", false); err != nil {
		t.Fatalf("A claim: %v", err)
	}
	if _, err := b.Claim(ctx, "K7XQ2M", true); err != nil {
		t.Fatalf("B force-take: %v", err)
	}

	// A's stale grace-expiry Delete: silent no-op, the lease survives at B.
	if err := a.Delete(ctx, "K7XQ2M"); err != nil {
		t.Fatalf("stale Delete errored (want silent no-op): %v", err)
	}
	origin, err := a.Resolve(ctx, "K7XQ2M")
	if err != nil {
		t.Fatalf("lease deleted by a non-holder: %v", err)
	}
	if origin.Holder != "pod-b" {
		t.Errorf("holder after stale delete = %q, want pod-b", origin.Holder)
	}

	// The holder-of-record deletes fine.
	if err := b.Delete(ctx, "K7XQ2M"); err != nil {
		t.Fatalf("holder Delete: %v", err)
	}
	if _, err := b.Resolve(ctx, "K7XQ2M"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("holder's Delete did not remove the lease: %v", err)
	}

	// A drain-released lease (empty holder) is anyone's to GC.
	if _, err := a.Claim(ctx, "K7XQ2M", false); err != nil {
		t.Fatalf("re-claim: %v", err)
	}
	a.ReleaseAll(ctx)
	if err := b.Delete(ctx, "K7XQ2M"); err != nil {
		t.Fatalf("Delete of released lease: %v", err)
	}
	if _, err := b.Resolve(ctx, "K7XQ2M"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("released lease not deletable: %v", err)
	}
}
