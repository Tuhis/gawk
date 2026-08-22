package kube_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Tuhis/gawk/gawk-admin/internal/kube"
	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-server/moderation"
)

// Leader-election test timings. The library enforces
// RetryPeriod*1.2 < RenewDeadline < LeaseDuration; these keep a full
// acquire→lose→re-acquire cycle inside a couple of seconds so the tests are
// fast without being tight enough to flake on a loaded machine.
const (
	testLeaseDuration = 2 * time.Second
	testRenewDeadline = 1 * time.Second
	testRetryPeriod   = 150 * time.Millisecond
	// testSettle bounds how long a test waits for leadership to move. It is
	// generously above testLeaseDuration on purpose: the assertion is "within
	// the TTL, not never", and a CI runner's scheduler is not a real-time one.
	testSettle = 15 * time.Second
)

// candidate is one gawk-admin replica in the tests: an Election plus a record
// of whether its singleton work is currently running.
type candidate struct {
	name     string
	election *kube.Election
	// running is true while OnLeading has not returned — i.e. while this
	// replica is the one doing the singleton work.
	running atomic.Bool
	// leadCount counts how many times it acquired leadership.
	leadCount atomic.Int64
	cancel    context.CancelFunc
	done      chan struct{}
}

func newCandidate(t *testing.T, client *k8sfake.Clientset, name string, work func(ctx context.Context)) *candidate {
	t.Helper()
	c := &candidate{name: name, done: make(chan struct{})}
	e, err := kube.NewElection(kube.LeaderOptions{
		Client:        client,
		Namespace:     testNamespace,
		Identity:      name,
		LeaseName:     "gawk-admin-leader-test",
		LeaseDuration: testLeaseDuration,
		RenewDeadline: testRenewDeadline,
		RetryPeriod:   testRetryPeriod,
		OnLeading: func(ctx context.Context) {
			c.running.Store(true)
			c.leadCount.Add(1)
			defer c.running.Store(false)
			if work != nil {
				work(ctx)
			}
			<-ctx.Done()
		},
	})
	if err != nil {
		t.Fatalf("NewElection(%s): %v", name, err)
	}
	c.election = e
	return c
}

// start campaigns in the background. Cancelling its context WITHOUT
// ReleaseOnCancel is how these tests simulate a crashed leader: the Lease is
// abandoned, not handed over, so the survivor must wait out the TTL exactly as
// it would in production (§6).
func (c *candidate) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	go func() {
		defer close(c.done)
		c.election.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-c.done:
		case <-time.After(testSettle):
			t.Errorf("candidate %s did not stop", c.name)
		}
	})
}

func (c *candidate) kill(t *testing.T) {
	t.Helper()
	c.cancel()
	select {
	case <-c.done:
	case <-time.After(testSettle):
		t.Fatalf("candidate %s did not stop after cancel", c.name)
	}
}

// waitFor polls until cond holds or testSettle elapses.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testSettle)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// leaders returns the candidates currently running their singleton work.
func leaders(cs ...*candidate) []string {
	var out []string
	for _, c := range cs {
		if c.running.Load() {
			out = append(out, c.name)
		}
	}
	return out
}

// The core D16 property: the singleton background work runs on EXACTLY ONE of
// two live replicas, and moves to the survivor within the lease TTL when the
// leader dies.
func TestLeaderElectionRunsSingletonWorkOnExactlyOneReplica(t *testing.T) {
	client := k8sfake.NewClientset()

	a := newCandidate(t, client, "gawk-admin-a", nil)
	b := newCandidate(t, client, "gawk-admin-b", nil)
	a.start(t)
	b.start(t)

	waitFor(t, "a leader to emerge", func() bool { return len(leaders(a, b)) == 1 })

	// Hold the assertion for a few renew cycles: "exactly one" must be true
	// continuously, not just at one lucky instant.
	deadline := time.Now().Add(3 * testRenewDeadline)
	var first string
	for time.Now().Before(deadline) {
		l := leaders(a, b)
		if len(l) != 1 {
			t.Fatalf("leaders = %v, want exactly one", l)
		}
		if first == "" {
			first = l[0]
		} else if l[0] != first {
			t.Fatalf("leadership moved from %s to %s while both replicas were healthy", first, l[0])
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Kill the leader without releasing the Lease — a crash, not a shutdown.
	var leader, survivor *candidate
	if first == a.name {
		leader, survivor = a, b
	} else {
		leader, survivor = b, a
	}
	leader.kill(t)

	waitFor(t, "the survivor to take over", survivor.running.Load)
	if leader.running.Load() {
		t.Fatalf("the killed leader is still running its singleton work")
	}
	if got := survivor.leadCount.Load(); got != 1 {
		t.Fatalf("survivor acquired leadership %d times, want 1", got)
	}
	if !survivor.election.IsLeader() {
		t.Fatalf("IsLeader() disagrees with the running work")
	}
}

// The leader's context must be cancelled when leadership ends, so everything
// the leader started — reconciler loop and webhook dispatcher alike — stops.
func TestLeadershipContextEndsWithLeadership(t *testing.T) {
	client := k8sfake.NewClientset()

	var (
		mu      sync.Mutex
		stopped []string
	)
	work := func(name string) func(context.Context) {
		return func(ctx context.Context) {
			go func() {
				<-ctx.Done()
				mu.Lock()
				stopped = append(stopped, name)
				mu.Unlock()
			}()
		}
	}
	a := newCandidate(t, client, "gawk-admin-a", work("gawk-admin-a"))
	a.start(t)
	waitFor(t, "a to lead", a.running.Load)

	a.kill(t)
	waitFor(t, "the leader's work context to be cancelled", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(stopped) == 1
	})
}

// Two replicas against one record store must not produce duplicate CR writes.
// The reconciler is leader-only, so the non-leader never writes at all — and
// the CR set is what one reconciler would have produced.
func TestTwoReplicasProduceNoDuplicateCRWrites(t *testing.T) {
	client := k8sfake.NewClientset()
	recs := newFakeRecords()
	crs, _ := newFakeCRClient(t)
	counting := &countingBans{inner: crs}

	recs.add(store.Ban{Target: moderation.Target{Type: moderation.TargetBroadcastID, Value: "RRR234"}, CreatedBy: "op"})
	recs.add(store.Ban{Target: moderation.Target{Type: moderation.TargetIP, Value: "203.0.113.7"}, CreatedBy: "op"})

	// Each replica runs its own reconciler, but only under leadership — the
	// wiring main.go will use.
	runReconciler := func(ctx context.Context) {
		r, err := kube.NewReconciler(kube.ReconcilerOptions{
			Records: recs, Bans: counting, Interval: 50 * time.Millisecond,
		})
		if err != nil {
			t.Errorf("NewReconciler: %v", err)
			return
		}
		go r.Run(ctx)
	}

	a := newCandidate(t, client, "gawk-admin-a", runReconciler)
	b := newCandidate(t, client, "gawk-admin-b", runReconciler)
	a.start(t)
	b.start(t)

	waitFor(t, "the CRs to be projected", func() bool {
		list, err := crs.List(context.Background())
		return err == nil && len(list) == 2
	})
	// Let several sweeps run on the leader; a converged pass must write
	// nothing, so the upsert count must stay at exactly one per ban.
	time.Sleep(500 * time.Millisecond)

	list, err := crs.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("two replicas produced %d CRs, want 2: %+v", len(list), list)
	}
	upserts, deletes := counting.counts()
	if upserts != 2 {
		t.Fatalf("CR upserts = %d across many sweeps on two replicas, want 2 (one per ban)", upserts)
	}
	if deletes != 0 {
		t.Fatalf("CR deletes = %d, want 0", deletes)
	}
	if len(leaders(a, b)) != 1 {
		t.Fatalf("leaders = %v, want exactly one", leaders(a, b))
	}
}

// Losing the Lease must not end the campaign. client-go's LeaderElector.Run
// returns FOR GOOD once a renewal misses RenewDeadline — a >10 s API-server
// blip is enough — so a replica that does not re-enter the election never runs
// the reconciler/janitor or the webhook dispatcher again. Once every replica
// has been demoted once, nothing expires a ban and nothing sends a queued
// webhook delivery, silently, until a pod restarts.
func TestElectionReEntersAfterLosingTheLease(t *testing.T) {
	client := k8sfake.NewClientset()

	// A reactor that makes every Lease write fail is an API-server blip: it
	// breaks renewal (and re-acquisition) without cancelling anyone's context,
	// which is exactly the production condition — the pod is healthy, the API
	// server is not.
	var blip atomic.Bool
	client.PrependReactor("update", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		if blip.Load() {
			return true, nil, errors.New("apiserver is unavailable")
		}
		return false, nil, nil
	})

	a := newCandidate(t, client, "gawk-admin-a", nil)
	a.start(t)
	waitFor(t, "the replica to lead", a.running.Load)

	blip.Store(true)
	waitFor(t, "the replica to lose the lease", func() bool { return !a.running.Load() })
	blip.Store(false)

	waitFor(t, "the replica to re-enter the election", func() bool {
		select {
		case <-a.done:
			t.Fatalf("Election.Run returned after a lost lease: this replica has stopped " +
				"campaigning, so once every replica has been demoted once nobody runs the " +
				"reconciler/janitor or the webhook dispatcher")
		default:
		}
		return a.leadCount.Load() >= 2
	})
	if !a.election.IsLeader() {
		t.Fatalf("IsLeader() is false after re-acquiring the lease")
	}
}
