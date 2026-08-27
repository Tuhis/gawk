package moderationsrc

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Tuhis/gawk/gawk-server/moderation"
)

// startK8s watches Ban CRs in POD_NAMESPACE (docs/42 D2/§4.2). It is
// deliberately independent of -cluster-mode: enforcement is not a federation
// feature, and a single-pod relay must be able to enforce bans too.
func startK8s(ctx context.Context, opts Options) error {
	namespace := opts.Namespace
	if namespace == "" {
		namespace = os.Getenv("POD_NAMESPACE")
	}
	if namespace == "" {
		return fmt.Errorf("moderation source k8s requires POD_NAMESPACE (downward API)")
	}
	restCfg, err := restConfig()
	if err != nil {
		return fmt.Errorf("moderation source k8s requires kubernetes credentials: %w", err)
	}
	client, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return err
	}
	sink := Sink{Set: opts.Set, OnAdded: opts.OnBanAdded}
	go RunInformer(ctx, BanListerWatcher(client, namespace), sink, opts.Log, opts.SyncTimeout, opts.ResyncInterval)
	opts.Log.Info("moderation k8s source started", "namespace", namespace,
		"resource", moderation.GroupVersionResource.String())
	return nil
}

// restConfig resolves Kubernetes credentials: in-cluster first, then the
// ordinary kubeconfig loading rules (KUBECONFIG, then ~/.kube/config) — the
// same fallback, for the same reason, as gawk-admin's cmd/gawk-admin
// restConfig. In a pod nothing changes: the in-cluster path wins. Outside one
// — the docs/41 compose lane, whose kube-apiserver has no kubelet and
// therefore no pods — KUBECONFIG is how the relay reaches the Ban CRs the
// portal writes. Deliberately NOT a flag: the environment variable is the
// conventional mechanism, and a flag would have to be plumbed through the
// chart (the registryOptions rule) for something a pod would never set.
func restConfig() (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("no in-cluster config and no usable kubeconfig: %w", err)
	}
	return cfg, nil
}

// BanListerWatcher lists and watches every Ban in a namespace.
//
// NOTE THE ABSENCE: no label selector, ever (docs/42 §4.2, §7). Every Ban in
// the namespace is relevant by definition, and a required label would turn a
// break-glass `kubectl apply` that forgot it into a silently unenforced ban.
// The options passed by the reflector are forwarded untouched.
//
// Exported so tests can substitute a fake ListerWatcher (a real API server is
// not available in unit tests).
func BanListerWatcher(client dynamic.Interface, namespace string) cache.ListerWatcher {
	ri := client.Resource(moderation.GroupVersionResource).Namespace(namespace)
	return &cache.ListWatch{
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			return ri.List(context.Background(), options)
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return ri.Watch(context.Background(), options)
		},
	}
}

// Sink is where a source delivers ban records: the Set the publish path
// evaluates from, plus the optional AP3 actuation callback (docs/42 §4.3).
// Bundled rather than passed as two more parameters because every source
// needs both and neither is useful without the other.
type Sink struct {
	// Set is required.
	Set *moderation.Set
	// OnAdded fires once per applied record; nil actuates nothing, which is
	// what a relay wired for enforcement-on-reconnect only would do.
	OnAdded func(moderation.Record)
}

// apply upserts a record and fires the actuation callback. The Set is updated
// FIRST, always: the gate must already be closed when the kill lands, or a
// broadcaster whose session we just terminated could win the race to reclaim
// its own ID before the ban is evaluable (docs/42 §4.1 step 4).
func (s Sink) apply(rec moderation.Record) error {
	if err := s.Set.Upsert(rec); err != nil {
		return err
	}
	if s.OnAdded != nil {
		s.OnAdded(rec)
	}
	return nil
}

// replace swaps the whole list in and then actuates each record.
func (s Sink) replace(recs []moderation.Record) {
	s.Set.Replace(recs)
	if s.OnAdded == nil {
		return
	}
	for _, rec := range recs {
		s.OnAdded(rec)
	}
}

// RunInformer drives a moderation.Set from a Ban ListerWatcher, blocking
// until ctx is cancelled. Exported for the same reason BanListerWatcher is.
func RunInformer(ctx context.Context, lw cache.ListerWatcher, sink Sink, log *slog.Logger, syncTimeout, resync time.Duration) {
	if syncTimeout <= 0 {
		syncTimeout = defaultSyncTimeout
	}
	if resync <= 0 {
		resync = defaultResync
	}
	informer := cache.NewSharedIndexInformer(lw, &unstructured.Unstructured{}, 0, cache.Indexers{})
	_, _ = informer.AddEventHandler(BanEventHandler(sink, log))

	go func() {
		if !waitForSync(ctx, informer, log, syncTimeout) {
			// Only ctx cancellation gets here: the process is going away.
			return
		}
		log.Info("moderation ban informer synced", "bans", len(informer.GetStore().List()))
		replaceFromStore(informer, sink, log)

		// Periodic reconcile of the Set against the informer's store. The
		// Add/Update/Delete stream above is complete on its own (a watch
		// re-list produces the missing deltas), so this exists to heal a Set
		// that drifted — and it is the "resync drives Replace" half of the
		// design. The List-then-Replace window can in principle clobber an
		// event that lands between the two; the next tick heals it, and
		// nothing here is the authority anyway.
		ticker := time.NewTicker(resync)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				replaceFromStore(informer, sink, log)
			}
		}
	}()

	informer.RunWithContext(ctx)
}

// waitForSync blocks until the informer's first LIST has landed, and reports
// false only when ctx is cancelled first.
//
// It RETRIES rather than giving up, and that is the whole point. The docs/42
// §6 residual risk — a relay that cold-starts while the API server is
// unreachable enforces nothing — is made loud by warning every syncTimeout;
// but a single attempt that timed out used to end the goroutine outright,
// which killed replaceFromStore (the initial replace, the periodic drift
// heal, and the re-actuation pass) for the life of the process. A pod that
// started 31 seconds too early would then behave differently from every peer
// forever, with nothing but one Warn to say so.
func waitForSync(ctx context.Context, informer cache.SharedIndexInformer, log *slog.Logger, syncTimeout time.Duration) bool {
	for attempt := 1; ; attempt++ {
		syncCtx, cancel := context.WithTimeout(ctx, syncTimeout)
		synced := cache.WaitForCacheSync(syncCtx.Done(), informer.HasSynced)
		cancel()
		if synced {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
		log.Warn("moderation ban informer has not synced: still enforcing an EMPTY ban set",
			"timeout", syncTimeout, "attempt", attempt)
	}
}

// BanEventHandler maps informer events onto Set mutations. Exported so unit
// tests can drive add/update/delete without an API server.
func BanEventHandler(sink Sink, log *slog.Logger) cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj any) { upsert(sink, log, obj, "add") },
		UpdateFunc: func(old, obj any) {
			if !upsert(sink, log, obj, "update") {
				// The edit did not apply, so nothing superseded the old
				// target: leave it enforced rather than lift a ban on the
				// strength of a spec we could not read.
				return
			}
			liftSupersededTarget(sink, log, old, obj)
		},
		DeleteFunc: func(obj any) {
			// A watch gap can deliver the last-known state in a tombstone
			// rather than the object itself.
			if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
				obj = tomb.Obj
			}
			ban, err := banFrom(obj)
			if err != nil {
				log.Warn("moderation ban CR delete ignored", "err", err)
				return
			}
			if err := sink.Set.Remove(ban.Spec.Target); err != nil {
				log.Warn("moderation ban CR delete ignored", "name", ban.Name, "err", err)
				return
			}
			log.Info("moderation ban removed", "name", ban.Name,
				"target_type", string(ban.Spec.Target.Type))
		},
	}
}

// upsert applies one Ban CR and reports whether it took effect.
func upsert(sink Sink, log *slog.Logger, obj any, why string) bool {
	ban, err := banFrom(obj)
	if err != nil {
		log.Warn("moderation ban CR ignored", "reason", why, "err", err)
		return false
	}
	rec, err := moderation.RecordFromBan(ban)
	if err != nil {
		// An unparseable target is skipped, never widened: a ban nobody can
		// read must not become a ban on everybody.
		log.Warn("moderation ban CR ignored", "name", ban.Name, "reason", why, "err", err)
		return false
	}
	if err := sink.apply(rec); err != nil {
		log.Warn("moderation ban CR ignored", "name", ban.Name, "reason", why, "err", err)
		return false
	}
	// The ban reason is operator-private context (docs/42 §4.3) — Debug only,
	// same rule as the publish-path rejection log.
	log.Info("moderation ban applied", "name", ban.Name, "reason_for_event", why,
		"target_type", string(rec.Target.Type), "expires_at", expiresAtLog(rec))
	log.Debug("moderation ban detail", "name", ban.Name,
		"target", rec.Target.Value, "ban_reason", rec.Reason, "created_by", rec.CreatedBy)
	return true
}

// liftSupersededTarget removes the target a Ban CR used to carry once an
// edit moved it somewhere else. Without this, `kubectl edit` of a CIDR
// enforces BOTH ranges until the next resync (up to ResyncInterval), so
// publishers in a range the operator explicitly stopped banning keep being
// 451'd for minutes.
//
// The Set is keyed by target with no reference counting, so if a second Ban
// CR happens to name the same target, this lifts that one too until the
// resync rebuilds the Set. That is pre-existing behaviour of DeleteFunc
// (which removes the target of the deleted CR regardless of any other CR
// naming it), not something introduced here; duplicate CRs for one target
// are already a shape the design collapses.
func liftSupersededTarget(sink Sink, log *slog.Logger, oldObj, newObj any) {
	oldRec, err := recordOf(oldObj)
	if err != nil {
		// The previous spec was unreadable, so it never entered the Set.
		return
	}
	newRec, err := recordOf(newObj)
	if err != nil || oldRec.Target == newRec.Target {
		return
	}
	if err := sink.Set.Remove(oldRec.Target); err != nil {
		log.Warn("moderation ban target not lifted after an edit", "err", err)
		return
	}
	log.Info("moderation ban target superseded by an edit",
		"target_type", string(oldRec.Target.Type), "new_target_type", string(newRec.Target.Type))
}

// recordOf converts an informer object straight to a normalized Record.
func recordOf(obj any) (moderation.Record, error) {
	ban, err := banFrom(obj)
	if err != nil {
		return moderation.Record{}, err
	}
	return moderation.RecordFromBan(ban)
}

func expiresAtLog(rec moderation.Record) string {
	if rec.ExpiresAt == nil {
		return "never"
	}
	return rec.ExpiresAt.UTC().Format(time.RFC3339)
}

// banFrom converts an informer object into a typed Ban. The informer stores
// *unstructured.Unstructured (there is no generated typed client for this
// CRD); a *moderation.Ban is accepted too so tests can feed typed objects.
func banFrom(obj any) (*moderation.Ban, error) {
	switch o := obj.(type) {
	case *moderation.Ban:
		return o, nil
	case *unstructured.Unstructured:
		var ban moderation.Ban
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(o.Object, &ban); err != nil {
			return nil, fmt.Errorf("ban %q: %w", o.GetName(), err)
		}
		return &ban, nil
	default:
		return nil, fmt.Errorf("unexpected object type %T", obj)
	}
}

func replaceFromStore(informer cache.SharedIndexInformer, sink Sink, log *slog.Logger) {
	objs := informer.GetStore().List()
	records := make([]moderation.Record, 0, len(objs))
	for _, obj := range objs {
		ban, err := banFrom(obj)
		if err != nil {
			log.Warn("moderation ban CR ignored on resync", "err", err)
			continue
		}
		rec, err := moderation.RecordFromBan(ban)
		if err != nil {
			log.Warn("moderation ban CR ignored on resync", "name", ban.Name, "err", err)
			continue
		}
		records = append(records, rec)
	}
	sink.replace(records)
}
