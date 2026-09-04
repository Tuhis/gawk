package roomcluster

// PR #302 review findings on the store, each pinned before it was fixed:
// a rotated attach Secret must reach the NEXT join without a CR bump
// (AttachSecret resolves per join); Mint's local re-check race must give
// its reservation back (Unreserve); an orphaned dynamic CR — home crashed
// while populated, emptySince unset — must not count against -max-rooms
// forever (the janitor stamps it, then deletes it a pass later); static
// adoption must rebuild status.attachments like dynamic adoption does; and
// no store error or log line may carry a raw room code (docs/44 D16) — the
// Secret is named after the code and Kubernetes error texts quote names.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Tuhis/gawk/gawk-server/internal/roomsrv"
	"github.com/Tuhis/gawk/gawk-server/rooms"
)

// Finding A: the portal rotates the Secret in place and never touches the
// CR, so a copy cached at adoption keeps admitting the old secret. The
// relay reads the Secret per join instead: the rotation is honoured by the
// next AttachSecret call with no CR change at all.
func TestAttachSecretReadsTheSecretPerJoinSoARotationNeedsNoCRBump(t *testing.T) {
	withListWatchReflector(t)
	ref := &rooms.SecretKeyRef{Name: "room-tuhisroom", Key: "attachSecret"}
	client := newFakeDynamic(t,
		roomObject(t, staticCR("tuhisroom", ref)),
		secretObject("room-tuhisroom", map[string]string{"attachSecret": "hunter2"}),
		roomObject(t, staticCR("open", nil)),
		roomObject(t, staticCR("broken", &rooms.SecretKeyRef{Name: "room-broken", Key: "attachSecret"})),
		secretObject("room-broken", map[string]string{"other": "x"}),
	)
	clock := newFakeClock()
	a := newTestStore(t, client, "pod-a", clock, func(o *Options) { o.RenewInterval = time.Hour })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runSynced(t, ctx, client, a.Store)
	if err := a.Adopt(ctx, "tuhisroom"); err != nil {
		t.Fatalf("Adopt: %v", err)
	}

	secret, found, err := a.AttachSecret("TuhisRoom")
	if err != nil || !found || secret != "hunter2" {
		t.Fatalf("AttachSecret before rotation = %q, %v, %v; want hunter2, true, nil", secret, found, err)
	}
	// Rotate the Secret only — the CR is untouched, as the portal does it.
	if _, err := client.Resource(secretGVR).Namespace(testNamespace).Update(ctx,
		secretObject("room-tuhisroom", map[string]string{"attachSecret": "rotated"}), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if cur := getRoom(t, client, "tuhisroom"); cur.Spec.DisplayName != "Tuhis' room" {
		t.Fatalf("the test bumped the CR itself: %+v", cur.Spec)
	}
	secret, found, err = a.AttachSecret("tuhisroom")
	if err != nil || !found || secret != "rotated" {
		t.Fatalf("AttachSecret after rotation = %q, %v, %v; want rotated, true, nil (the rotation reaches the next join)", secret, found, err)
	}

	// No reference: the registry's inline definition rules.
	if secret, found, err := a.AttachSecret("open"); err != nil || found || secret != "" {
		t.Errorf("AttachSecret(open) = %q, %v, %v; want \"\", false, nil", secret, found, err)
	}
	// Unknown to the cache (a file-defined static room in cluster mode): not
	// a CR room, so found=false rather than an error that fails every join.
	if secret, found, err := a.AttachSecret("nosuch"); err != nil || found || secret != "" {
		t.Errorf("AttachSecret(nosuch) = %q, %v, %v; want \"\", false, nil", secret, found, err)
	}
	// A reference that cannot be honoured fails closed: the join is refused
	// (503), never admitted against an empty secret.
	if _, _, err := a.AttachSecret("broken"); err == nil {
		t.Error("AttachSecret(broken) = nil error; want the missing key to fail closed")
	}
	if err := client.Resource(secretGVR).Namespace(testNamespace).Delete(ctx, "room-tuhisroom", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.AttachSecret("tuhisroom"); err == nil {
		t.Error("AttachSecret with the Secret gone = nil error; want fail closed")
	}
}

// Finding B: Reserve created the CR and started renewing; when Mint then
// finds the code taken locally, Unreserve gives both back — otherwise the
// slot counts against -max-rooms until the janitor's long window.
func TestUnreserveDeletesTheCRAndStopsRenewing(t *testing.T) {
	withListWatchReflector(t)
	client := newFakeDynamic(t)
	clock := newFakeClock()
	a := newTestStore(t, client, "pod-a", clock, func(o *Options) { o.RenewInterval = time.Hour })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runSynced(t, ctx, client, a.Store)

	cr := dynamicCR("k7xq2m", nil, nil)
	if err := a.Reserve(ctx, cr); err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	if _, held := a.Holding("k7xq2m"); !held {
		t.Fatal("Reserve did not start the lease")
	}
	a.Unreserve(ctx, "K7XQ2M")
	if _, held := a.Holding("k7xq2m"); held {
		t.Error("Unreserve left the renew loop running")
	}
	if _, err := client.Resource(rooms.GroupVersionResource).Namespace(testNamespace).Get(ctx, "k7xq2m", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("CR after Unreserve: err=%v, want NotFound", err)
	}
	// Idempotent: a second call (or one racing the informer's delete) is
	// quiet, and the code is reservable again.
	a.Unreserve(ctx, "k7xq2m")
	a.Unreserve(ctx, "not a code")
	waitFor(t, 15*time.Second, func() bool { return !a.Known("k7xq2m") }, "the cache to drop the CR")
	if err := a.Reserve(ctx, dynamicCR("k7xq2m", nil, nil)); err != nil {
		t.Errorf("Reserve after Unreserve: %v", err)
	}
}

// Finding C: a dynamic room whose home crashed while populated has
// emptySince unset, so the old janitor skipped it forever while Reserve
// counted it. Pass one stamps emptySince on a stale, unstamped CR (fenced
// on resourceVersion); a later pass deletes it once the stamp is older than
// the empty grace. A live home's unstamped room is left alone.
func TestJanitorStampsOrphansThenDeletesThemAPassLater(t *testing.T) {
	withListWatchReflector(t)
	clock := newFakeClock()
	now := clock.Now()
	old := now.Add(-10 * time.Minute)
	staleLease := &rooms.Lease{Holder: "pod-dead", Generation: 3, RenewedAt: ptrTime(old)}
	freshLease := &rooms.Lease{Holder: "pod-b", Generation: 1, RenewedAt: ptrTime(now.Add(-time.Second))}
	client := newFakeDynamic(t,
		roomObject(t, dynamicCR("orphan", staleLease, nil, rooms.Attachment{BroadcastID: "ABC23Z"})),
		roomObject(t, dynamicCR("busy", freshLease, nil)),
	)
	a := newTestStore(t, client, "pod-a", clock, func(o *Options) { o.MaxRooms = 2 })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runSynced(t, ctx, client, a.Store)
	if err := a.Reserve(ctx, dynamicCR("newone", nil, nil)); !errors.Is(err, roomsrv.ErrMaxRooms) {
		t.Fatalf("the orphan does not count against -max-rooms yet (err=%v); the premise is wrong", err)
	}

	// Pass one: stamped, not deleted.
	a.JanitorSweep(ctx)
	orphan := getRoom(t, client, "orphan")
	if orphan.Status.EmptySince == nil || !orphan.Status.EmptySince.Time.Equal(now) {
		t.Fatalf("orphan.emptySince after pass one = %v, want stamped at %v", orphan.Status.EmptySince, now)
	}
	if orphan.Status.Lease == nil || orphan.Status.Lease.Holder != "pod-dead" || orphan.Status.Lease.Generation != 3 {
		t.Errorf("the stamp rewrote the lease: %+v", orphan.Status.Lease)
	}
	if busy := getRoom(t, client, "busy"); busy.Status.EmptySince != nil {
		t.Errorf("a live home's populated room was stamped: %v", busy.Status.EmptySince)
	}
	// Inside the grace, a second pass still keeps it (an adopter may claim
	// it — Claim rewrites emptySince — and the first join clears it).
	clock.Advance(30 * time.Second)
	waitFor(t, 15*time.Second, func() bool {
		r, ok := a.Lookup("orphan")
		return ok && r.Status.EmptySince != nil
	}, "the stamp to reach the cache")
	a.JanitorSweep(ctx)
	if _, err := client.Resource(rooms.GroupVersionResource).Namespace(testNamespace).Get(ctx, "orphan", metav1.GetOptions{}); err != nil {
		t.Fatalf("orphan deleted inside the empty grace: %v", err)
	}
	if got := getRoom(t, client, "orphan"); !got.Status.EmptySince.Time.Equal(now) {
		t.Errorf("a second pass re-stamped emptySince to %v; the clock must not restart", got.Status.EmptySince)
	}

	// Past the grace: deleted, and the slot is free again.
	clock.Advance(time.Minute)
	a.JanitorSweep(ctx)
	if _, err := client.Resource(rooms.GroupVersionResource).Namespace(testNamespace).Get(ctx, "orphan", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("orphan survived the pass after the grace (err=%v)", err)
	}
	waitFor(t, 15*time.Second, func() bool { return !a.Known("orphan") }, "the cache to drop the orphan")
	if err := a.Reserve(ctx, dynamicCR("newone", nil, nil)); err != nil {
		t.Errorf("Reserve after the orphan was collected: %v", err)
	}
}

// The stamp is fenced: a CR that moved under the janitor (an adoption
// racing the sweep) is left to its new home.
func TestJanitorStampLosesToAConcurrentWrite(t *testing.T) {
	withListWatchReflector(t)
	clock := newFakeClock()
	old := clock.Now().Add(-10 * time.Minute)
	staleLease := &rooms.Lease{Holder: "pod-dead", Generation: 3, RenewedAt: ptrTime(old)}
	client := newFakeDynamic(t, roomObject(t, dynamicCR("orphan", staleLease, nil)))
	a, logs := loggingStore(t, client, "pod-a", nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runSynced(t, ctx, client, a.Store)
	// Bump the stored version behind the cache's back: the fenced update
	// conflicts and the pass writes nothing.
	client.PrependReactor("update", "rooms", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(rooms.GroupVersionResource.GroupResource(), "orphan", errors.New("moved"))
	})
	a.JanitorSweep(ctx)
	if got := getRoom(t, client, "orphan"); got.Status.EmptySince != nil {
		t.Errorf("a conflicting stamp landed: %v", got.Status.EmptySince)
	}
	if logs.contains("orphan") {
		t.Errorf("the janitor logged the raw code:\n%s", logs)
	}
}

// Finding D: dynamic adoption rebuilds the room from status.attachments;
// static adoption rebuilt it from the spec alone, so an away broadcaster
// lost its tile when a static room's home died. The adoption path passes
// the CR's attachments through; a plain spec refresh (informer) does not,
// since the registry ignores them on update anyway.
func TestAdoptStaticRebuildsAttachmentsFromTheCR(t *testing.T) {
	withListWatchReflector(t)
	cr := staticCR("tuhisroom", nil)
	cr.Status.Lease = &rooms.Lease{Holder: "pod-dead", Generation: 2, RenewedAt: ptrTime(time.Date(2026, 9, 3, 11, 0, 0, 0, time.UTC))}
	cr.Status.Attachments = []rooms.Attachment{{BroadcastID: "ABC23Z", Label: "main"}, {BroadcastID: "DEF456", Label: "cam"}}
	client := newFakeDynamic(t, roomObject(t, cr))
	clock := newFakeClock()
	a := newTestStore(t, client, "pod-a", clock, func(o *Options) { o.RenewInterval = time.Hour })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runSynced(t, ctx, client, a.Store)
	if err := a.Adopt(ctx, "tuhisroom"); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	ups, _, _ := a.reg.snapshot()
	if len(ups) != 1 {
		t.Fatalf("upserts = %+v, want one", ups)
	}
	if got := ups[0].Attachments; len(got) != 2 || got[0].BroadcastID != "ABC23Z" || got[1].BroadcastID != "DEF456" || got[1].Label != "cam" {
		t.Errorf("adopted static attachments = %+v, want the CR's two", got)
	}

	// A spec refresh on the held room carries no attachments.
	cur := getRoom(t, client, "tuhisroom")
	renamed := cur.DeepCopy()
	renamed.Spec.DisplayName = "Renamed"
	a.observe(roomObject(t, cur), roomObject(t, renamed))
	waitFor(t, 15*time.Second, func() bool {
		ups, _, _ := a.reg.snapshot()
		return len(ups) == 2
	}, "the spec refresh")
	ups, _, _ = a.reg.snapshot()
	if ups[1].DisplayName != "Renamed" || ups[1].Attachments != nil {
		t.Errorf("refresh upsert = %+v, want Renamed with nil attachments", ups[1])
	}
}

// Finding E (docs/44 D16): Kubernetes quotes object names in its error
// texts, and the attach Secret is conventionally named after the code, so
// an error or log line that embeds either verbatim leaks the code. Every
// store error and log line names a room by its HMAC'd key only.
func TestErrorsAndLogsNeverCarryTheRoomCode(t *testing.T) {
	withListWatchReflector(t)
	const code = "tuhisroom"
	ref := &rooms.SecretKeyRef{Name: "room-" + code, Key: "attachSecret"}
	client := newFakeDynamic(t,
		roomObject(t, staticCR(code, ref)),
		roomObject(t, staticCR("other", nil)),
		brokenRoomObject("brokenroom"),
	)
	a, logs := loggingStore(t, client, "pod-a", func(o *Options) {
		o.Obfuscate = func(string) string { return "hmac" }
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runSynced(t, ctx, client, a.Store)
	leaks := func(what string, err error) {
		t.Helper()
		if err != nil && strings.Contains(err.Error(), code) {
			t.Errorf("%s error carries the code: %v", what, err)
		}
	}

	// Adoption with the Secret missing: the API's `secrets "room-<code>"
	// not found` must not surface.
	err := a.Adopt(ctx, code)
	if !errors.Is(err, roomsrv.ErrUnavailable) {
		t.Fatalf("Adopt without the Secret = %v, want ErrUnavailable", err)
	}
	leaks("Adopt(secret missing)", err)
	_, _, err = a.AttachSecret(code)
	leaks("AttachSecret(secret missing)", err)

	// A forbidden API answer quotes the name too.
	client.PrependReactor("get", "rooms", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(rooms.GroupVersionResource.GroupResource(), action.(k8stesting.GetAction).GetName(), errors.New("rbac"))
	})
	leaks("Adopt(forbidden)", a.Adopt(ctx, code))
	leaks("Release(forbidden)", a.Release(ctx, code))
	_, err = a.Claim(ctx, code, false)
	leaks("Claim(forbidden)", err)
	a.RoomEnded(code, 0)
	a.RoomEmpty(code, true)

	if logs.contains(code) || logs.contains("brokenroom") {
		t.Errorf("a log line carries a raw room code:\n%s", logs)
	}
}
