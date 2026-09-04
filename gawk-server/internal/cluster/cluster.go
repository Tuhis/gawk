// Package cluster implements the per-broadcast origin registry (R17 W3,
// docs/22 Decision 8): one Kubernetes Lease per broadcast, named
// gawk-bc-<id>, holder = pod name, annotations carrying the origin's
// dialable pod address and an originGeneration counter bumped on every
// claim. Any pod can claim (create-if-absent), reclaim, or — presenting a
// verified resume token, which the transport layer checks before calling
// Claim with force — force-take a lease from a live holder: the
// broadcaster-in-hand is ground truth, and force-taking is what makes
// re-homing event-driven instead of TTL-bound. TTL (stale renewTime) covers
// crashes only.
//
// The whole package is dormant unless -cluster-mode is on: no Kubernetes
// client is even constructed otherwise, keeping single-pod behavior
// byte-identical to pre-R17.
package cluster

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	coordv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// Lease naming + metadata. The label selects gawk broadcast leases for the
// informer, the janitor, and the cluster-wide MaxBroadcasts count without
// touching anything else in the namespace.
const (
	leasePrefix     = "gawk-bc-"
	LeaseLabelKey   = "gawk/lease"
	LeaseLabelValue = "broadcast"

	annotationAddr          = "gawk/origin-addr"
	annotationGeneration    = "gawk/origin-generation"
	annotationGraceDeadline = "gawk/grace-deadline"
)

// Defaults (docs/22 Decision 8): renew every ~5 s with a 15 s lease duration
// — hundreds of leases ⇒ low-hundreds QPS worst case, fine. Constants by
// design; Options can override them for tests.
const (
	DefaultLeaseDuration = 15 * time.Second
	DefaultRenewInterval = 5 * time.Second
	janitorInterval      = time.Minute
	// claimRetries bounds the CAS retry loop; conflicts mean another claimant
	// is racing and a fresh Get re-evaluates who should win.
	claimRetries = 3
)

// Sentinel errors. Check with errors.Is.
var (
	// ErrHeldElsewhere: the lease has a live (renewing) holder and the claim
	// was not a force-take.
	ErrHeldElsewhere = errors.New("cluster: broadcast lease held by a live origin")
	// ErrMaxBroadcasts: creating the lease would exceed the cluster-wide
	// broadcast limit (enforced at Lease-create, docs/22 Decision 13).
	ErrMaxBroadcasts = errors.New("cluster: cluster-wide broadcast limit reached")
	// ErrNotFound: no lease exists for the broadcast.
	ErrNotFound = errors.New("cluster: no lease for broadcast")
)

// Origin describes the current holder of a broadcast lease.
type Origin struct {
	Holder     string // pod name
	Addr       string // dialable pod address, "<podIP>:<port>" — never the Service VIP
	Generation int64  // bumped on every claim; fences stale edges (W4)
}

// Options configures a Coordinator.
type Options struct {
	Client    kubernetes.Interface
	Namespace string
	PodName   string
	// AdvertiseAddr is this pod's dialable address ("<podIP>:4433") written
	// into the lease for edge pulls (W4).
	AdvertiseAddr string
	// BroadcastGrace mirrors the hub's GC grace: the janitor deletes leases
	// whose grace deadline passed or whose renewTime went stale past it.
	BroadcastGrace time.Duration
	// MaxBroadcasts caps cluster-wide concurrent broadcasts (≈ lease count)
	// at Lease-create. 0 = unlimited.
	MaxBroadcasts int
	Log           *slog.Logger

	// OnLeaseDeleted fires (from the informer) when a broadcast lease
	// disappears: cluster-wide "broadcast ended" — the registry closes local
	// viewers with 4000.
	OnLeaseDeleted func(broadcastID string)
	// OnLeaseLost fires when a lease this pod holds (and is renewing) turns
	// out to be held by someone else — it was force-taken (NAT rebind /
	// re-home). W5's demote path hangs off this.
	OnLeaseLost func(broadcastID string, newOrigin Origin)

	// Test seams. Zero values mean production defaults.
	LeaseDuration time.Duration
	RenewInterval time.Duration
	Now           func() time.Time
}

// Coordinator owns this pod's lease claims, their renew loops, the janitor
// and the deletion/loss watch.
type Coordinator struct {
	opts Options

	mu   sync.Mutex
	held map[string]*heldLease
	// leaseStore is the informer's cache, published by runInformer so
	// Lookup can answer from it without an API round trip; nil until Run.
	leaseStore cache.Store
	// leaseSynced closes once the informer's initial list has landed in
	// leaseStore — before that the cache is empty, not authoritative.
	leaseSynced chan struct{}
}

type heldLease struct {
	generation int64
	cancel     context.CancelFunc
	done       chan struct{}
	// graced: the publisher disconnected; renewals stopped, holdership kept.
	graced bool
}

// New builds a Coordinator. Run must be called for the informer + janitor.
func New(opts Options) (*Coordinator, error) {
	if opts.Client == nil {
		return nil, errors.New("cluster: Options.Client is required")
	}
	if opts.Namespace == "" || opts.PodName == "" || opts.AdvertiseAddr == "" {
		return nil, errors.New("cluster: Namespace, PodName and AdvertiseAddr are required")
	}
	if opts.LeaseDuration <= 0 {
		opts.LeaseDuration = DefaultLeaseDuration
	}
	if opts.RenewInterval <= 0 {
		opts.RenewInterval = DefaultRenewInterval
	}
	if opts.BroadcastGrace <= 0 {
		opts.BroadcastGrace = 5 * time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	return &Coordinator{opts: opts, held: make(map[string]*heldLease), leaseSynced: make(chan struct{})}, nil
}

// leaseName maps a broadcast ID onto a Lease name. Lease names must be
// lowercase RFC 1123 subdomains — the API server rejects uppercase outright
// (docs/22 finding 11) — while broadcast IDs are canonically uppercase.
// The lowercase mapping is injective because IDs are case-insensitively
// unique (broadcastid.Normalize upcases before validating).
func leaseName(broadcastID string) string { return leasePrefix + strings.ToLower(broadcastID) }

// broadcastIDFromLease is leaseName's inverse: canonical uppercase back out,
// so janitor/informer comparisons match hub registry IDs.
func broadcastIDFromLease(name string) (string, bool) {
	id, ok := strings.CutPrefix(name, leasePrefix)
	return strings.ToUpper(id), ok
}

func (c *Coordinator) leases() interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*coordv1.Lease, error)
	Create(ctx context.Context, lease *coordv1.Lease, opts metav1.CreateOptions) (*coordv1.Lease, error)
	Update(ctx context.Context, lease *coordv1.Lease, opts metav1.UpdateOptions) (*coordv1.Lease, error)
	Delete(ctx context.Context, name string, opts metav1.DeleteOptions) error
	List(ctx context.Context, opts metav1.ListOptions) (*coordv1.LeaseList, error)
	Watch(ctx context.Context, opts metav1.ListOptions) (watch.Interface, error)
} {
	return c.opts.Client.CoordinationV1().Leases(c.opts.Namespace)
}

func labelSelector() string { return LeaseLabelKey + "=" + LeaseLabelValue }

// parseOrigin extracts the Origin view of a lease.
func parseOrigin(l *coordv1.Lease) Origin {
	o := Origin{Addr: l.Annotations[annotationAddr]}
	if l.Spec.HolderIdentity != nil {
		o.Holder = *l.Spec.HolderIdentity
	}
	if g, err := strconv.ParseInt(l.Annotations[annotationGeneration], 10, 64); err == nil {
		o.Generation = g
	}
	return o
}

// renewStale reports whether the lease's renewTime is older than its
// advertised duration — the holder stopped renewing (crash, or grace).
func (c *Coordinator) renewStale(l *coordv1.Lease) bool {
	if l.Spec.RenewTime == nil {
		return true
	}
	dur := c.opts.LeaseDuration
	if l.Spec.LeaseDurationSeconds != nil {
		dur = time.Duration(*l.Spec.LeaseDurationSeconds) * time.Second
	}
	return c.opts.Now().Sub(l.Spec.RenewTime.Time) > dur
}

// Claim acquires the broadcast's lease for this pod and starts its renew
// loop. force is set when the claimant presented a valid resume token — the
// proof of ownership that beats even a live holder (docs/22 Decision 8).
// Without force, a live holder wins (ErrHeldElsewhere). Returns the new
// originGeneration.
func (c *Coordinator) Claim(ctx context.Context, broadcastID string, force bool) (int64, error) {
	name := leaseName(broadcastID)
	var lastErr error
	for range claimRetries {
		existing, err := c.leases().Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			gen, err := c.createLease(ctx, broadcastID, name)
			if apierrors.IsAlreadyExists(err) {
				lastErr = err
				continue // raced another creator; re-evaluate
			}
			if err != nil {
				return 0, err
			}
			c.startRenew(broadcastID, gen)
			return gen, nil
		}
		if err != nil {
			return 0, err
		}

		origin := parseOrigin(existing)
		takeable := origin.Holder == "" || origin.Holder == c.opts.PodName || c.renewStale(existing) || force
		if !takeable {
			return 0, fmt.Errorf("%w: %s", ErrHeldElsewhere, origin.Holder)
		}

		gen := origin.Generation + 1
		now := metav1.NewMicroTime(c.opts.Now())
		updated := existing.DeepCopy()
		updated.Spec.HolderIdentity = &c.opts.PodName
		secs := int32(c.opts.LeaseDuration / time.Second)
		updated.Spec.LeaseDurationSeconds = &secs
		updated.Spec.AcquireTime = &now
		updated.Spec.RenewTime = &now
		if updated.Annotations == nil {
			updated.Annotations = map[string]string{}
		}
		updated.Annotations[annotationAddr] = c.opts.AdvertiseAddr
		updated.Annotations[annotationGeneration] = strconv.FormatInt(gen, 10)
		delete(updated.Annotations, annotationGraceDeadline)

		if _, err := c.leases().Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
			if apierrors.IsConflict(err) {
				lastErr = err
				continue // CAS lost; a fresh Get decides who should win now
			}
			return 0, err
		}
		c.startRenew(broadcastID, gen)
		return gen, nil
	}
	return 0, fmt.Errorf("cluster: claim retries exhausted for %s: %w", broadcastID, lastErr)
}

// createLease is the create-if-absent path; this is also where the
// cluster-wide MaxBroadcasts is enforced (≈ count of gawk leases).
func (c *Coordinator) createLease(ctx context.Context, broadcastID, name string) (int64, error) {
	if c.opts.MaxBroadcasts > 0 {
		list, err := c.leases().List(ctx, metav1.ListOptions{LabelSelector: labelSelector()})
		if err != nil {
			return 0, err
		}
		if len(list.Items) >= c.opts.MaxBroadcasts {
			return 0, ErrMaxBroadcasts
		}
	}
	now := metav1.NewMicroTime(c.opts.Now())
	secs := int32(c.opts.LeaseDuration / time.Second)
	lease := &coordv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{LeaseLabelKey: LeaseLabelValue},
			Annotations: map[string]string{
				annotationAddr:       c.opts.AdvertiseAddr,
				annotationGeneration: "1",
			},
		},
		Spec: coordv1.LeaseSpec{
			HolderIdentity:       &c.opts.PodName,
			LeaseDurationSeconds: &secs,
			AcquireTime:          &now,
			RenewTime:            &now,
		},
	}
	if _, err := c.leases().Create(ctx, lease, metav1.CreateOptions{}); err != nil {
		return 0, err
	}
	return 1, nil
}

// startRenew (re)starts the renew loop for a held lease.
func (c *Coordinator) startRenew(broadcastID string, generation int64) {
	c.mu.Lock()
	if prev, ok := c.held[broadcastID]; ok {
		prev.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	h := &heldLease{generation: generation, cancel: cancel, done: make(chan struct{})}
	c.held[broadcastID] = h
	c.mu.Unlock()

	go c.renewLoop(ctx, broadcastID, h)
}

func (c *Coordinator) renewLoop(ctx context.Context, broadcastID string, h *heldLease) {
	defer close(h.done)
	ticker := time.NewTicker(c.opts.RenewInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if err := c.renewOnce(ctx, broadcastID, h); err != nil {
			if errors.Is(err, errLost) {
				return
			}
			c.opts.Log.Warn("lease renew failed", "broadcast_id", broadcastID, "err", err)
		}
	}
}

var errLost = errors.New("cluster: lease lost")

func (c *Coordinator) renewOnce(ctx context.Context, broadcastID string, h *heldLease) error {
	opCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	lease, err := c.leases().Get(opCtx, leaseName(broadcastID), metav1.GetOptions{})
	if err != nil {
		return err
	}
	origin := parseOrigin(lease)
	if origin.Holder != c.opts.PodName || origin.Generation != h.generation {
		// Force-taken from under us (the watch usually reports it first, but
		// the renew loop must never fight the new holder).
		c.forgetHeld(broadcastID, h)
		if c.opts.OnLeaseLost != nil {
			c.opts.OnLeaseLost(broadcastID, origin)
		}
		return errLost
	}
	now := metav1.NewMicroTime(c.opts.Now())
	updated := lease.DeepCopy()
	updated.Spec.RenewTime = &now
	if _, err := c.leases().Update(opCtx, updated, metav1.UpdateOptions{}); err != nil {
		return err
	}
	return nil
}

func (c *Coordinator) forgetHeld(broadcastID string, h *heldLease) {
	c.mu.Lock()
	if cur, ok := c.held[broadcastID]; ok && cur == h {
		delete(c.held, broadcastID)
	}
	c.mu.Unlock()
	h.cancel()
}

// stopRenew halts the renew loop for a broadcast (if any) and returns its
// held state.
func (c *Coordinator) stopRenew(broadcastID string) *heldLease {
	c.mu.Lock()
	h := c.held[broadcastID]
	delete(c.held, broadcastID)
	c.mu.Unlock()
	if h != nil {
		h.cancel()
		<-h.done
	}
	return h
}

// EnterGrace marks the broadcaster as away (docs/22 Decision 8): renewals
// stop, holdership is kept, and a grace deadline is stamped so the janitor
// on any pod can GC the lease if nobody comes back.
func (c *Coordinator) EnterGrace(ctx context.Context, broadcastID string) error {
	c.stopRenew(broadcastID)
	lease, err := c.leases().Get(ctx, leaseName(broadcastID), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if parseOrigin(lease).Holder != c.opts.PodName {
		return nil // no longer ours to grace
	}
	updated := lease.DeepCopy()
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	deadline := c.opts.Now().Add(c.opts.BroadcastGrace)
	updated.Annotations[annotationGraceDeadline] = deadline.UTC().Format(time.RFC3339)
	_, err = c.leases().Update(ctx, updated, metav1.UpdateOptions{})
	return err
}

// Delete removes the broadcast's lease (local grace-GC expired, or the
// broadcast ended for good). Cluster-wide "broadcast ended": every pod's
// informer sees the deletion.
// Holder-gated (post-review fix, PR #47): only the holder-of-record — or
// anyone, once a drain Release cleared the holder — may delete. A pod whose
// lease was force-taken reaches its local grace expiry later with a stale
// opinion, and deleting here would end the LIVE broadcast cluster-wide under
// the new origin. The delete is additionally CAS-fenced on resourceVersion
// so a claim racing between the Get and the Delete wins over the deletion.
func (c *Coordinator) Delete(ctx context.Context, broadcastID string) error {
	c.stopRenew(broadcastID)
	lease, err := c.leases().Get(ctx, leaseName(broadcastID), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if h := parseOrigin(lease).Holder; h != "" && h != c.opts.PodName {
		return nil // not ours to end — the broadcast lives on elsewhere
	}
	err = c.leases().Delete(ctx, leaseName(broadcastID), metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{ResourceVersion: &lease.ResourceVersion},
	})
	if apierrors.IsNotFound(err) || apierrors.IsConflict(err) {
		return nil
	}
	return err
}

// Release clears this pod's holdership without deleting the lease (the
// SIGTERM drain, docs/22 Decision 8): the broadcaster's instant reconnect
// lands on another pod and claims the empty-holder lease with no force
// needed and no TTL wait.
func (c *Coordinator) Release(ctx context.Context, broadcastID string) error {
	c.stopRenew(broadcastID)
	lease, err := c.leases().Get(ctx, leaseName(broadcastID), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if parseOrigin(lease).Holder != c.opts.PodName {
		return nil
	}
	empty := ""
	updated := lease.DeepCopy()
	updated.Spec.HolderIdentity = &empty
	_, err = c.leases().Update(ctx, updated, metav1.UpdateOptions{})
	return err
}

// ReleaseAll releases every lease this pod currently holds (drain).
func (c *Coordinator) ReleaseAll(ctx context.Context) {
	c.mu.Lock()
	ids := make([]string, 0, len(c.held))
	for id := range c.held {
		ids = append(ids, id)
	}
	c.mu.Unlock()
	for _, id := range ids {
		if err := c.Release(ctx, id); err != nil {
			c.opts.Log.Warn("lease release failed during drain", "broadcast_id", id, "err", err)
		}
	}
}

// Resolve returns the current origin of a broadcast (edge pull, W4).
func (c *Coordinator) Resolve(ctx context.Context, broadcastID string) (Origin, error) {
	lease, err := c.leases().Get(ctx, leaseName(broadcastID), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return Origin{}, ErrNotFound
	}
	if err != nil {
		return Origin{}, err
	}
	return parseOrigin(lease), nil
}

// Lookup is Resolve's cached, non-blocking twin (R42, docs/44 §4.5): the
// broadcast's lease as the informer last saw it. ok is false before the
// informer has synced (LeasesSynced) and when no lease exists — the
// fleet-wide "no such broadcast". inGrace is true once the origin stamped
// a grace deadline (EnterGrace) or, the crash case that never stamps one,
// once its renewTime went stale. A room's 1 Hz refresh asks this for every
// attachment homed on another pod, which is why it must not be a Get.
func (c *Coordinator) Lookup(broadcastID string) (origin Origin, inGrace bool, ok bool) {
	c.mu.Lock()
	store := c.leaseStore
	c.mu.Unlock()
	if store == nil || !c.LeasesSynced() {
		return Origin{}, false, false
	}
	obj, exists, err := store.GetByKey(c.opts.Namespace + "/" + leaseName(broadcastID))
	if err != nil || !exists {
		return Origin{}, false, false
	}
	lease, isLease := obj.(*coordv1.Lease)
	if !isLease {
		return Origin{}, false, false
	}
	return parseOrigin(lease), lease.Annotations[annotationGraceDeadline] != "" || c.renewStale(lease), true
}

// LeasesSynced reports whether the lease informer has completed its initial
// list, i.e. whether Lookup's "no lease" means anything.
func (c *Coordinator) LeasesSynced() bool {
	select {
	case <-c.leaseSynced:
		return true
	default:
		return false
	}
}

// WaitLeaseSync blocks until the lease informer has synced or ctx ends;
// false means ctx ended first. Callers that turn "unknown" into an action
// (the room refresh's expiry) wait here before their first poll.
func (c *Coordinator) WaitLeaseSync(ctx context.Context) bool {
	select {
	case <-c.leaseSynced:
		return true
	case <-ctx.Done():
		return false
	}
}

// Held reports whether this pod currently believes it holds the broadcast's
// lease at the given generation (the W4 generation fence for the internal
// subscribe route).
func (c *Coordinator) Held(broadcastID string, generation int64) bool {
	gen, held := c.OriginGeneration(broadcastID)
	return held && gen == generation
}

// OriginGeneration returns the generation at which this pod holds the
// broadcast's lease, and whether it holds it at all — the 404 (not origin)
// vs 409 (stale generation) split for the internal subscribe route (W4).
func (c *Coordinator) OriginGeneration(broadcastID string) (int64, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	h, ok := c.held[broadcastID]
	if !ok {
		return 0, false
	}
	return h.generation, true
}

// Run starts the informer (lease deletions + holdership loss) and the
// janitor, blocking until ctx is cancelled.
func (c *Coordinator) Run(ctx context.Context) {
	go c.janitorLoop(ctx)
	c.runInformer(ctx)
}

func (c *Coordinator) runInformer(ctx context.Context) {
	// One label-selected informer per pod (docs/22 Decision 8): a single
	// watch on gawk broadcast leases in our namespace, shared by the
	// deletion (cluster-wide "broadcast ended") and holdership-loss signals.
	factory := informers.NewSharedInformerFactoryWithOptions(
		c.opts.Client, 0,
		informers.WithNamespace(c.opts.Namespace),
		informers.WithTweakListOptions(func(options *metav1.ListOptions) {
			options.LabelSelector = labelSelector()
		}),
	)
	informer := factory.Coordination().V1().Leases().Informer()
	// The same cache serves Lookup (R42): publish it before the informer
	// runs, and flag it authoritative once the initial list has landed.
	c.mu.Lock()
	c.leaseStore = informer.GetStore()
	c.mu.Unlock()
	go func() {
		if cache.WaitForCacheSync(ctx.Done(), informer.HasSynced) {
			close(c.leaseSynced)
		}
	}()
	_, _ = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		DeleteFunc: func(obj any) {
			lease, ok := obj.(*coordv1.Lease)
			if !ok {
				if tomb, ok2 := obj.(cache.DeletedFinalStateUnknown); ok2 {
					lease, ok = tomb.Obj.(*coordv1.Lease)
				}
				if !ok {
					return
				}
			}
			id, ok := broadcastIDFromLease(lease.Name)
			if !ok {
				return
			}
			c.mu.Lock()
			h := c.held[id]
			delete(c.held, id)
			c.mu.Unlock()
			if h != nil {
				h.cancel()
			}
			if c.opts.OnLeaseDeleted != nil {
				c.opts.OnLeaseDeleted(id)
			}
		},
		UpdateFunc: func(_, newObj any) {
			lease, ok := newObj.(*coordv1.Lease)
			if !ok {
				return
			}
			id, ok := broadcastIDFromLease(lease.Name)
			if !ok {
				return
			}
			origin := parseOrigin(lease)
			c.mu.Lock()
			h, held := c.held[id]
			lost := held && (origin.Holder != c.opts.PodName || origin.Generation != h.generation)
			if lost {
				delete(c.held, id)
			}
			c.mu.Unlock()
			if lost {
				h.cancel()
				if c.opts.OnLeaseLost != nil {
					c.opts.OnLeaseLost(id, origin)
				}
			}
		},
	})
	informer.RunWithContext(ctx)
}

// janitorLoop is the leaderless GC (docs/22 Decision 8): every pod
// periodically deletes leases whose grace deadline passed or whose renewTime
// went stale past the grace period — covering a dead origin that never got
// to run its own GC. Deletion uses a resourceVersion precondition so a
// concurrent reclaim wins.
func (c *Coordinator) janitorLoop(ctx context.Context) {
	ticker := time.NewTicker(janitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.JanitorSweep(ctx)
		}
	}
}

// JanitorSweep runs one janitor pass. Exported for tests (and callable from
// ops tooling); production runs it on the janitorLoop cadence.
func (c *Coordinator) JanitorSweep(ctx context.Context) {
	list, err := c.leases().List(ctx, metav1.ListOptions{LabelSelector: labelSelector()})
	if err != nil {
		c.opts.Log.Warn("janitor list failed", "err", err)
		return
	}
	now := c.opts.Now()
	for i := range list.Items {
		lease := &list.Items[i]
		if !c.janitorExpired(lease, now) {
			continue
		}
		err := c.leases().Delete(ctx, lease.Name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{ResourceVersion: &lease.ResourceVersion},
		})
		if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			c.opts.Log.Warn("janitor delete failed", "lease", lease.Name, "err", err)
			continue
		}
		if err == nil {
			c.opts.Log.Info("janitor deleted stale broadcast lease", "lease", lease.Name)
		}
	}
}

// janitorExpired: the grace deadline passed, or (crash case: no deadline was
// ever stamped) the renewTime is stale past the whole grace period.
func (c *Coordinator) janitorExpired(lease *coordv1.Lease, now time.Time) bool {
	if d := lease.Annotations[annotationGraceDeadline]; d != "" {
		deadline, err := time.Parse(time.RFC3339, d)
		if err == nil {
			return now.After(deadline)
		}
	}
	if lease.Spec.RenewTime == nil {
		return true
	}
	return now.Sub(lease.Spec.RenewTime.Time) > c.opts.BroadcastGrace+c.opts.LeaseDuration
}
