package roomcluster

// The store's failure paths (docs/44 §4.5), which the happy-path lease
// tests in store_test.go never reach: option validation and defaults,
// objects the informer hands over that are not Rooms, an API that answers
// with errors or endless conflicts on every write path (Claim, Adopt,
// Reserve's post-create status write, patchStatus, Release, renewOnce,
// RoomEnded's get and delete), the renew loop's warn-and-keep-going, the
// informer's junk/tombstone handling, Run's sync timeout, and the janitor
// ticking on its own interval. Every branch that logs is asserted through
// the log — a Warn nobody can grep is the same as no Warn.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"

	"github.com/Tuhis/gawk/gawk-server/internal/roomsrv"
	"github.com/Tuhis/gawk/gawk-server/rooms"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *logBuffer) contains(s string) bool { return strings.Contains(b.String(), s) }

// loggingStore is newTestStore with a capturing logger and the renew loop
// parked (an hour), so reactors can be installed after a claim without
// racing the loop's reads of the reactor chain.
func loggingStore(t *testing.T, client *dynamicfake.FakeDynamicClient, pod string, mutate func(*Options)) (*testStore, *logBuffer) {
	t.Helper()
	logs := &logBuffer{}
	s := newTestStore(t, client, pod, newFakeClock(), func(o *Options) {
		o.Log = slog.New(slog.NewTextHandler(logs, nil))
		o.RenewInterval = time.Hour
		if mutate != nil {
			mutate(o)
		}
	})
	return s, logs
}

type k8sFake = interface {
	PrependReactor(verb, resource string, reaction k8stesting.ReactionFunc)
}

// failRooms makes every <verb> on rooms answer 503.
func failRooms(client k8sFake, verb string) {
	client.PrependReactor(verb, "rooms", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("etcd is away")
	})
}

// failRoomsAfter lets the first n <verb> calls through and fails the rest.
func failRoomsAfter(client k8sFake, verb string, n int32) *atomic.Int32 {
	var calls atomic.Int32
	client.PrependReactor(verb, "rooms", func(k8stesting.Action) (bool, runtime.Object, error) {
		if calls.Add(1) <= n {
			return false, nil, nil
		}
		return true, nil, apierrors.NewServiceUnavailable("etcd is away")
	})
	return &calls
}

// failingRegistry is a recordingRegistry whose UpsertStatic can be made to
// fail (a registry-side rejection of a static definition).
type failingRegistry struct {
	recordingRegistry
	upsertErr error
}

func (r *failingRegistry) UpsertStatic(def roomsrv.StaticRoom) error {
	if r.upsertErr != nil {
		return r.upsertErr
	}
	return r.recordingRegistry.UpsertStatic(def)
}

func TestNewValidatesOptionsAndAppliesDefaults(t *testing.T) {
	client := newFakeDynamic(t)
	for _, tc := range []struct {
		name string
		opts Options
		want string
	}{
		{"no client", Options{}, "Client is required"},
		{"no identity", Options{Client: client}, "Namespace, PodName and AdvertiseAddr"},
		{"no advertise addr", Options{Client: client, Namespace: "gawk", PodName: "pod-a"}, "Namespace, PodName and AdvertiseAddr"},
		{"no registry", Options{Client: client, Namespace: "gawk", PodName: "pod-a", AdvertiseAddr: "pod-a:4433"}, "Registry is required"},
	} {
		if _, err := New(tc.opts); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: New = %v, want an error mentioning %q", tc.name, err, tc.want)
		}
	}
	s, err := New(Options{Client: client, Namespace: "gawk", PodName: "pod-a", AdvertiseAddr: "pod-a:4433", Registry: &recordingRegistry{}})
	if err != nil {
		t.Fatalf("New(minimal) = %v", err)
	}
	o := s.opts
	if o.LeaseDuration != DefaultLeaseDuration || o.RenewInterval != DefaultRenewInterval || o.JanitorInterval != DefaultJanitorInterval ||
		o.StaleWindow != DefaultStaleWindow || o.SyncTimeout != defaultSyncTimeout || o.EmptyGrace != roomsrv.DefaultEmptyGrace {
		t.Errorf("defaults not applied: %+v", o)
	}
	if o.Now == nil || o.Log == nil || o.Obfuscate == nil || o.Obfuscate("k7xq2m") != "k7xq2m" {
		t.Errorf("nil seams not defaulted: now=%v log=%v obfuscate=%v", o.Now != nil, o.Log != nil, o.Obfuscate != nil)
	}
}

func brokenRoomObject(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": rooms.SchemeGroupVersion.String(), "kind": rooms.Kind,
		"metadata": map[string]any{"name": name, "namespace": testNamespace, "resourceVersion": "1"},
		"spec":     "not a spec",
	}}
}

// roomFrom takes typed Rooms and unstructured ones; anything else — or an
// unstructured object whose shape is not a Room — is an error, and the
// cache lookups treat such an object as absent rather than panicking.
func TestRoomFromAndCacheRejectMalformedObjects(t *testing.T) {
	typed := staticCR("tuhisroom", nil)
	if got, err := roomFrom(typed); err != nil || got != typed {
		t.Fatalf("roomFrom(typed) = %v, %v", got, err)
	}
	if _, err := roomFrom(brokenRoomObject("broken")); err == nil || !strings.Contains(err.Error(), `room "broken"`) {
		t.Fatalf("roomFrom(broken unstructured) = %v, want a named conversion error", err)
	}
	if _, err := roomFrom("junk"); err == nil || !strings.Contains(err.Error(), "unexpected object type") {
		t.Fatalf("roomFrom(string) = %v", err)
	}

	withListWatchReflector(t)
	client := newFakeDynamic(t, brokenRoomObject("broken"), roomObject(t, dynamicCR("k7xq2m", nil, nil)))
	a, logs := loggingStore(t, client, "pod-a", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runSynced(t, ctx, client, a.Store)
	waitFor(t, 15*time.Second, func() bool { return logs.contains("room CR ignored") }, "the informer to log the broken CR")
	if a.Known("broken") {
		t.Error("a CR that does not convert is Known")
	}
	if _, ok := a.Lookup("broken"); ok {
		t.Error("a CR that does not convert is Lookup-able")
	}
	if n := a.countDynamic(); n != 1 {
		t.Errorf("countDynamic = %d, want 1 (the broken CR is skipped)", n)
	}
}

func TestLookupReturnsTheCachedCR(t *testing.T) {
	withListWatchReflector(t)
	client := newFakeDynamic(t, roomObject(t, staticCR("tuhisroom", nil)))
	a := newTestStore(t, client, "pod-a", newFakeClock(), nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runSynced(t, ctx, client, a.Store)
	r, ok := a.Lookup("TuhisRoom")
	if !ok || r.Name != "tuhisroom" || r.Spec.Kind != rooms.KindStatic || r.Spec.DisplayCode != "TuhisRoom" {
		t.Fatalf("Lookup = %+v, %v", r, ok)
	}
	for _, code := range []string{"nosuch", "bad_code", ""} {
		if _, ok := a.Lookup(code); ok {
			t.Errorf("Lookup(%q) found something", code)
		}
	}
	if s := a.stale(nil, time.Second); !s {
		t.Error("a nil lease is not stale")
	}
	if s := a.stale(&rooms.Lease{Holder: "pod-b"}, time.Second); !s {
		t.Error("a lease without renewedAt is not stale")
	}
}

// Claim's own vocabulary: bad and unknown codes are ErrNotFound; an API
// that cannot be read or written is ErrUnavailable; a write that conflicts
// on every retry is reported as exhausted, and nothing is held afterwards.
func TestClaimErrorBranches(t *testing.T) {
	ctx := context.Background()
	t.Run("codes", func(t *testing.T) {
		a := newTestStore(t, newFakeDynamic(t), "pod-a", newFakeClock(), nil)
		if _, err := a.Claim(ctx, "bad_code", false); !errors.Is(err, ErrNotFound) {
			t.Errorf("Claim(bad code) = %v", err)
		}
		if _, err := a.Claim(ctx, "nosuch", false); !errors.Is(err, ErrNotFound) {
			t.Errorf("Claim(unknown) = %v", err)
		}
		if err := a.Adopt(ctx, "bad_code"); !errors.Is(err, ErrNotFound) {
			t.Errorf("Adopt(bad code) = %v", err)
		}
	})
	t.Run("get unavailable", func(t *testing.T) {
		client := newFakeDynamic(t, roomObject(t, staticCR("tuhisroom", nil)))
		a := newTestStore(t, client, "pod-a", newFakeClock(), nil)
		failRooms(client, "get")
		if _, err := a.Claim(ctx, "tuhisroom", false); !errors.Is(err, roomsrv.ErrUnavailable) {
			t.Errorf("Claim = %v, want ErrUnavailable", err)
		}
		if err := a.Adopt(ctx, "tuhisroom"); !errors.Is(err, roomsrv.ErrUnavailable) {
			t.Errorf("Adopt = %v, want ErrUnavailable", err)
		}
		if err := a.Release(ctx, "tuhisroom"); !errors.Is(err, roomsrv.ErrUnavailable) {
			t.Errorf("Release = %v, want ErrUnavailable", err)
		}
	})
	t.Run("update conflicts forever", func(t *testing.T) {
		client := newFakeDynamic(t, roomObject(t, staticCR("tuhisroom", nil)))
		a := newTestStore(t, client, "pod-a", newFakeClock(), nil)
		client.PrependReactor("update", "rooms", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewConflict(rooms.GroupVersionResource.GroupResource(), "tuhisroom", errors.New("always"))
		})
		_, err := a.Claim(ctx, "tuhisroom", false)
		if err == nil || !strings.Contains(err.Error(), "claim retries exhausted") || !apierrors.IsConflict(err) {
			t.Errorf("Claim = %v, want retries exhausted wrapping the conflict", err)
		}
		if _, held := a.Holding("tuhisroom"); held {
			t.Error("an unwritten claim is held")
		}
	})
	t.Run("update unavailable", func(t *testing.T) {
		client := newFakeDynamic(t, roomObject(t, staticCR("tuhisroom", nil)))
		a := newTestStore(t, client, "pod-a", newFakeClock(), nil)
		failRooms(client, "update")
		if _, err := a.Claim(ctx, "tuhisroom", false); !errors.Is(err, roomsrv.ErrUnavailable) {
			t.Errorf("Claim = %v, want ErrUnavailable", err)
		}
		if _, held := a.Holding("tuhisroom"); held {
			t.Error("an unwritten claim is held")
		}
	})
}

// Reserve creates the CR, then writes the lease through the status
// subresource; when that second write fails the CR is deleted again so
// the code is not left reserved by nobody.
func TestReserveRollsBackWhenTheStatusWriteFails(t *testing.T) {
	ctx := context.Background()
	client := newFakeDynamic(t)
	a := newTestStore(t, client, "pod-a", newFakeClock(), nil)
	failRooms(client, "update")
	cr := &rooms.Room{Spec: rooms.RoomSpec{Kind: rooms.KindDynamic}}
	cr.Name = "k7xq2m"
	if err := a.Reserve(ctx, cr); !errors.Is(err, roomsrv.ErrUnavailable) {
		t.Fatalf("Reserve = %v, want ErrUnavailable", err)
	}
	if _, err := client.Resource(rooms.GroupVersionResource).Namespace(testNamespace).Get(ctx, "k7xq2m", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("CR after the rollback: err=%v, want NotFound", err)
	}
	if _, held := a.Holding("k7xq2m"); held {
		t.Error("a rolled-back reservation is held")
	}
}

// patchStatus (behind RoomEmpty and AttachmentsChanged) writes nothing
// unless this pod holds the lease at the generation the CR carries, maps
// a vanished CR to ErrNotFound, retries conflicts a bounded number of
// times, and surfaces a dead API as ErrUnavailable — the last two logged
// by the callers, the first two silently (they are the expected shape of
// a room ending or moving).
func TestPatchStatusBranches(t *testing.T) {
	ctx := context.Background()
	noop := func(*rooms.RoomStatus) {}
	t.Run("not held and bad code", func(t *testing.T) {
		client := newFakeDynamic(t, roomObject(t, staticCR("tuhisroom", nil)))
		a, logs := loggingStore(t, client, "pod-a", nil)
		if err := a.patchStatus(ctx, "tuhisroom", noop); !errors.Is(err, errLost) {
			t.Errorf("patchStatus(unheld) = %v, want errLost", err)
		}
		if err := a.patchStatus(ctx, "bad_code", noop); !errors.Is(err, ErrNotFound) {
			t.Errorf("patchStatus(bad code) = %v, want ErrNotFound", err)
		}
		a.RoomEmpty("tuhisroom", true)
		a.AttachmentsChanged("tuhisroom", nil)
		if logs.contains("write failed") {
			t.Errorf("an unheld room's status writes were logged as failures:\n%s", logs)
		}
	})
	t.Run("CR gone under a held lease", func(t *testing.T) {
		client := newFakeDynamic(t, roomObject(t, staticCR("tuhisroom", nil)))
		a, logs := loggingStore(t, client, "pod-a", nil)
		if err := a.Adopt(ctx, "tuhisroom"); err != nil {
			t.Fatal(err)
		}
		if err := client.Resource(rooms.GroupVersionResource).Namespace(testNamespace).Delete(ctx, "tuhisroom", metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := a.patchStatus(ctx, "tuhisroom", noop); !errors.Is(err, ErrNotFound) {
			t.Errorf("patchStatus(deleted) = %v, want ErrNotFound", err)
		}
		a.RoomEmpty("tuhisroom", false)
		if logs.contains("write failed") {
			t.Errorf("a deleted room's status write was logged as a failure:\n%s", logs)
		}
	})
	t.Run("lease moved under a held record", func(t *testing.T) {
		// The CR says pod-b at generation 2 while this pod still believes
		// generation 1 (the informer has not delivered the loss yet).
		client := newFakeDynamic(t, roomObject(t, staticCR("tuhisroom", nil)))
		a, _ := loggingStore(t, client, "pod-a", nil)
		if err := a.Adopt(ctx, "tuhisroom"); err != nil {
			t.Fatal(err)
		}
		b := newTestStore(t, client, "pod-b", newFakeClock(), nil)
		if _, err := b.Claim(ctx, "tuhisroom", true); err != nil {
			t.Fatal(err)
		}
		if err := a.patchStatus(ctx, "tuhisroom", noop); !errors.Is(err, errLost) {
			t.Errorf("patchStatus(moved) = %v, want errLost", err)
		}
	})
	t.Run("conflicts exhausted and API down are logged", func(t *testing.T) {
		client := newFakeDynamic(t, roomObject(t, staticCR("tuhisroom", nil)))
		a, logs := loggingStore(t, client, "pod-a", nil)
		if err := a.Adopt(ctx, "tuhisroom"); err != nil {
			t.Fatal(err)
		}
		var conflicts atomic.Int32
		client.PrependReactor("update", "rooms", func(k8stesting.Action) (bool, runtime.Object, error) {
			conflicts.Add(1)
			return true, nil, apierrors.NewConflict(rooms.GroupVersionResource.GroupResource(), "tuhisroom", errors.New("always"))
		})
		err := a.patchStatus(ctx, "tuhisroom", noop)
		if err == nil || !strings.Contains(err.Error(), "retries exhausted") {
			t.Errorf("patchStatus(conflicts) = %v, want retries exhausted", err)
		}
		if n := conflicts.Load(); n != claimRetries {
			t.Errorf("update attempts = %d, want %d", n, claimRetries)
		}
		a.RoomEmpty("tuhisroom", true)
		a.AttachmentsChanged("tuhisroom", []rooms.Attachment{{BroadcastID: "AB2CD3"}})
		if !logs.contains("room emptySince write failed") || !logs.contains("room attachments write failed") {
			t.Errorf("exhausted writes not logged:\n%s", logs)
		}
		failRooms(client, "update")
		if err := a.patchStatus(ctx, "tuhisroom", noop); !errors.Is(err, roomsrv.ErrUnavailable) {
			t.Errorf("patchStatus(API down) = %v, want ErrUnavailable", err)
		}
	})
}

// RoomEnded: a code with no CR is silent; a static room is never deleted;
// a get or delete the API refuses is logged, not retried forever.
func TestRoomEndedBranches(t *testing.T) {
	ctx := context.Background()
	t.Run("unknown and static", func(t *testing.T) {
		client := newFakeDynamic(t, roomObject(t, staticCR("tuhisroom", nil)))
		a, logs := loggingStore(t, client, "pod-a", nil)
		a.RoomEnded("nosuch", wire.RoomEndReasonEmpty)
		if err := a.Adopt(ctx, "tuhisroom"); err != nil {
			t.Fatal(err)
		}
		a.RoomEnded("tuhisroom", wire.RoomEndReasonOperator)
		getRoom(t, client, "tuhisroom")
		if _, held := a.Holding("tuhisroom"); held {
			t.Error("RoomEnded left the renew loop running")
		}
		if logs.contains("room end:") {
			t.Errorf("unexpected failure logs:\n%s", logs)
		}
	})
	t.Run("get fails", func(t *testing.T) {
		client := newFakeDynamic(t, roomObject(t, dynamicCR("k7xq2m", nil, nil)))
		a, logs := loggingStore(t, client, "pod-a", nil)
		if err := a.Adopt(ctx, "k7xq2m"); err != nil {
			t.Fatal(err)
		}
		failRooms(client, "get")
		a.RoomEnded("k7xq2m", wire.RoomEndReasonEmpty)
		if !logs.contains("room end: get failed") {
			t.Errorf("get failure not logged:\n%s", logs)
		}
	})
	t.Run("delete fails", func(t *testing.T) {
		client := newFakeDynamic(t, roomObject(t, dynamicCR("k7xq2m", nil, nil)))
		a, logs := loggingStore(t, client, "pod-a", nil)
		if err := a.Adopt(ctx, "k7xq2m"); err != nil {
			t.Fatal(err)
		}
		failRooms(client, "delete")
		a.RoomEnded("k7xq2m", wire.RoomEndReasonEmpty)
		if !logs.contains("room end: delete failed") {
			t.Errorf("delete failure not logged:\n%s", logs)
		}
		getRoom(t, client, "k7xq2m")
	})
}

// Release: nothing to do for an unknown CR, an unclaimed one, or one held
// by another pod; ReleaseAll logs a release the API refused.
func TestReleaseBranches(t *testing.T) {
	ctx := context.Background()
	client := newFakeDynamic(t, roomObject(t, staticCR("tuhisroom", nil)), roomObject(t, dynamicCR("k7xq2m", nil, nil)))
	a, logs := loggingStore(t, client, "pod-a", nil)
	b := newTestStore(t, client, "pod-b", newFakeClock(), nil)
	if err := a.Release(ctx, "nosuch"); err != nil {
		t.Errorf("Release(unknown) = %v", err)
	}
	if err := a.Release(ctx, "tuhisroom"); err != nil {
		t.Errorf("Release(unclaimed) = %v", err)
	}
	if err := b.Adopt(ctx, "tuhisroom"); err != nil {
		t.Fatal(err)
	}
	if err := a.Release(ctx, "tuhisroom"); err != nil {
		t.Errorf("Release(held by pod-b) = %v", err)
	}
	if got := getRoom(t, client, "tuhisroom"); got.Status.Lease.Holder != "pod-b" {
		t.Errorf("pod-a's release touched pod-b's lease: %+v", got.Status.Lease)
	}
	if err := a.Adopt(ctx, "k7xq2m"); err != nil {
		t.Fatal(err)
	}
	failRooms(client, "get")
	a.ReleaseAll(ctx)
	if !logs.contains("room lease release failed during drain") {
		t.Errorf("refused release not logged:\n%s", logs)
	}
	if _, held := a.Holding("k7xq2m"); held {
		t.Error("ReleaseAll left a held record for a lease it could not release (the renew loop must stop regardless)")
	}
}

// renewOnce: a CR that vanished drops the held record silently (the
// informer's delete ends the room); an unreadable or unwritable API is a
// plain error that keeps the lease held; and the loop logs such errors and
// keeps trying rather than demoting the home.
func TestRenewFailureBranches(t *testing.T) {
	ctx := context.Background()
	t.Run("CR vanished", func(t *testing.T) {
		client := newFakeDynamic(t, roomObject(t, dynamicCR("k7xq2m", nil, nil)))
		a, _ := loggingStore(t, client, "pod-a", nil)
		if err := a.Adopt(ctx, "k7xq2m"); err != nil {
			t.Fatal(err)
		}
		if err := client.Resource(rooms.GroupVersionResource).Namespace(testNamespace).Delete(ctx, "k7xq2m", metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		a.mu.Lock()
		h := a.held["k7xq2m"]
		a.mu.Unlock()
		if err := a.renewOnce(ctx, "k7xq2m", h); !errors.Is(err, errLost) {
			t.Errorf("renewOnce(vanished) = %v, want errLost", err)
		}
		if _, held := a.Holding("k7xq2m"); held {
			t.Error("a vanished CR is still held")
		}
		select {
		case code := <-a.lost:
			t.Errorf("OnLeaseLost(%q) fired for a CR that was deleted, not taken", code)
		default:
		}
	})
	t.Run("API errors keep the lease", func(t *testing.T) {
		client := newFakeDynamic(t, roomObject(t, dynamicCR("k7xq2m", nil, nil)))
		a, _ := loggingStore(t, client, "pod-a", nil)
		if err := a.Adopt(ctx, "k7xq2m"); err != nil {
			t.Fatal(err)
		}
		a.mu.Lock()
		h := a.held["k7xq2m"]
		a.mu.Unlock()
		failRooms(client, "update")
		if err := a.renewOnce(ctx, "k7xq2m", h); err == nil || errors.Is(err, errLost) {
			t.Errorf("renewOnce(update down) = %v, want a plain error", err)
		}
		failRooms(client, "get")
		if err := a.renewOnce(ctx, "k7xq2m", h); !errors.Is(err, roomsrv.ErrUnavailable) || errors.Is(err, errLost) {
			t.Errorf("renewOnce(get down) = %v, want ErrUnavailable", err)
		}
		if _, held := a.Holding("k7xq2m"); !held {
			t.Error("an API outage cost the home its lease")
		}
	})
	t.Run("the loop logs and keeps going", func(t *testing.T) {
		client := newFakeDynamic(t, roomObject(t, dynamicCR("k7xq2m", nil, nil)))
		// The claim's own status write is the first update; every renew
		// after it fails. Installed before the loop exists (no race).
		failRoomsAfter(client, "update", 1)
		logs := &logBuffer{}
		a := newTestStore(t, client, "pod-a", newFakeClock(), func(o *Options) {
			o.Log = slog.New(slog.NewTextHandler(logs, nil))
			o.RenewInterval = 10 * time.Millisecond
		})
		if err := a.Adopt(ctx, "k7xq2m"); err != nil {
			t.Fatal(err)
		}
		waitFor(t, 15*time.Second, func() bool { return logs.contains("room lease renew failed") }, "the renew failure log")
		if _, held := a.Holding("k7xq2m"); !held {
			t.Error("renew failures demoted the home")
		}
	})
}

// Adopt's later failures: the re-read after the claim failing (the lease is
// dropped again), a local copy already present (still a success), and a
// static definition the registry refuses (not held).
func TestAdoptLateFailures(t *testing.T) {
	ctx := context.Background()
	t.Run("re-read fails", func(t *testing.T) {
		client := newFakeDynamic(t, roomObject(t, dynamicCR("k7xq2m", nil, nil)))
		a, _ := loggingStore(t, client, "pod-a", nil)
		// Adopt's get, Claim's get, then the re-read: fail the third.
		failRoomsAfter(client, "get", 2)
		if err := a.Adopt(ctx, "k7xq2m"); !errors.Is(err, roomsrv.ErrUnavailable) {
			t.Fatalf("Adopt = %v, want ErrUnavailable", err)
		}
		if _, held := a.Holding("k7xq2m"); held {
			t.Error("a room that could not be rebuilt is held")
		}
		if _, adopted, _ := a.reg.snapshot(); len(adopted) != 0 {
			t.Errorf("registry adopted %+v", adopted)
		}
	})
	t.Run("local copy already present", func(t *testing.T) {
		client := newFakeDynamic(t, roomObject(t, dynamicCR("k7xq2m", nil, nil)))
		a, logs := loggingStore(t, client, "pod-a", nil)
		a.reg.adoptRet = true
		if err := a.Adopt(ctx, "k7xq2m"); err != nil {
			t.Fatalf("Adopt = %v", err)
		}
		if _, held := a.Holding("k7xq2m"); !held {
			t.Error("not held after a successful adoption")
		}
		if !logs.contains("room adopted") {
			t.Errorf("adoption not logged:\n%s", logs)
		}
	})
	t.Run("registry refuses the static definition", func(t *testing.T) {
		client := newFakeDynamic(t, roomObject(t, staticCR("tuhisroom", nil)))
		reg := &failingRegistry{upsertErr: errors.New("registry says no")}
		a, _ := loggingStore(t, client, "pod-a", func(o *Options) { o.Registry = reg })
		if err := a.Adopt(ctx, "tuhisroom"); err == nil || !strings.Contains(err.Error(), "registry says no") {
			t.Fatalf("Adopt = %v, want the registry's error", err)
		}
		if _, held := a.Holding("tuhisroom"); held {
			t.Error("a room the registry refused is held")
		}
	})
}

// staticDef fails closed on a Secret that exists but has no such key, or a
// value that is not base64.
func TestStaticDefFailsClosedOnBadSecrets(t *testing.T) {
	ctx := context.Background()
	badB64 := secretObject("room-bad", nil)
	_ = unstructured.SetNestedField(badB64.Object, "!!not base64!!", "data", "attachSecret")
	client := newFakeDynamic(t,
		roomObject(t, staticCR("nokey", &rooms.SecretKeyRef{Name: "room-nokey", Key: "attachSecret"})),
		roomObject(t, staticCR("badb64", &rooms.SecretKeyRef{Name: "room-bad", Key: "attachSecret"})),
		secretObject("room-nokey", map[string]string{"other": "x"}),
		badB64,
	)
	a := newTestStore(t, client, "pod-a", newFakeClock(), nil)
	for _, code := range []string{"nokey", "badb64"} {
		if err := a.Adopt(ctx, code); !errors.Is(err, roomsrv.ErrUnavailable) {
			t.Errorf("Adopt(%s) = %v, want ErrUnavailable", code, err)
		}
		if _, held := a.Holding(code); held {
			t.Errorf("%s held with an unreadable attach secret", code)
		}
	}
}

// The informer handlers: junk objects are logged and ignored; a delete
// tombstone (DeletedFinalStateUnknown) is unwrapped and ends the room; a
// held static room whose spec refresh fails — unreadable secret, or a
// registry rejection — is logged and the lease kept.
func TestObserveHandlersOnJunkTombstonesAndFailedRefresh(t *testing.T) {
	ctx := context.Background()
	client := newFakeDynamic(t, roomObject(t, staticCR("tuhisroom", nil)))
	a, logs := loggingStore(t, client, "pod-a", nil)

	a.observe(nil, "junk")
	a.observeDelete("junk")
	if !logs.contains("room CR ignored") || !logs.contains("room CR delete ignored") {
		t.Errorf("junk objects not logged:\n%s", logs)
	}
	if _, _, ended := a.reg.snapshot(); len(ended) != 0 {
		t.Errorf("junk ended a room: %v", ended)
	}

	if err := a.Adopt(ctx, "tuhisroom"); err != nil {
		t.Fatal(err)
	}
	cur := getRoom(t, client, "tuhisroom")
	edited := cur.DeepCopy()
	edited.Spec.AttachSecretRef = &rooms.SecretKeyRef{Name: "missing", Key: "attachSecret"}
	a.observe(roomObject(t, cur), roomObject(t, edited))
	waitFor(t, 15*time.Second, func() bool { return logs.contains("static room spec refresh failed") }, "the failed refresh log")
	if _, held := a.Holding("tuhisroom"); !held {
		t.Error("a failed spec refresh cost the home its lease")
	}

	// The registry refusing the refreshed definition is logged too.
	a.opts.Registry = &failingRegistry{upsertErr: errors.New("registry says no")}
	renamed := cur.DeepCopy()
	renamed.Spec.DisplayName = "Renamed"
	a.observe(roomObject(t, cur), roomObject(t, renamed))
	waitFor(t, 15*time.Second, func() bool { return logs.contains("static room refresh rejected") }, "the rejected refresh log")
	a.opts.Registry = a.reg

	// A tombstone for the held room ends it and drops the lease.
	a.observeDelete(cache.DeletedFinalStateUnknown{Key: testNamespace + "/tuhisroom", Obj: roomObject(t, cur)})
	if _, _, ended := a.reg.snapshot(); len(ended) != 1 || ended[0] != "tuhisroom/"+strconv.Itoa(wire.RoomEndReasonOperator) {
		t.Errorf("tombstone ended %v, want tuhisroom/operator", ended)
	}
	if _, held := a.Holding("tuhisroom"); held {
		t.Error("a tombstoned room is still held")
	}
}

// Run: when the first list never lands within SyncTimeout the store says
// so (rooms homed elsewhere are unresolvable until it does) and keeps
// trying; cancelling the context ends it without a second warning.
func TestRunWarnsWhenTheInformerDoesNotSync(t *testing.T) {
	withListWatchReflector(t)
	client := newFakeDynamic(t)
	failRooms(client, "list")
	logs := &logBuffer{}
	a := newTestStore(t, client, "pod-a", newFakeClock(), func(o *Options) {
		o.Log = slog.New(slog.NewTextHandler(logs, nil))
		o.SyncTimeout = 50 * time.Millisecond
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()
	waitFor(t, 15*time.Second, func() bool { return logs.contains("room informer has not synced") }, "the sync warning")
	if a.HasSynced() {
		t.Error("HasSynced with every list failing")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// The janitor runs on its own ticker: a stale, long-empty dynamic room is
// deleted without anyone calling JanitorSweep.
func TestJanitorLoopSweepsOnItsInterval(t *testing.T) {
	withListWatchReflector(t)
	clock := newFakeClock()
	old := clock.Now().Add(-10 * time.Minute)
	client := newFakeDynamic(t, roomObject(t, dynamicCR("garbage", &rooms.Lease{Holder: "pod-dead", Generation: 1, RenewedAt: ptrTime(old)}, &old)))
	a := newTestStore(t, client, "pod-a", clock, func(o *Options) { o.JanitorInterval = 20 * time.Millisecond })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runSynced(t, ctx, client, a.Store)
	waitFor(t, 15*time.Second, func() bool {
		_, err := client.Resource(rooms.GroupVersionResource).Namespace(testNamespace).Get(ctx, "garbage", metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	}, "the janitor loop to delete the stale room")
}
