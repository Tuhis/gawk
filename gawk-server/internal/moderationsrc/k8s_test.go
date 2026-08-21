package moderationsrc

// R39 AP2 (docs/42 §4.2/§9): the Kubernetes ban source.
//
// envtest is NOT available on this machine, so these drive the real informer
// over a fake dynamic client and a fake ListerWatcher instead. That covers
// the conversion, the Set mutations, the no-label-selector guarantee and the
// unreachable-API-server warning; what it does NOT cover is CRD schema
// validation and RBAC, which need a real API server (kind/envtest — AP3's
// e2e-cluster tier).

import (
	"context"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clientfeatures "k8s.io/client-go/features"
	clientfeaturestesting "k8s.io/client-go/features/testing"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	"github.com/Tuhis/gawk/gawk-server/moderation"
)

const testNamespace = "production"

func banObject(t *testing.T, name string, target moderation.Target, reason string, labels map[string]string) *unstructured.Unstructured {
	t.Helper()
	ban := &moderation.Ban{
		TypeMeta: metav1.TypeMeta{
			APIVersion: moderation.SchemeGroupVersion.String(),
			Kind:       moderation.Kind,
		},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Labels: labels},
		Spec:       moderation.BanSpec{Target: target, Reason: reason, CreatedBy: "kubectl"},
	}
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(ban)
	if err != nil {
		t.Fatalf("ToUnstructured: %v", err)
	}
	return &unstructured.Unstructured{Object: raw}
}

func newFakeDynamic(t *testing.T, objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := moderation.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{moderation.GroupVersionResource: moderation.ListKind},
		objs...)
}

// The no-label-selector guarantee (docs/42 §4.2, §7): a required label would
// turn a break-glass `kubectl apply` that forgot it into a silently
// unenforced ban, so the ListerWatcher must never narrow the watch.
func TestBanListerWatcherSetsNoLabelSelector(t *testing.T) {
	client := newFakeDynamic(t)
	lw := BanListerWatcher(client, testNamespace)

	if _, err := lw.List(metav1.ListOptions{}); err != nil {
		t.Fatalf("List: %v", err)
	}
	w, err := lw.Watch(metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	defer w.Stop()

	sawList, sawWatch := false, false
	for _, a := range client.Actions() {
		switch v := a.(type) {
		case k8stesting.ListActionImpl:
			sawList = true
			if sel := v.GetListRestrictions().Labels; sel != nil && !sel.Empty() {
				t.Errorf("LIST narrowed by label selector %q; every Ban in the namespace must be listed", sel)
			}
		case k8stesting.WatchActionImpl:
			sawWatch = true
			if sel := v.GetWatchRestrictions().Labels; sel != nil && !sel.Empty() {
				t.Errorf("WATCH narrowed by label selector %q; every Ban in the namespace must be watched", sel)
			}
		}
		if a.GetNamespace() != testNamespace {
			t.Errorf("%s action addressed namespace %q, want %q", a.GetVerb(), a.GetNamespace(), testNamespace)
		}
		if a.GetResource() != moderation.GroupVersionResource {
			t.Errorf("%s action addressed %v, want %v", a.GetVerb(), a.GetResource(), moderation.GroupVersionResource)
		}
	}
	if !sawList || !sawWatch {
		t.Fatalf("expected both a list and a watch action, got %v", client.Actions())
	}
}

// recordingListerWatcher captures the options the reflector passes, which is
// where a stray label selector would show up.
type recordingListerWatcher struct {
	mu       sync.Mutex
	listOpts []metav1.ListOptions
	watchOpt []metav1.ListOptions

	items   []unstructured.Unstructured
	watcher *watch.FakeWatcher
}

func (r *recordingListerWatcher) List(options metav1.ListOptions) (runtime.Object, error) {
	r.mu.Lock()
	r.listOpts = append(r.listOpts, options)
	items := append([]unstructured.Unstructured(nil), r.items...)
	r.mu.Unlock()
	list := &unstructured.UnstructuredList{Items: items}
	list.SetAPIVersion(moderation.SchemeGroupVersion.String())
	list.SetKind(moderation.ListKind)
	list.SetResourceVersion("1")
	return list, nil
}

func (r *recordingListerWatcher) Watch(options metav1.ListOptions) (watch.Interface, error) {
	r.mu.Lock()
	r.watchOpt = append(r.watchOpt, options)
	r.mu.Unlock()
	return r.watcher, nil
}

func (r *recordingListerWatcher) selectors() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, o := range r.listOpts {
		out = append(out, o.LabelSelector)
	}
	for _, o := range r.watchOpt {
		out = append(out, o.LabelSelector)
	}
	return out
}

// listWatchInformer builds the informer over a fake ListerWatcher.
//
// client-go 1.35+ defaults the WatchListClient feature ON, which makes the
// reflector open a streaming watch-list and wait for an
// "initial-events-end" bookmark instead of calling List — a protocol a
// watch.FakeWatcher does not speak, so the informer would simply never sync.
// Turning the gate off for the test restores plain LIST+WATCH, which is what
// these tests are driving. Production keeps the default either way: the ban
// informer never inspects the gate.
func withListWatchReflector(t *testing.T) {
	t.Helper()
	clientfeaturestesting.SetFeatureDuringTest(t, clientfeatures.WatchListClient, false)
}

func waitSet(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", desc)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// Add / update / delete drive the Set — and a CR carrying NO labels at all
// enforces, which is the whole point of watching without a selector.
func TestInformerAddUpdateDeleteDrivesSet(t *testing.T) {
	withListWatchReflector(t)
	fw := watch.NewFake()
	lw := &recordingListerWatcher{watcher: fw}
	set := moderation.NewSet()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunInformer(ctx, lw, Sink{Set: set}, discardLog, time.Second, time.Hour)

	// A break-glass ban applied by hand: no labels whatsoever.
	unlabelled := banObject(t, "ban-id-abc23z",
		moderation.Target{Type: moderation.TargetBroadcastID, Value: "ABC23Z"}, "kubectl break-glass", nil)
	if len(unlabelled.GetLabels()) != 0 {
		t.Fatalf("the fixture is supposed to have no labels, got %v", unlabelled.GetLabels())
	}
	fw.Add(unlabelled)
	waitSet(t, "the unlabelled ID ban to enforce", func() bool {
		_, ok := set.BannedID("ABC23Z", time.Now())
		return ok
	})

	ipBan := banObject(t, "ban-ip-deadbeef1234",
		moderation.Target{Type: moderation.TargetIP, Value: "203.0.113.7"}, "abuse", nil)
	fw.Add(ipBan)
	waitSet(t, "the IP ban to enforce", func() bool {
		_, ok := set.BannedIP(netip.MustParseAddr("203.0.113.7"), time.Now())
		return ok
	})

	// Update: widen the CIDR in place.
	widened := banObject(t, "ban-ip-deadbeef1234",
		moderation.Target{Type: moderation.TargetIP, Value: "203.0.113.0/24"}, "abuse", nil)
	fw.Modify(widened)
	waitSet(t, "the widened CIDR to enforce", func() bool {
		_, ok := set.BannedIP(netip.MustParseAddr("203.0.113.99"), time.Now())
		return ok
	})

	// Delete.
	fw.Delete(unlabelled)
	waitSet(t, "the ID ban to lift", func() bool {
		_, ok := set.BannedID("ABC23Z", time.Now())
		return !ok
	})

	for _, sel := range lw.selectors() {
		if sel != "" {
			t.Fatalf("the informer narrowed its watch with label selector %q; every Ban in the namespace must be watched", sel)
		}
	}
}

// The initial LIST enforces too — a relay that restarts must come up already
// enforcing, not wait for the next watch event.
func TestInformerEnforcesTheInitialList(t *testing.T) {
	withListWatchReflector(t)
	fw := watch.NewFake()
	lw := &recordingListerWatcher{watcher: fw}
	lw.items = []unstructured.Unstructured{
		*banObject(t, "ban-id-abc23z",
			moderation.Target{Type: moderation.TargetBroadcastID, Value: "ABC23Z"}, "existing", nil),
	}
	set := moderation.NewSet()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunInformer(ctx, lw, Sink{Set: set}, discardLog, time.Second, time.Hour)

	waitSet(t, "the listed ban to enforce", func() bool {
		_, ok := set.BannedID("ABC23Z", time.Now())
		return ok
	})
}

// An unparseable target is skipped, never widened: a ban nobody can read must
// not become a ban on everybody, and it must not stop the rest of the list.
func TestInformerSkipsUnparseableBans(t *testing.T) {
	withListWatchReflector(t)
	fw := watch.NewFake()
	lw := &recordingListerWatcher{watcher: fw}
	set := moderation.NewSet()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go RunInformer(ctx, lw, Sink{Set: set}, discardLog, time.Second, time.Hour)

	fw.Add(banObject(t, "ban-bogus",
		moderation.Target{Type: "publisher", Value: "whatever"}, "bad type", nil))
	fw.Add(banObject(t, "ban-id-abc23z",
		moderation.Target{Type: moderation.TargetBroadcastID, Value: "ABC23Z"}, "good", nil))

	waitSet(t, "the valid ban to enforce", func() bool {
		_, ok := set.BannedID("ABC23Z", time.Now())
		return ok
	})
	if _, ok := set.BannedIP(netip.MustParseAddr("203.0.113.7"), time.Now()); ok {
		t.Error("an unparseable target became a match")
	}
	if got := set.ActiveCounts(time.Now()); got["broadcastId"] != 1 || got["ip"] != 0 {
		t.Errorf("ActiveCounts = %v, want exactly the one valid ban", got)
	}
}

// Editing a Ban CR's spec.target must LIFT the old target, not merely add the
// new one. Before the fix `kubectl edit` of a CIDR left the previous range
// enforced until the next resync (up to five minutes), so innocent publishers
// in the old range kept getting 451. (PR #280 review.)
func TestInformerUpdateLiftsTheSupersededTarget(t *testing.T) {
	withListWatchReflector(t)
	fw := watch.NewFake()
	lw := &recordingListerWatcher{watcher: fw}
	set := moderation.NewSet()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// An hour between resyncs, so only UpdateFunc can be what lifts the old
	// target — the resync heal would otherwise mask the bug.
	go RunInformer(ctx, lw, Sink{Set: set}, discardLog, time.Second, time.Hour)

	fw.Add(banObject(t, "ban-ip-deadbeef1234",
		moderation.Target{Type: moderation.TargetIP, Value: "203.0.113.0/24"}, "abuse", nil))
	waitSet(t, "the original CIDR ban", func() bool {
		_, ok := set.BannedIP(netip.MustParseAddr("203.0.113.7"), time.Now())
		return ok
	})

	// The operator MOVED the ban to another range; they did not widen it to
	// cover both.
	fw.Modify(banObject(t, "ban-ip-deadbeef1234",
		moderation.Target{Type: moderation.TargetIP, Value: "198.51.100.0/24"}, "abuse", nil))
	waitSet(t, "the edited CIDR ban", func() bool {
		_, ok := set.BannedIP(netip.MustParseAddr("198.51.100.9"), time.Now())
		return ok
	})
	if _, ok := set.BannedIP(netip.MustParseAddr("203.0.113.7"), time.Now()); ok {
		t.Error("the superseded CIDR is still enforced — publishers in the old range stay 451'd until the next resync")
	}
	if got := set.ActiveCounts(time.Now()); got["ip"] != 1 {
		t.Errorf("ActiveCounts = %v, want exactly the edited ban", got)
	}

	// The same for an ID ban, whose old value is otherwise unreachable: no
	// CR names it any more, so only the update event can lift it.
	fw.Add(banObject(t, "ban-id-abc23z",
		moderation.Target{Type: moderation.TargetBroadcastID, Value: "ABC23Z"}, "fraud", nil))
	waitSet(t, "the original ID ban", func() bool {
		_, ok := set.BannedID("ABC23Z", time.Now())
		return ok
	})
	fw.Modify(banObject(t, "ban-id-abc23z",
		moderation.Target{Type: moderation.TargetBroadcastID, Value: "ZZZ23Z"}, "fraud", nil))
	waitSet(t, "the edited ID ban", func() bool {
		_, ok := set.BannedID("ZZZ23Z", time.Now())
		return ok
	})
	if _, ok := set.BannedID("ABC23Z", time.Now()); ok {
		t.Error("the superseded broadcast ID is still banned after the edit")
	}

	// An edit whose NEW target is unreadable lifts nothing: the direction of
	// safety is the same one upsert already takes — a ban nobody can parse
	// is skipped, never widened, and never turned into an accidental unban.
	fw.Modify(banObject(t, "ban-id-abc23z",
		moderation.Target{Type: moderation.TargetBroadcastID, Value: "!!!"}, "fat fingers", nil))
	time.Sleep(50 * time.Millisecond)
	if _, ok := set.BannedID("ZZZ23Z", time.Now()); !ok {
		t.Error("an unparseable edit lifted the ban that was in force")
	}
}

// A watch gap delivers the last-known state in a tombstone; the delete must
// still land.
func TestBanEventHandlerHandlesTombstones(t *testing.T) {
	set := moderation.NewSet()
	h := BanEventHandler(Sink{Set: set}, discardLog)
	obj := banObject(t, "ban-id-abc23z",
		moderation.Target{Type: moderation.TargetBroadcastID, Value: "ABC23Z"}, "x", nil)

	h.OnAdd(obj, false)
	if _, ok := set.BannedID("ABC23Z", time.Now()); !ok {
		t.Fatal("OnAdd did not enforce")
	}
	h.OnDelete(cache.DeletedFinalStateUnknown{Key: testNamespace + "/ban-id-abc23z", Obj: obj})
	if _, ok := set.BannedID("ABC23Z", time.Now()); ok {
		t.Error("a tombstoned delete did not lift the ban")
	}

	// Junk never panics and never enforces.
	h.OnAdd("not an object", false)
	h.OnDelete(42)
	h.OnUpdate(nil, struct{}{})
}

// docs/42 §6, the honest residual risk: a relay cold-starting while the API
// server is unreachable enforces NOTHING, and must say so at Warn rather than
// look healthy.
func TestInformerWarnsWhenItCannotSync(t *testing.T) {
	buf := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A ListerWatcher that never answers stands in for an unreachable API
	// server: the reflector's LIST simply never completes.
	lw := &stallingListerWatcher{done: ctx.Done()}
	set := moderation.NewSet()
	go RunInformer(ctx, lw, Sink{Set: set}, log, 50*time.Millisecond, time.Hour)

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), "EMPTY ban set") {
		if time.Now().After(deadline) {
			t.Fatalf("no empty-ban-set warning was logged:\n%s", buf.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A relay that cold-starts during a brief API-server outage must not be
// permanently different from its peers. Before the fix the resync goroutine
// returned on the first WaitForCacheSync timeout, so `replaceFromStore` —
// the initial replace AND the periodic pass that heals a drifted Set and
// re-fires OnBanAdded — was dead for the life of the process: a pod that
// started 31 seconds too early behaved differently forever. (PR #280 review.)
func TestInformerHealsAfterAFailedFirstSync(t *testing.T) {
	withListWatchReflector(t)
	buf := &syncBuffer{}
	log := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rec := &recordingListerWatcher{watcher: watch.NewFake()}
	rec.items = []unstructured.Unstructured{
		*banObject(t, "ban-id-abc23z",
			moderation.Target{Type: moderation.TargetBroadcastID, Value: "ABC23Z"}, "existing", nil),
	}
	lw := &gatedListerWatcher{recordingListerWatcher: rec, gate: make(chan struct{}), done: ctx.Done()}

	set := moderation.NewSet()
	var got actuations
	go RunInformer(ctx, lw, Sink{Set: set, OnAdded: got.add}, log,
		30*time.Millisecond, 20*time.Millisecond)

	// The API server is unreachable, and the relay says so (docs/42 §6).
	waitSet(t, "the empty-ban-set warning", func() bool {
		return strings.Contains(buf.String(), "EMPTY ban set")
	})

	// ...and then it comes back, well after the first sync attempt gave up.
	close(lw.gate)
	waitSet(t, "the listed ban to enforce once the API server returns", func() bool {
		_, ok := set.BannedID("ABC23Z", time.Now())
		return ok
	})

	// The healing pass is what is actually at stake. A Set that drifted — a
	// delete missed in a watch gap, or the List/Replace clobber window the
	// implementation admits to — is repaired only by the periodic
	// store→Set reconcile, and nothing else in the informer removes a ban
	// the store does not have.
	if err := set.Upsert(moderation.Record{
		Target: moderation.Target{Type: moderation.TargetBroadcastID, Value: "ZZZ23Z"},
	}); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	waitSet(t, "the resync to heal the drifted ban set", func() bool {
		_, ok := set.BannedID("ZZZ23Z", time.Now())
		return !ok
	})

	// The other half of what the resync is for: re-actuating, so a publisher
	// that raced the admission gate is still killed on a later pass.
	idTarget := moderation.Target{Type: moderation.TargetBroadcastID, Value: "ABC23Z"}
	waitSet(t, "the resync to re-actuate the ban", func() bool {
		n := 0
		for _, target := range got.targets() {
			if target == idTarget {
				n++
			}
		}
		return n > 1
	})
}

// gatedListerWatcher stalls LIST and WATCH until its gate is closed: an API
// server that is unreachable for a while and then returns.
type gatedListerWatcher struct {
	*recordingListerWatcher
	gate chan struct{}
	done <-chan struct{}
}

func (g *gatedListerWatcher) List(options metav1.ListOptions) (runtime.Object, error) {
	select {
	case <-g.gate:
	case <-g.done:
		return nil, context.Canceled
	}
	return g.recordingListerWatcher.List(options)
}

func (g *gatedListerWatcher) Watch(options metav1.ListOptions) (watch.Interface, error) {
	select {
	case <-g.gate:
	case <-g.done:
		return nil, context.Canceled
	}
	return g.recordingListerWatcher.Watch(options)
}

type stallingListerWatcher struct{ done <-chan struct{} }

func (s *stallingListerWatcher) List(metav1.ListOptions) (runtime.Object, error) {
	<-s.done
	return nil, context.Canceled
}

func (s *stallingListerWatcher) Watch(metav1.ListOptions) (watch.Interface, error) {
	<-s.done
	return nil, context.Canceled
}

type syncBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
