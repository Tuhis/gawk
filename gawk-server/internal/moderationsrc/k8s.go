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
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("moderation source k8s requires in-cluster kubernetes config: %w", err)
	}
	client, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return err
	}
	go RunInformer(ctx, BanListerWatcher(client, namespace), opts.Set, opts.Log, opts.SyncTimeout, opts.ResyncInterval)
	opts.Log.Info("moderation k8s source started", "namespace", namespace,
		"resource", moderation.GroupVersionResource.String())
	return nil
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

// RunInformer drives a moderation.Set from a Ban ListerWatcher, blocking
// until ctx is cancelled. Exported for the same reason BanListerWatcher is.
func RunInformer(ctx context.Context, lw cache.ListerWatcher, set *moderation.Set, log *slog.Logger, syncTimeout, resync time.Duration) {
	if syncTimeout <= 0 {
		syncTimeout = defaultSyncTimeout
	}
	if resync <= 0 {
		resync = defaultResync
	}
	informer := cache.NewSharedIndexInformer(lw, &unstructured.Unstructured{}, 0, cache.Indexers{})
	_, _ = informer.AddEventHandler(BanEventHandler(set, log))

	go func() {
		// The docs/42 §6 residual risk, made loud: a relay that cold-starts
		// while the API server is unreachable enforces nothing, and must say
		// so rather than look healthy.
		syncCtx, cancel := context.WithTimeout(ctx, syncTimeout)
		defer cancel()
		if !cache.WaitForCacheSync(syncCtx.Done(), informer.HasSynced) {
			log.Warn("moderation ban informer has not synced: starting with an EMPTY ban set",
				"timeout", syncTimeout)
			return
		}
		log.Info("moderation ban informer synced", "bans", len(informer.GetStore().List()))
		replaceFromStore(informer, set, log)

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
				replaceFromStore(informer, set, log)
			}
		}
	}()

	informer.RunWithContext(ctx)
}

// BanEventHandler maps informer events onto Set mutations. Exported so unit
// tests can drive add/update/delete without an API server.
func BanEventHandler(set *moderation.Set, log *slog.Logger) cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj any) { upsert(set, log, obj, "add") },
		UpdateFunc: func(_, obj any) { upsert(set, log, obj, "update") },
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
			if err := set.Remove(ban.Spec.Target); err != nil {
				log.Warn("moderation ban CR delete ignored", "name", ban.Name, "err", err)
				return
			}
			log.Info("moderation ban removed", "name", ban.Name,
				"target_type", string(ban.Spec.Target.Type))
		},
	}
}

func upsert(set *moderation.Set, log *slog.Logger, obj any, why string) {
	ban, err := banFrom(obj)
	if err != nil {
		log.Warn("moderation ban CR ignored", "reason", why, "err", err)
		return
	}
	rec, err := moderation.RecordFromBan(ban)
	if err != nil {
		// An unparseable target is skipped, never widened: a ban nobody can
		// read must not become a ban on everybody.
		log.Warn("moderation ban CR ignored", "name", ban.Name, "reason", why, "err", err)
		return
	}
	if err := set.Upsert(rec); err != nil {
		log.Warn("moderation ban CR ignored", "name", ban.Name, "reason", why, "err", err)
		return
	}
	// The ban reason is operator-private context (docs/42 §4.3) — Debug only,
	// same rule as the publish-path rejection log.
	log.Info("moderation ban applied", "name", ban.Name, "reason_for_event", why,
		"target_type", string(rec.Target.Type), "expires_at", expiresAtLog(rec))
	log.Debug("moderation ban detail", "name", ban.Name,
		"target", rec.Target.Value, "ban_reason", rec.Reason, "created_by", rec.CreatedBy)
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

func replaceFromStore(informer cache.SharedIndexInformer, set *moderation.Set, log *slog.Logger) {
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
	set.Replace(records)
}
