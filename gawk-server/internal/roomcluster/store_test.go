package roomcluster

// R42 RM3 acceptance (docs/44 §9): the home-pod lease and the Room CR
// lifecycle against the fake dynamic client — two claimants ⇒ one holder,
// a stale generation loses on renew and writes nothing afterwards, the
// janitor deletes only stale-and-empty dynamic rooms, the informer's static
// refresh / delete paths, Reserve's error mapping, and status.key.
//
// The fake tracker has no resourceVersion continuity (it never bumps or
// checks one), so a reactor here supplies the CAS the real API server
// enforces: every update must carry the stored resourceVersion or it is a
// Conflict, and a successful write bumps it. Without that, "two claimants"
// would both win and the test would prove nothing.

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clientfeatures "k8s.io/client-go/features"
	clientfeaturestesting "k8s.io/client-go/features/testing"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Tuhis/gawk/gawk-server/internal/roomsrv"
	"github.com/Tuhis/gawk/gawk-server/rooms"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

const testNamespace = "gawk"

var discardLog = slog.New(slog.NewTextHandler(io.Discard, nil))

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)} }

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

// recordingRegistry records what the store asked the registry to do.
type recordingRegistry struct {
	mu       sync.Mutex
	upserts  []roomsrv.StaticRoom
	adopted  []*rooms.Room
	ended    []string
	adoptRet bool
}

func (r *recordingRegistry) UpsertStatic(def roomsrv.StaticRoom) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.upserts = append(r.upserts, def)
	return nil
}

func (r *recordingRegistry) AdoptDynamic(cr *rooms.Room) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.adopted = append(r.adopted, cr.DeepCopy())
	return !r.adoptRet
}

func (r *recordingRegistry) EndRoom(code string, reason uint8) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ended = append(r.ended, code+"/"+strconv.Itoa(int(reason)))
}

func (r *recordingRegistry) snapshot() (upserts []roomsrv.StaticRoom, adopted []*rooms.Room, ended []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]roomsrv.StaticRoom(nil), r.upserts...), append([]*rooms.Room(nil), r.adopted...), append([]string(nil), r.ended...)
}

// withListWatchReflector turns the WatchListClient gate off so the reflector
// speaks plain LIST+WATCH, which is what the fake tracker implements (the
// same reason moderationsrc's tests do it).
func withListWatchReflector(t *testing.T) {
	t.Helper()
	clientfeaturestesting.SetFeatureDuringTest(t, clientfeatures.WatchListClient, false)
}

func newFakeDynamic(t *testing.T, objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	t.Helper()
	// An EMPTY scheme, on purpose: with the typed Room registered the fake
	// converts every listed item to rooms.Room and then cannot hand it back
	// as the *unstructured.Unstructured the informer asked for ("can't
	// assign or convert unstructured.Unstructured into rooms.Room"). The
	// production client is unstructured end to end anyway.
	scheme := runtime.NewScheme()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{rooms.GroupVersionResource: rooms.ListKind},
		objs...)
	installCAS(t, client)
	return client
}

// installCAS gives the fake tracker resourceVersion semantics for rooms:
// a create stamps "1", an update must present the stored version (else
// Conflict) and bumps it. Objects seeded through NewSimpleDynamicClient
// carry whatever version the test gave them ("" reads as 0).
func installCAS(t *testing.T, client *dynamicfake.FakeDynamicClient) {
	t.Helper()
	var mu sync.Mutex
	stored := func(ns, name string) (string, bool) {
		obj, err := client.Tracker().Get(rooms.GroupVersionResource, ns, name)
		if err != nil {
			return "", false
		}
		acc, _ := obj.(*unstructured.Unstructured)
		if acc == nil {
			return "", true
		}
		return acc.GetResourceVersion(), true
	}
	client.PrependReactor("create", "rooms", func(action k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		obj := action.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
		obj.SetResourceVersion("1")
		return false, nil, nil
	})
	client.PrependReactor("update", "rooms", func(action k8stesting.Action) (bool, runtime.Object, error) {
		mu.Lock()
		defer mu.Unlock()
		obj := action.(k8stesting.UpdateAction).GetObject().(*unstructured.Unstructured)
		cur, ok := stored(action.GetNamespace(), obj.GetName())
		if !ok {
			return true, nil, apierrors.NewNotFound(rooms.GroupVersionResource.GroupResource(), obj.GetName())
		}
		if obj.GetResourceVersion() != cur {
			return true, nil, apierrors.NewConflict(rooms.GroupVersionResource.GroupResource(), obj.GetName(),
				errors.New("resourceVersion mismatch"))
		}
		n, _ := strconv.Atoi(cur)
		obj.SetResourceVersion(strconv.Itoa(n + 1))
		return false, nil, nil
	})
}

// roomWatchRegistered returns a channel closed once a Room watch has been
// registered with the fake tracker — from then on every mutation reaches
// the informer. Install before Run, like cluster_test's leaseWatchRegistered.
func roomWatchRegistered(client *dynamicfake.FakeDynamicClient) <-chan struct{} {
	registered := make(chan struct{})
	var once sync.Once
	client.PrependWatchReactor("rooms", func(action k8stesting.Action) (bool, watch.Interface, error) {
		w, err := client.Tracker().Watch(rooms.GroupVersionResource, action.GetNamespace())
		if err != nil {
			return false, nil, err
		}
		once.Do(func() { close(registered) })
		return true, w, nil
	})
	return registered
}

func roomObject(t *testing.T, r *rooms.Room) *unstructured.Unstructured {
	t.Helper()
	r.Namespace = testNamespace
	u, err := toUnstructured(r)
	if err != nil {
		t.Fatal(err)
	}
	if u.GetResourceVersion() == "" {
		u.SetResourceVersion("1")
	}
	return u
}

func staticCR(name string, secret *rooms.SecretKeyRef) *rooms.Room {
	r := &rooms.Room{Spec: rooms.RoomSpec{Kind: rooms.KindStatic, DisplayCode: "TuhisRoom", DisplayName: "Tuhis' room", AttachSecretRef: secret, MaxBroadcasts: 2}}
	r.Name = name
	return r
}

func dynamicCR(name string, lease *rooms.Lease, emptySince *time.Time, attachments ...rooms.Attachment) *rooms.Room {
	r := &rooms.Room{Spec: rooms.RoomSpec{Kind: rooms.KindDynamic}}
	r.Name = name
	r.Status = rooms.RoomStatus{CreatorTokenFingerprint: "abcd", Lease: lease, Attachments: attachments}
	if emptySince != nil {
		mt := metav1.NewTime(*emptySince)
		r.Status.EmptySince = &mt
	}
	return r
}

func secretObject(name string, data map[string]string) *unstructured.Unstructured {
	enc := map[string]any{}
	for k, v := range data {
		enc[k] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Secret",
		"metadata": map[string]any{"name": name, "namespace": testNamespace},
		"data":     enc,
	}}
}

type testStore struct {
	*Store
	reg   *recordingRegistry
	clock *fakeClock
	lost  chan string
}

func newTestStore(t *testing.T, client *dynamicfake.FakeDynamicClient, pod string, clock *fakeClock, mutate func(*Options)) *testStore {
	t.Helper()
	reg := &recordingRegistry{}
	lost := make(chan string, 8)
	opts := Options{
		Client:        client,
		Namespace:     testNamespace,
		PodName:       pod,
		AdvertiseAddr: pod + ":4433",
		Registry:      reg,
		Obfuscate:     func(s string) string { return "key-" + s },
		Log:           discardLog,
		OnLeaseLost:   func(code string) { lost <- code },
		LeaseDuration: 15 * time.Second,
		RenewInterval: 20 * time.Millisecond,
		StaleWindow:   5 * time.Minute,
		EmptyGrace:    time.Minute,
		SyncTimeout:   time.Second,
		Now:           clock.Now,
	}
	if mutate != nil {
		mutate(&opts)
	}
	s, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		s.mu.Lock()
		held := make([]*heldLease, 0, len(s.held))
		for _, h := range s.held {
			held = append(held, h)
		}
		s.mu.Unlock()
		for _, h := range held {
			h.cancel()
		}
	})
	return &testStore{Store: s, reg: reg, clock: clock, lost: lost}
}

// runSynced starts the informer and waits for its first list.
func runSynced(t *testing.T, ctx context.Context, client *dynamicfake.FakeDynamicClient, s *Store) {
	t.Helper()
	watching := roomWatchRegistered(client)
	go s.Run(ctx)
	select {
	case <-watching:
	case <-time.After(15 * time.Second):
		t.Fatal("room watch never registered")
	}
	waitFor(t, 15*time.Second, s.HasSynced, "informer sync")
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

func getRoom(t *testing.T, client *dynamicfake.FakeDynamicClient, name string) *rooms.Room {
	t.Helper()
	u, err := client.Resource(rooms.GroupVersionResource).Namespace(testNamespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get room %s: %v", name, err)
	}
	r, err := roomFrom(u)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Reserve creates the CR as the atomic reservation with this pod as home,
// generation 1, the HMAC'd key and emptySince (mint starts the grace); a
// second reservation of the same code is "taken" — neither of the two
// pass-through errors — so Mint re-mints.
func TestReserveCreatesTheCRAndMapsAlreadyExistsToTaken(t *testing.T) {
	withListWatchReflector(t)
	client := newFakeDynamic(t)
	clock := newFakeClock()
	a := newTestStore(t, client, "pod-a", clock, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runSynced(t, ctx, client, a.Store)

	cr := &rooms.Room{Spec: rooms.RoomSpec{Kind: rooms.KindDynamic}}
	cr.Name = "k7xq2m"
	cr.Status.CreatorTokenFingerprint = "fp"
	if err := a.Reserve(ctx, cr); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	got := getRoom(t, client, "k7xq2m")
	if got.Status.Key != "key-k7xq2m" {
		t.Errorf("status.key = %q, want the obfuscated code", got.Status.Key)
	}
	if l := got.Status.Lease; l == nil || l.Holder != "pod-a" || l.Addr != "pod-a:4433" || l.Generation != 1 || l.RenewedAt == nil {
		t.Errorf("lease = %+v, want pod-a gen 1", got.Status.Lease)
	}
	if got.Status.EmptySince == nil || got.Status.CreatorTokenFingerprint != "fp" || got.Status.CreatedAt == nil {
		t.Errorf("status = %+v", got.Status)
	}
	if gen, held := a.Holding("K7XQ2M"); !held || gen != 1 {
		t.Errorf("Holding = %d,%v; want 1,true", gen, held)
	}

	err := a.Reserve(ctx, cr.DeepCopy())
	if err == nil || errors.Is(err, roomsrv.ErrUnavailable) || errors.Is(err, roomsrv.ErrMaxRooms) {
		t.Fatalf("second Reserve = %v, want a 'taken' error that is neither pass-through", err)
	}
}

func TestReserveMapsAPIFailureToUnavailableAndFleetLimitToMaxRooms(t *testing.T) {
	withListWatchReflector(t)
	now := time.Now()
	client := newFakeDynamic(t,
		roomObject(t, dynamicCR("aaaaaa", nil, &now)),
		roomObject(t, dynamicCR("bbbbbb", nil, &now)),
		roomObject(t, staticCR("tuhisroom", nil)))
	clock := newFakeClock()
	a := newTestStore(t, client, "pod-a", clock, func(o *Options) { o.MaxRooms = 2 })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runSynced(t, ctx, client, a.Store)

	cr := &rooms.Room{Spec: rooms.RoomSpec{Kind: rooms.KindDynamic}}
	cr.Name = "cccccc"
	// Two dynamic rooms exist fleet-wide (the static one does not count):
	// the limit of 2 binds at create.
	if err := a.Reserve(ctx, cr.DeepCopy()); !errors.Is(err, roomsrv.ErrMaxRooms) {
		t.Fatalf("Reserve at the fleet limit = %v, want ErrMaxRooms", err)
	}

	b := newTestStore(t, client, "pod-b", clock, func(o *Options) { o.MaxRooms = 10 })
	runSynced(t, ctx, client, b.Store)
	client.PrependReactor("create", "rooms", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("etcd is away")
	})
	if err := b.Reserve(ctx, cr.DeepCopy()); !errors.Is(err, roomsrv.ErrUnavailable) {
		t.Fatalf("Reserve with the API down = %v, want ErrUnavailable", err)
	}
	if _, held := b.Holding("cccccc"); held {
		t.Error("a failed reservation left a held lease behind")
	}
}

// Two claimants ⇒ one holder (docs/44 RM3 acceptance): both pods race for
// a static room nobody holds; the CAS makes exactly one the home, and the
// other proxies (ErrHeldElsewhere).
func TestTwoClaimantsOneHolder(t *testing.T) {
	withListWatchReflector(t)
	client := newFakeDynamic(t, roomObject(t, staticCR("tuhisroom", nil)))
	clock := newFakeClock()
	a := newTestStore(t, client, "pod-a", clock, nil)
	b := newTestStore(t, client, "pod-b", clock, nil)
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, s := range []*testStore{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.Adopt(ctx, "TuhisRoom")
		}()
	}
	wg.Wait()

	wins := 0
	for i, err := range errs {
		switch {
		case err == nil:
			wins++
		case errors.Is(err, ErrHeldElsewhere):
		default:
			t.Fatalf("claimant %d: unexpected error %v", i, err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d winners, want exactly one (errs %v)", wins, errs)
	}
	got := getRoom(t, client, "tuhisroom")
	_, heldA := a.Holding("tuhisroom")
	_, heldB := b.Holding("tuhisroom")
	if heldA == heldB {
		t.Fatalf("held by a=%v b=%v, want exactly one", heldA, heldB)
	}
	winner := "pod-a"
	winReg := a.reg
	if heldB {
		winner, winReg = "pod-b", b.reg
	}
	if got.Status.Lease.Holder != winner || got.Status.Lease.Generation != 1 || got.Status.Key != "key-tuhisroom" {
		t.Errorf("lease after the race = %+v key=%q, want holder %s gen 1", got.Status.Lease, got.Status.Key, winner)
	}
	// The winner populated its registry with the static definition; the
	// loser did not.
	ups, _, _ := winReg.snapshot()
	if len(ups) != 1 || ups[0].Code != "tuhisroom" || ups[0].DisplayCode != "TuhisRoom" || ups[0].MaxBroadcasts != 2 {
		t.Errorf("winner upserts = %+v", ups)
	}
	loser := a.reg
	if heldA {
		loser = b.reg
	}
	if ups, _, _ := loser.snapshot(); len(ups) != 0 {
		t.Errorf("loser upserted %+v", ups)
	}
}

// A stale generation loses: after a force-take (the lease went stale and
// another pod adopted), the old home's renew finds the generation rejected,
// demotes itself (OnLeaseLost), and its status writes are refused — a
// stale home never fights the new one (docs/44 §4.5 "Fencing").
func TestStaleGenerationLosesOnRenewAndWritesNothing(t *testing.T) {
	now := time.Now()
	client := newFakeDynamic(t, roomObject(t, dynamicCR("k7xq2m", nil, &now, rooms.Attachment{BroadcastID: "5UP4XW"})))
	clock := newFakeClock()
	a := newTestStore(t, client, "pod-a", clock, func(o *Options) { o.RenewInterval = time.Hour })
	b := newTestStore(t, client, "pod-b", clock, nil)
	ctx := context.Background()

	if err := a.Adopt(ctx, "k7xq2m"); err != nil {
		t.Fatalf("a.Adopt: %v", err)
	}
	_, adopted, _ := a.reg.snapshot()
	if len(adopted) != 1 || len(adopted[0].Status.Attachments) != 1 || adopted[0].Status.Attachments[0].BroadcastID != "5UP4XW" {
		t.Fatalf("AdoptDynamic got %+v, want the CR with its attachment", adopted)
	}
	// While live, b cannot take it.
	if err := b.Adopt(ctx, "k7xq2m"); !errors.Is(err, ErrHeldElsewhere) {
		t.Fatalf("b.Adopt against a live home = %v, want ErrHeldElsewhere", err)
	}
	// a stops renewing (crash); past the lease duration b force-takes.
	clock.Advance(16 * time.Second)
	if err := b.Adopt(ctx, "k7xq2m"); err != nil {
		t.Fatalf("b.Adopt after staleness: %v", err)
	}
	got := getRoom(t, client, "k7xq2m")
	if got.Status.Lease.Holder != "pod-b" || got.Status.Lease.Generation != 2 {
		t.Fatalf("lease after force-take = %+v", got.Status.Lease)
	}

	// a's next renew is rejected and demotes it.
	a.mu.Lock()
	h := a.held["k7xq2m"]
	a.mu.Unlock()
	if err := a.renewOnce(ctx, "k7xq2m", h); !errors.Is(err, errLost) {
		t.Fatalf("stale renew = %v, want errLost", err)
	}
	select {
	case code := <-a.lost:
		if code != "k7xq2m" {
			t.Fatalf("OnLeaseLost(%q)", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnLeaseLost never fired")
	}
	if _, held := a.Holding("k7xq2m"); held {
		t.Error("a still believes it holds the room")
	}
	// a's late status writes are refused; b's land.
	a.AttachmentsChanged("k7xq2m", nil)
	a.RoomEmpty("k7xq2m", true)
	b.AttachmentsChanged("k7xq2m", []rooms.Attachment{{BroadcastID: "5UP4XW"}, {BroadcastID: "AB2CD3", Label: "laptop"}})
	b.RoomEmpty("k7xq2m", false)
	got = getRoom(t, client, "k7xq2m")
	if len(got.Status.Attachments) != 2 || got.Status.EmptySince != nil || got.Status.Lease.Holder != "pod-b" {
		t.Errorf("status after the fence = %+v", got.Status)
	}
	// a ending "its" room must not delete b's CR.
	a.RoomEnded("k7xq2m", wire.RoomEndReasonEmpty)
	getRoom(t, client, "k7xq2m")
	// b ending it does.
	b.RoomEnded("k7xq2m", wire.RoomEndReasonCreator)
	if _, err := client.Resource(rooms.GroupVersionResource).Namespace(testNamespace).Get(ctx, "k7xq2m", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("CR after the home ended the room: err=%v, want NotFound", err)
	}
}

// Drain: Release clears the holder and keeps the generation, so the next
// join claims without waiting for staleness.
func TestReleaseAllClearsHolderKeepsGeneration(t *testing.T) {
	client := newFakeDynamic(t, roomObject(t, staticCR("tuhisroom", nil)))
	clock := newFakeClock()
	a := newTestStore(t, client, "pod-a", clock, nil)
	b := newTestStore(t, client, "pod-b", clock, nil)
	ctx := context.Background()
	if err := a.Adopt(ctx, "tuhisroom"); err != nil {
		t.Fatal(err)
	}
	a.ReleaseAll(ctx)
	got := getRoom(t, client, "tuhisroom")
	if got.Status.Lease.Holder != "" || got.Status.Lease.Addr != "" || got.Status.Lease.Generation != 1 {
		t.Fatalf("lease after release = %+v, want empty holder at gen 1", got.Status.Lease)
	}
	if _, held := a.Holding("tuhisroom"); held {
		t.Error("released lease still held")
	}
	if home, ok := a.Resolve("tuhisroom"); ok && home.Live {
		t.Errorf("Resolve after release reports live: %+v", home)
	}
	// No staleness wait: b claims immediately, at generation 2.
	if err := b.Adopt(ctx, "tuhisroom"); err != nil {
		t.Fatalf("b.Adopt after release: %v", err)
	}
	if got := getRoom(t, client, "tuhisroom"); got.Status.Lease.Holder != "pod-b" || got.Status.Lease.Generation != 2 {
		t.Errorf("lease after re-claim = %+v", got.Status.Lease)
	}
}

// The janitor deletes only dynamic rooms that are BOTH stale past the long
// window AND empty longer than the grace — never static rooms, never a
// stale room with participants (emptySince unset), never a fresh one.
func TestJanitorDeletesOnlyStaleAndEmptyDynamicRooms(t *testing.T) {
	withListWatchReflector(t)
	clock := newFakeClock()
	now := clock.Now()
	old := now.Add(-10 * time.Minute)
	recent := now.Add(-time.Second)
	staleLease := &rooms.Lease{Holder: "pod-dead", Generation: 3, RenewedAt: ptrTime(old)}
	freshLease := &rooms.Lease{Holder: "pod-b", Generation: 1, RenewedAt: ptrTime(recent)}
	client := newFakeDynamic(t,
		roomObject(t, dynamicCR("garbage", staleLease, &old)),                                            // stale + empty past grace → deleted
		roomObject(t, dynamicCR("stalebusy", staleLease, nil)),                                           // stale, roster unknown → kept for adoption
		roomObject(t, dynamicCR("fresh", freshLease, &old)),                                              // live home → kept
		roomObject(t, dynamicCR("justemptied", staleLease, &recent)),                                     // empty inside the grace → kept
		roomObject(t, dynamicCR("released", &rooms.Lease{Generation: 2, RenewedAt: ptrTime(old)}, &old)), // drained and empty → deleted
		roomObject(t, staticCR("tuhisroom", nil)),                                                        // static → never
	)
	a := newTestStore(t, client, "pod-a", clock, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runSynced(t, ctx, client, a.Store)

	a.JanitorSweep(ctx)
	ri := client.Resource(rooms.GroupVersionResource).Namespace(testNamespace)
	for _, name := range []string{"garbage", "released"} {
		if _, err := ri.Get(ctx, name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
			t.Errorf("%s survived the janitor (err=%v)", name, err)
		}
	}
	for _, name := range []string{"stalebusy", "fresh", "justemptied", "tuhisroom"} {
		if _, err := ri.Get(ctx, name, metav1.GetOptions{}); err != nil {
			t.Errorf("%s was janitored: %v", name, err)
		}
	}
}

func ptrTime(t time.Time) *metav1.Time {
	mt := metav1.NewTime(t)
	return &mt
}

// The informer: a held static room's spec edit (attach-secret rotation)
// refreshes the registry; a CR deletion ends the room locally with the
// operator reason; a foreign lease on a held room is a loss.
func TestInformerRefreshesHeldStaticRoomsAndEndsDeletedOnes(t *testing.T) {
	withListWatchReflector(t)
	client := newFakeDynamic(t,
		roomObject(t, staticCR("tuhisroom", &rooms.SecretKeyRef{Name: "room-tuhisroom", Key: "attachSecret"})),
		secretObject("room-tuhisroom", map[string]string{"attachSecret": "hunter2"}),
		roomObject(t, dynamicCR("k7xq2m", nil, nil)),
	)
	clock := newFakeClock()
	a := newTestStore(t, client, "pod-a", clock, func(o *Options) { o.RenewInterval = time.Hour })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runSynced(t, ctx, client, a.Store)

	// Nothing is upserted merely because a static CR exists: a static room
	// has no home until its first join (docs/44 §4.5 "Placement").
	if ups, _, _ := a.reg.snapshot(); len(ups) != 0 {
		t.Fatalf("static CR upserted before any claim: %+v", ups)
	}
	if !a.Known("TuhisRoom") || !a.Known("K7XQ2M") || a.Known("nosuch") {
		t.Fatal("Known does not reflect the informer cache")
	}
	if err := a.Adopt(ctx, "tuhisroom"); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	ups, _, _ := a.reg.snapshot()
	if len(ups) != 1 || ups[0].AttachSecret != "hunter2" {
		t.Fatalf("adopted static definition = %+v, want the Secret's attach secret", ups)
	}

	// Rotate the secret and bump the CR: the held room is refreshed.
	ri := client.Resource(rooms.GroupVersionResource).Namespace(testNamespace)
	_, _ = client.Resource(secretGVR).Namespace(testNamespace).Update(ctx, secretObject("room-tuhisroom", map[string]string{"attachSecret": "rotated"}), metav1.UpdateOptions{})
	cur, _ := ri.Get(ctx, "tuhisroom", metav1.GetOptions{})
	_ = unstructured.SetNestedField(cur.Object, "Renamed", "spec", "displayName")
	if _, err := ri.Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 15*time.Second, func() bool {
		ups, _, _ := a.reg.snapshot()
		return len(ups) >= 2 && ups[len(ups)-1].AttachSecret == "rotated" && ups[len(ups)-1].DisplayName == "Renamed"
	}, "the rotated secret to reach the registry")
	if _, held := a.Holding("tuhisroom"); !held {
		t.Fatal("a spec edit by the operator cost the home its lease")
	}

	// The operator deletes the CR: the room ends locally (4007 operator).
	if err := ri.Delete(ctx, "tuhisroom", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 15*time.Second, func() bool {
		_, _, ended := a.reg.snapshot()
		return len(ended) == 1 && ended[0] == "tuhisroom/"+strconv.Itoa(wire.RoomEndReasonOperator)
	}, "EndRoom(operator) on CR deletion")
	if _, held := a.Holding("tuhisroom"); held {
		t.Error("deleted room still held")
	}

	// A held dynamic room whose lease is rewritten by another pod is lost.
	if err := a.Adopt(ctx, "k7xq2m"); err != nil {
		t.Fatal(err)
	}
	b := newTestStore(t, client, "pod-b", clock, nil)
	if _, err := b.Claim(ctx, "k7xq2m", true); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-a.lost:
		if code != "k7xq2m" {
			t.Fatalf("lost %q", code)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the informer never reported the force-take")
	}
	// A dynamic CR deleted by someone else ends the room on the new home.
	if err := ri.Delete(ctx, "k7xq2m", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 15*time.Second, func() bool {
		_, _, ended := a.reg.snapshot()
		return len(ended) == 2
	}, "EndRoom on the dynamic CR deletion")
}

// A static room whose Secret cannot be read is not homed here: attach must
// never silently open because the key was unreadable.
func TestAdoptStaticFailsClosedWithoutTheSecret(t *testing.T) {
	client := newFakeDynamic(t, roomObject(t, staticCR("tuhisroom", &rooms.SecretKeyRef{Name: "missing", Key: "k"})))
	a := newTestStore(t, client, "pod-a", newFakeClock(), nil)
	err := a.Adopt(context.Background(), "tuhisroom")
	if !errors.Is(err, roomsrv.ErrUnavailable) {
		t.Fatalf("Adopt = %v, want ErrUnavailable", err)
	}
	if _, held := a.Holding("tuhisroom"); held {
		t.Error("a room that could not be built is held")
	}
	if ups, _, _ := a.reg.snapshot(); len(ups) != 0 {
		t.Errorf("registry got %+v", ups)
	}
	if err := a.Adopt(context.Background(), "nosuch"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Adopt(nosuch) = %v, want ErrNotFound", err)
	}
}

// The renew loop keeps renewedAt moving and Resolve reports the holder as
// live; once it stops the lease goes stale and Resolve says so.
func TestRenewLoopKeepsTheLeaseLive(t *testing.T) {
	withListWatchReflector(t)
	client := newFakeDynamic(t, roomObject(t, dynamicCR("k7xq2m", nil, nil)))
	clock := newFakeClock()
	a := newTestStore(t, client, "pod-a", clock, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runSynced(t, ctx, client, a.Store)
	// Installed BEFORE the renew loop exists: the fake's reactor chain is a
	// plain slice, so appending to it under a running goroutine is a race.
	var renews atomic.Int32
	client.PrependReactor("update", "rooms", func(action k8stesting.Action) (bool, runtime.Object, error) {
		renews.Add(1)
		return false, nil, nil
	})
	if err := a.Adopt(ctx, "k7xq2m"); err != nil {
		t.Fatal(err)
	}
	renews.Store(0)
	clock.Advance(10 * time.Second)
	waitFor(t, 15*time.Second, func() bool { return renews.Load() >= 1 }, "a renew")
	waitFor(t, 15*time.Second, func() bool {
		home, ok := a.Resolve("k7xq2m")
		return ok && home.Live && home.Holder == "pod-a" && home.Addr == "pod-a:4433"
	}, "the renewed lease to reach the cache")
	a.stopRenew("k7xq2m")
	clock.Advance(20 * time.Second)
	if home, _ := a.Resolve("k7xq2m"); home.Live {
		t.Errorf("lease still live after the renew loop stopped: %+v", home)
	}
}
