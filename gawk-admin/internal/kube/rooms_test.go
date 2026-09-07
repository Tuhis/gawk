package kube_test

import (
	"context"
	"errors"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/Tuhis/gawk/gawk-admin/internal/kube"
	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-server/rooms"
)

// newFakeRoomClient builds a RoomClient over client-go's dynamic fake (Room
// CRs) and typed fake (Secrets) — both real client surfaces, as with the ban
// client's fake.
func newFakeRoomClient(t *testing.T, objs ...runtime.Object) (*kube.RoomClient, *kubefake.Clientset) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := rooms.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	dc := dynamicfake.NewSimpleDynamicClient(scheme, objs...)
	cs := kubefake.NewClientset()
	return kube.NewRoomClientFor(dc, cs, testNamespace), cs
}

func dynamicRoom(name, key string) *rooms.Room {
	return &rooms.Room{
		TypeMeta:   metav1.TypeMeta{APIVersion: rooms.SchemeGroupVersion.String(), Kind: rooms.Kind},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec:       rooms.RoomSpec{Kind: rooms.KindDynamic},
		Status:     rooms.RoomStatus{Key: key},
	}
}

// kubectlRoom is a static room an operator applied by hand: no annotation,
// and its attach secret lives wherever they put it.
func kubectlRoom(name string, ref *rooms.SecretKeyRef) *rooms.Room {
	return &rooms.Room{
		TypeMeta:   metav1.TypeMeta{APIVersion: rooms.SchemeGroupVersion.String(), Kind: rooms.Kind},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec:       rooms.RoomSpec{Kind: rooms.KindStatic, DisplayCode: name, AttachSecretRef: ref},
	}
}

func secretValue(t *testing.T, cs *kubefake.Clientset, name, key string) (string, bool) {
	t.Helper()
	s, err := cs.CoreV1().Secrets(testNamespace).Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		return "", false
	}
	if v, ok := s.StringData[key]; ok {
		return v, true
	}
	v, ok := s.Data[key]
	return string(v), ok
}

func TestRoomClientCreateStaticWritesSecretThenCRAndReturnsTheSecretOnce(t *testing.T) {
	c, cs := newFakeRoomClient(t)
	ctx := context.Background()

	secret, err := c.CreateStatic(ctx, kube.StaticRoom{
		Code: "TuhisRoom", DisplayName: "Tuhis' room", MaxBroadcasts: 4, WithAttachSecret: true,
	})
	if err != nil {
		t.Fatalf("CreateStatic: %v", err)
	}
	if len(secret) != kube.AttachSecretLength {
		t.Fatalf("secret = %q, want %d characters", secret, kube.AttachSecretLength)
	}

	obj, err := c.Get(ctx, "tuhisroom")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !obj.Managed {
		t.Fatalf("a portal-created room must carry %s", kube.AnnotationRoomManaged)
	}
	spec := obj.Room.Spec
	if spec.Kind != rooms.KindStatic || spec.DisplayCode != "TuhisRoom" || spec.DisplayName != "Tuhis' room" || spec.MaxBroadcasts != 4 {
		t.Fatalf("spec = %+v", spec)
	}
	if spec.AttachSecretRef == nil || spec.AttachSecretRef.Name != "room-tuhisroom" || spec.AttachSecretRef.Key != kube.RoomSecretKey {
		t.Fatalf("attachSecretRef = %+v, want room-tuhisroom/attachSecret", spec.AttachSecretRef)
	}
	// The Secret holds the same value the caller was shown, and is stamped as
	// the portal's so Delete may remove it.
	stored, ok := secretValue(t, cs, "room-tuhisroom", kube.RoomSecretKey)
	if !ok || stored != secret {
		t.Fatalf("stored secret = %q ok=%v, want the returned value", stored, ok)
	}
	s, _ := cs.CoreV1().Secrets(testNamespace).Get(ctx, "room-tuhisroom", metav1.GetOptions{})
	if s.Annotations[kube.AnnotationRoomManaged] != "true" {
		t.Fatalf("the portal's Secret is not annotated as managed: %+v", s.Annotations)
	}
	// The API never reads a secret back: the listed object exposes only that
	// there IS one.
	list, err := c.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %d (err=%v)", len(list), err)
	}
}

func TestRoomClientCreateStaticWithoutSecretWritesNoSecret(t *testing.T) {
	c, cs := newFakeRoomClient(t)
	secret, err := c.CreateStatic(context.Background(), kube.StaticRoom{Code: "open-room"})
	if err != nil {
		t.Fatalf("CreateStatic: %v", err)
	}
	if secret != "" {
		t.Fatalf("no secret was asked for, got %q", secret)
	}
	obj, _ := c.Get(context.Background(), "open-room")
	if obj.Room.Spec.AttachSecretRef != nil {
		t.Fatalf("attachSecretRef = %+v, want none", obj.Room.Spec.AttachSecretRef)
	}
	if list, _ := cs.CoreV1().Secrets(testNamespace).List(context.Background(), metav1.ListOptions{}); len(list.Items) != 0 {
		t.Fatalf("a secret-less room created %d Secrets", len(list.Items))
	}
}

// The CR create is the reservation. A duplicate is refused with the sentinel,
// and — the error path releasing what it acquired — the Secret written ahead
// of the refused CR is removed again rather than left as an orphan carrying a
// value nothing references.
func TestRoomClientCreateStaticDuplicateIsRefusedAndReleasesItsSecret(t *testing.T) {
	c, cs := newFakeRoomClient(t, kubectlRoom("tuhisroom", nil))
	ctx := context.Background()

	_, err := c.CreateStatic(ctx, kube.StaticRoom{Code: "TUHISROOM", WithAttachSecret: true})
	if !errors.Is(err, kube.ErrRoomExists) {
		t.Fatalf("err = %v, want ErrRoomExists", err)
	}
	if _, ok := secretValue(t, cs, "room-tuhisroom", kube.RoomSecretKey); ok {
		t.Fatal("the Secret written for a refused create was left behind")
	}
	// The operator's CR is untouched.
	obj, err := c.Get(ctx, "tuhisroom")
	if err != nil || obj.Managed {
		t.Fatalf("the existing room was altered: %+v (err=%v)", obj, err)
	}
}

func TestRoomClientCreateStaticRejectsABadSlug(t *testing.T) {
	c, _ := newFakeRoomClient(t)
	for _, bad := range []string{"ab", "has space", "-lead", "trail-", "ünïcode"} {
		if _, err := c.CreateStatic(context.Background(), kube.StaticRoom{Code: bad}); !errors.Is(err, rooms.ErrInvalidCode) {
			t.Errorf("code %q: err = %v, want ErrInvalidCode", bad, err)
		}
	}
}

func TestRoomClientRotateSecretReplacesTheReferencedValue(t *testing.T) {
	c, cs := newFakeRoomClient(t)
	ctx := context.Background()
	first, err := c.CreateStatic(ctx, kube.StaticRoom{Code: "tuhisroom", WithAttachSecret: true})
	if err != nil {
		t.Fatalf("CreateStatic: %v", err)
	}
	second, err := c.RotateSecret(ctx, "tuhisroom")
	if err != nil {
		t.Fatalf("RotateSecret: %v", err)
	}
	if second == first || len(second) != kube.AttachSecretLength {
		t.Fatalf("rotated secret %q must be a fresh value", second)
	}
	stored, _ := secretValue(t, cs, "room-tuhisroom", kube.RoomSecretKey)
	if stored != second {
		t.Fatalf("stored = %q, want the rotated value", stored)
	}
	// Still exactly one Secret: rotation updates in place.
	if list, _ := cs.CoreV1().Secrets(testNamespace).List(ctx, metav1.ListOptions{}); len(list.Items) != 1 {
		t.Fatalf("rotation produced %d Secrets, want 1", len(list.Items))
	}
}

// A `kubectl apply`'d room may reference a Secret with any name and key, and
// may keep other keys in it. Rotation writes the key the relay READS — not
// `room-<code>` — and leaves the rest of the Secret alone.
func TestRoomClientRotateSecretHonoursAnOperatorsOwnReference(t *testing.T) {
	c, cs := newFakeRoomClient(t, kubectlRoom("ourroom", &rooms.SecretKeyRef{Name: "my-secrets", Key: "gate"}))
	ctx := context.Background()
	if _, err := cs.CoreV1().Secrets(testNamespace).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-secrets"},
		Data:       map[string][]byte{"gate": []byte("old"), "other": []byte("keep")},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed secret: %v", err)
	}

	rotated, err := c.RotateSecret(ctx, "ourroom")
	if err != nil {
		t.Fatalf("RotateSecret: %v", err)
	}
	s, _ := cs.CoreV1().Secrets(testNamespace).Get(ctx, "my-secrets", metav1.GetOptions{})
	if string(s.Data["gate"]) != rotated {
		t.Fatalf("gate = %q, want the rotated value", s.Data["gate"])
	}
	if string(s.Data["other"]) != "keep" {
		t.Fatalf("rotation clobbered an unrelated key: %+v", s.Data)
	}
	if _, ok := secretValue(t, cs, "room-ourroom", kube.RoomSecretKey); ok {
		t.Fatal("rotation wrote the portal-named Secret instead of the referenced one")
	}
}

// A static room created without a gate can grow one: rotation mints the
// portal-named Secret and patches the reference onto the CR.
func TestRoomClientRotateSecretAddsAGateToASecretlessRoom(t *testing.T) {
	c, cs := newFakeRoomClient(t)
	ctx := context.Background()
	if _, err := c.CreateStatic(ctx, kube.StaticRoom{Code: "open"}); err != nil {
		t.Fatalf("CreateStatic: %v", err)
	}
	secret, err := c.RotateSecret(ctx, "open")
	if err != nil {
		t.Fatalf("RotateSecret: %v", err)
	}
	obj, _ := c.Get(ctx, "open")
	ref := obj.Room.Spec.AttachSecretRef
	if ref == nil || ref.Name != "room-open" || ref.Key != kube.RoomSecretKey {
		t.Fatalf("attachSecretRef after rotate = %+v", ref)
	}
	if stored, _ := secretValue(t, cs, "room-open", kube.RoomSecretKey); stored != secret {
		t.Fatalf("stored = %q, want %q", stored, secret)
	}
	// The patch touched only the reference: the display code survived.
	if obj.Room.Spec.DisplayCode != "open" || !obj.Managed {
		t.Fatalf("the patch clobbered the spec or the annotation: %+v", obj)
	}
}

func TestRoomClientRotateSecretRefusesADynamicRoomAndAMissingOne(t *testing.T) {
	c, _ := newFakeRoomClient(t, dynamicRoom("r7k3mx", "9c1d2e3f4a5b"))
	if _, err := c.RotateSecret(context.Background(), "r7k3mx"); !errors.Is(err, kube.ErrRoomNotStatic) {
		t.Fatalf("dynamic: err = %v, want ErrRoomNotStatic", err)
	}
	if _, err := c.RotateSecret(context.Background(), "nope"); !errors.Is(err, kube.ErrRoomNotFound) {
		t.Fatalf("missing: err = %v, want ErrRoomNotFound", err)
	}
}

func TestRoomClientDeleteRemovesAManagedRoomAndItsSecret(t *testing.T) {
	c, cs := newFakeRoomClient(t)
	ctx := context.Background()
	if _, err := c.CreateStatic(ctx, kube.StaticRoom{Code: "tuhisroom", WithAttachSecret: true}); err != nil {
		t.Fatalf("CreateStatic: %v", err)
	}
	obj, err := c.DeleteExisting(ctx, "tuhisroom")
	if err != nil {
		t.Fatalf("DeleteExisting: %v", err)
	}
	if obj.Room.Spec.Kind != rooms.KindStatic {
		t.Fatalf("DeleteExisting returned %+v, want the deleted static room", obj)
	}
	if _, err := c.Get(ctx, "tuhisroom"); !errors.Is(err, kube.ErrRoomNotFound) {
		t.Fatalf("room still there after delete: %v", err)
	}
	if _, ok := secretValue(t, cs, "room-tuhisroom", kube.RoomSecretKey); ok {
		t.Fatal("the portal's Secret outlived its room")
	}
	// Idempotent: already gone is success for Delete, and a named refusal
	// for DeleteExisting.
	if err := c.Delete(ctx, "tuhisroom"); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	if _, err := c.DeleteExisting(ctx, "tuhisroom"); !errors.Is(err, kube.ErrRoomNotFound) {
		t.Fatalf("second DeleteExisting: err = %v, want ErrRoomNotFound", err)
	}
}

// An explicit portal delete of a `kubectl apply`'d room removes THAT CR — it
// is the operator asking, by name — but never the operator's own Secret, which
// the portal did not create and may be shared with something else.
func TestRoomClientDeleteLeavesAnOperatorsSecretAlone(t *testing.T) {
	c, cs := newFakeRoomClient(t, kubectlRoom("ourroom", &rooms.SecretKeyRef{Name: "my-secrets", Key: "gate"}))
	ctx := context.Background()
	if _, err := cs.CoreV1().Secrets(testNamespace).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-secrets"},
		Data:       map[string][]byte{"gate": []byte("old")},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("seed secret: %v", err)
	}
	if err := c.Delete(ctx, "ourroom"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := c.Get(ctx, "ourroom"); !errors.Is(err, kube.ErrRoomNotFound) {
		t.Fatalf("the operator's room was not deleted on an explicit request: %v", err)
	}
	if _, ok := secretValue(t, cs, "my-secrets", "gate"); !ok {
		t.Fatal("the operator's un-annotated Secret was deleted with the room")
	}
}

func TestRoomClientDeleteOfADynamicRoomTouchesNoSecret(t *testing.T) {
	c, cs := newFakeRoomClient(t, dynamicRoom("r7k3mx", "9c1d2e3f4a5b"))
	ctx := context.Background()
	obj, err := c.DeleteExisting(ctx, "r7k3mx")
	if err != nil || obj.Room.Spec.Kind != rooms.KindDynamic {
		t.Fatalf("DeleteExisting = %+v, %v", obj, err)
	}
	if list, _ := cs.CoreV1().Secrets(testNamespace).List(ctx, metav1.ListOptions{}); len(list.Items) != 0 {
		t.Fatalf("ending a dynamic room touched Secrets: %d", len(list.Items))
	}
}

func TestRoomClientListReportsBothKindsAndTheirProvenance(t *testing.T) {
	c, _ := newFakeRoomClient(t, dynamicRoom("r7k3mx", "9c1d2e3f4a5b"), kubectlRoom("ourroom", nil))
	ctx := context.Background()
	if _, err := c.CreateStatic(ctx, kube.StaticRoom{Code: "Mine", WithAttachSecret: true}); err != nil {
		t.Fatalf("CreateStatic: %v", err)
	}
	list, err := c.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	byName := map[string]kube.RoomObject{}
	for _, o := range list {
		byName[o.Name] = o
	}
	if len(byName) != 3 {
		t.Fatalf("listed %d rooms, want 3: %+v", len(byName), list)
	}
	if byName["r7k3mx"].Managed || byName["r7k3mx"].Room.Status.Key != "9c1d2e3f4a5b" {
		t.Fatalf("dynamic room = %+v", byName["r7k3mx"])
	}
	if byName["ourroom"].Managed {
		t.Fatalf("a kubectl room reported as portal-managed")
	}
	mine := byName["mine"]
	if !mine.Managed || rooms.DisplayCode(&mine.Room) != "Mine" {
		t.Fatalf("portal room = %+v", byName["mine"])
	}
}

// --- the reconciler's room sweep ------------------------------------------

// A dynamic room that vanishes between two sweeps was ended by the RELAY
// (grace expiry or a creator's EndRoom, docs/44 §4.4): the sweep records
// room.ended as "system", carrying the key it remembered from the last pass.
func TestRoomSweepRecordsARelayEndedDynamicRoomOnce(t *testing.T) {
	recs := newFakeRecords()
	bans, _ := newFakeCRClient(t)
	roomsClient, _ := newFakeRoomClient(t, dynamicRoom("r7k3mx", "9c1d2e3f4a5b"), kubectlRoom("ourroom", nil))
	r, err := kube.NewReconciler(kube.ReconcilerOptions{Records: recs, Bans: bans, Rooms: roomsClient})
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	ctx := context.Background()

	// The first pass seeds; nothing has ended yet.
	if err := r.SweepRoomsOnce(ctx); err != nil {
		t.Fatalf("seed sweep: %v", err)
	}
	if got := recs.eventTypes(); len(got) != 0 {
		t.Fatalf("the seeding pass recorded %v", got)
	}

	// The relay ends it (CR deleted by someone other than the portal).
	if err := roomsClient.Delete(ctx, "r7k3mx"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := r.SweepRoomsOnce(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := recs.eventTypes(); len(got) != 1 || got[0] != store.EventRoomEnded {
		t.Fatalf("events = %v, want one room.ended", got)
	}
	ev := recs.events[0]
	if ev.Actor != "system" || ev.RoomKey() != "9c1d2e3f4a5b" || ev.PayloadString(store.PayloadRoom) != "r7k3mx" {
		t.Fatalf("event = %+v payload=%s", ev, ev.Payload)
	}
	if s := ev.PayloadString(store.PayloadSummary); s != "a dynamic room ended" {
		t.Fatalf("summary = %q", s)
	}

	// A further pass records nothing more; the static room's continued
	// existence — or its deletion — is not the sweep's business.
	if err := roomsClient.Delete(ctx, "ourroom"); err != nil {
		t.Fatalf("Delete static: %v", err)
	}
	if err := r.SweepRoomsOnce(ctx); err != nil {
		t.Fatalf("third sweep: %v", err)
	}
	if got := recs.eventTypes(); len(got) != 1 {
		t.Fatalf("events after the third sweep = %v, want still one", got)
	}
}

// A room the PORTAL ended already has its room.ended, attributed to the
// operator; the sweep must not add a second, "system" one — that is the
// double-page RoomEndedSince exists to prevent.
func TestRoomSweepDoesNotDuplicateAPortalRecordedEnd(t *testing.T) {
	recs := newFakeRecords()
	bans, _ := newFakeCRClient(t)
	roomsClient, _ := newFakeRoomClient(t, dynamicRoom("r7k3mx", "9c1d2e3f4a5b"))
	r, err := kube.NewReconciler(kube.ReconcilerOptions{Records: recs, Bans: bans, Rooms: roomsClient})
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	ctx := context.Background()
	if err := r.SweepRoomsOnce(ctx); err != nil {
		t.Fatalf("seed sweep: %v", err)
	}

	// The portal's DELETE: event recorded inline, then the CR goes.
	if _, err := recs.AppendEvent(ctx, store.Event{
		Type: store.EventRoomEnded, OccurredAt: time.Now(), Actor: "op@example.com",
		Payload: []byte(`{"room":"r7k3mx","kind":"dynamic"}`),
	}); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}
	if err := roomsClient.Delete(ctx, "r7k3mx"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := r.SweepRoomsOnce(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := recs.eventTypes(); len(got) != 1 {
		t.Fatalf("events = %v, want only the operator's room.ended", got)
	}
}

// With Postgres down the sweep cannot tell a portal end from a relay end, so
// it records nothing — and it does not keep the vanished room in its baseline
// to retry, because a retry after the store returns would be a stale event
// with no way to know whether the portal's own record covered it.
func TestRoomSweepRecordsNothingWhilePostgresIsDown(t *testing.T) {
	recs := newFakeRecords()
	bans, _ := newFakeCRClient(t)
	roomsClient, _ := newFakeRoomClient(t, dynamicRoom("r7k3mx", "9c1d2e3f4a5b"))
	r, err := kube.NewReconciler(kube.ReconcilerOptions{Records: recs, Bans: bans, Rooms: roomsClient})
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	ctx := context.Background()
	if err := r.SweepRoomsOnce(ctx); err != nil {
		t.Fatalf("seed sweep: %v", err)
	}
	if err := roomsClient.Delete(ctx, "r7k3mx"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	recs.setDown(true)
	if err := r.SweepRoomsOnce(ctx); err != nil {
		t.Fatalf("sweep with Postgres down: %v", err)
	}
	recs.setDown(false)
	if err := r.SweepRoomsOnce(ctx); err != nil {
		t.Fatalf("sweep after recovery: %v", err)
	}
	if got := recs.eventTypes(); len(got) != 0 {
		t.Fatalf("events = %v, want none", got)
	}
}

// Rooms off (no lister) means the sweep is inert: the ban reconcile keeps its
// behaviour byte-for-byte and no Room CR is ever listed.
func TestRoomSweepIsANoOpWithoutARoomLister(t *testing.T) {
	recs := newFakeRecords()
	bans, _ := newFakeCRClient(t)
	r := newReconciler(t, recs, bans, time.Now, nil)
	if err := r.SweepRoomsOnce(context.Background()); err != nil {
		t.Fatalf("SweepRoomsOnce: %v", err)
	}
	if got := recs.eventTypes(); len(got) != 0 {
		t.Fatalf("events = %v", got)
	}
}
