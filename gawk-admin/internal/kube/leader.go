package kube

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// Leader-election timings (docs/42 §4.6: "15 s-class TTL").
//
// The constraint the library enforces is RetryPeriod*1.2 < RenewDeadline <
// LeaseDuration; these satisfy it with room. The number that matters
// operationally is LeaseDuration: it is how long the singleton work is paused
// after a leader dies abruptly (§6, "another replica takes the Lease within
// the ~15 s TTL"), and the work it gates — a 60 s reconcile sweep and a
// webhook retry queue — tolerates that easily.
const (
	DefaultLeaseName     = "gawk-admin-leader"
	DefaultLeaseDuration = 15 * time.Second
	DefaultRenewDeadline = 10 * time.Second
	DefaultRetryPeriod   = 2 * time.Second
)

// LeaderOptions configure an Election.
type LeaderOptions struct {
	Client    kubernetes.Interface
	Namespace string
	// LeaseName defaults to DefaultLeaseName.
	LeaseName string
	// Identity distinguishes candidates; the pod name in production.
	Identity string
	Log      *slog.Logger

	// OnLeading runs the singleton background work — the reconciler/janitor
	// and AP7's webhook dispatcher, the two pieces D16 names. It is called
	// with a context cancelled the moment leadership is lost, so everything it
	// starts must stop when that context ends.
	OnLeading func(ctx context.Context)
	// OnStoppedLeading is an optional notification hook; the context passed to
	// OnLeading is already cancelled by the time it runs.
	OnStoppedLeading func()

	// ReleaseOnCancel makes a graceful shutdown hand the Lease over
	// immediately instead of making the next leader wait out the TTL. Callers
	// that shut down cleanly should set it; tests that simulate a CRASH must
	// not.
	ReleaseOnCancel bool

	// Test seams. Zero values mean the Default* constants.
	LeaseDuration time.Duration
	RenewDeadline time.Duration
	RetryPeriod   time.Duration
}

// Election runs one candidate in the leader election.
type Election struct {
	le     *leaderelection.LeaderElector
	log    *slog.Logger
	leader atomic.Bool
}

// NewElection builds a candidate. Nothing happens until Run.
func NewElection(opts LeaderOptions) (*Election, error) {
	if opts.Client == nil {
		return nil, fmt.Errorf("kube: LeaderOptions.Client is required")
	}
	if opts.Namespace == "" || opts.Identity == "" {
		return nil, fmt.Errorf("kube: LeaderOptions.Namespace and Identity are required")
	}
	if opts.OnLeading == nil {
		return nil, fmt.Errorf("kube: LeaderOptions.OnLeading is required")
	}
	if opts.LeaseName == "" {
		opts.LeaseName = DefaultLeaseName
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.LeaseDuration <= 0 {
		opts.LeaseDuration = DefaultLeaseDuration
	}
	if opts.RenewDeadline <= 0 {
		opts.RenewDeadline = DefaultRenewDeadline
	}
	if opts.RetryPeriod <= 0 {
		opts.RetryPeriod = DefaultRetryPeriod
	}

	e := &Election{log: opts.Log}
	lock := &resourcelock.LeaseLock{
		LeaseMeta:  metav1.ObjectMeta{Name: opts.LeaseName, Namespace: opts.Namespace},
		Client:     opts.Client.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{Identity: opts.Identity},
	}
	le, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:            lock,
		LeaseDuration:   opts.LeaseDuration,
		RenewDeadline:   opts.RenewDeadline,
		RetryPeriod:     opts.RetryPeriod,
		ReleaseOnCancel: opts.ReleaseOnCancel,
		Name:            opts.LeaseName,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				e.leader.Store(true)
				opts.Log.Info("became gawk-admin leader", "identity", opts.Identity, "lease", opts.LeaseName)
				defer e.leader.Store(false)
				opts.OnLeading(ctx)
			},
			OnStoppedLeading: func() {
				e.leader.Store(false)
				opts.Log.Warn("lost gawk-admin leadership", "identity", opts.Identity, "lease", opts.LeaseName)
				if opts.OnStoppedLeading != nil {
					opts.OnStoppedLeading()
				}
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("kube: leader election: %w", err)
	}
	e.le = le
	return e, nil
}

// Run campaigns until ctx ends. It blocks; a non-leader simply stands by and
// keeps serving API traffic from the same process (D16).
func (e *Election) Run(ctx context.Context) { e.le.Run(ctx) }

// IsLeader reports whether this replica currently holds the Lease. It is
// observational — the singleton work is gated by OnLeading's context, never by
// polling this.
func (e *Election) IsLeader() bool { return e.leader.Load() }
