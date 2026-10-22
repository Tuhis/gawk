package popsrc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"

	"github.com/Tuhis/gawk/gawk-server/pops"
)

// PopListerWatcher lists and watches every Pop in a namespace. No label
// selector, for the reason the Room and Ban informers have none: a
// `kubectl apply` that forgot a label must not produce a PoP nobody serves.
func PopListerWatcher(client dynamic.Interface, namespace string) cache.ListerWatcher {
	ri := client.Resource(pops.GroupVersionResource).Namespace(namespace)
	return &cache.ListWatch{
		ListFunc: func(options metav1.ListOptions) (runtime.Object, error) {
			return ri.List(context.Background(), options)
		},
		WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
			return ri.Watch(context.Background(), options)
		},
	}
}

type k8sSource struct {
	*store
	informer cache.SharedIndexInformer
	log      *slog.Logger
}

// StartK8s starts an informer over the namespace's Pop CRs and returns at
// once; Synced turns true when the first list has landed. The informer runs
// until ctx ends. A CR that does not convert, or whose name is not a
// DNS-1123 label (it must be a gawk/pop label value), is skipped with a
// warning, never served.
func StartK8s(ctx context.Context, dc dynamic.Interface, ns string, log *slog.Logger) (Source, error) {
	if dc == nil {
		return nil, errors.New("popsrc: a dynamic client is required")
	}
	if log == nil {
		log = slog.Default()
	}
	s := &k8sSource{store: newStore(), log: log}
	s.informer = cache.NewSharedIndexInformer(PopListerWatcher(dc, ns), &unstructured.Unstructured{}, 0, cache.Indexers{})
	if _, err := s.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    s.observe,
		UpdateFunc: func(_, obj any) { s.observe(obj) },
		DeleteFunc: s.observeDelete,
	}); err != nil {
		return nil, fmt.Errorf("popsrc: informer handler: %w", err)
	}
	go s.informer.RunWithContext(ctx)
	go func() {
		if cache.WaitForCacheSync(ctx.Done(), s.informer.HasSynced) {
			s.synced.Store(true)
			log.Info("pop informer synced", "pops", len(s.informer.GetStore().List()))
		}
	}()
	return s, nil
}

func popFrom(obj any) (*pops.Pop, error) {
	switch o := obj.(type) {
	case *pops.Pop:
		return o.DeepCopy(), nil
	case *unstructured.Unstructured:
		var p pops.Pop
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(o.Object, &p); err != nil {
			return nil, fmt.Errorf("pop CR %q: %w", o.GetName(), err)
		}
		return &p, nil
	default:
		return nil, fmt.Errorf("unexpected object type %T", obj)
	}
}

func (s *k8sSource) observe(obj any) {
	p, err := popFrom(obj)
	if err != nil {
		s.log.Warn("pop CR skipped", "err", err)
		if u, ok := obj.(*unstructured.Unstructured); ok && s.remove(u.GetName()) {
			s.notify([]string{u.GetName()})
		}
		return
	}
	if err := pops.ValidateName(p.Name); err != nil {
		s.log.Warn("pop CR skipped: its name cannot be a gawk/pop label value", "pop", p.Name, "err", err)
		return
	}
	if s.put(p) {
		s.notify([]string{p.Name})
	}
}

func (s *k8sSource) observeDelete(obj any) {
	if tomb, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tomb.Obj
	}
	acc, err := metaName(obj)
	if err != nil {
		return
	}
	if s.remove(acc) {
		s.notify([]string{acc})
	}
}

func metaName(obj any) (string, error) {
	switch o := obj.(type) {
	case *unstructured.Unstructured:
		return o.GetName(), nil
	case *pops.Pop:
		return o.Name, nil
	default:
		return "", fmt.Errorf("unexpected object type %T", obj)
	}
}
