package api_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Tuhis/gawk/gawk-admin/internal/api"
	"github.com/Tuhis/gawk/gawk-admin/internal/kube"
	"github.com/Tuhis/gawk/gawk-admin/internal/store"
	"github.com/Tuhis/gawk/gawk-server/rooms"
)

// fakeRooms is an in-memory api.Rooms with the kube client's rules: the CR
// name is the reservation, rotation is static-only, delete reports the kind.
// The rules themselves are tested against client-go's fakes in internal/kube;
// this double exists so the ROUTES are testable without a cluster.
type fakeRooms struct {
	mu    sync.Mutex
	rooms map[string]kube.RoomObject
	// secrets counts mints so a test can assert "one secret per create".
	secrets int
	err     error
}

func newFakeRooms(objs ...kube.RoomObject) *fakeRooms {
	f := &fakeRooms{rooms: map[string]kube.RoomObject{}}
	for _, o := range objs {
		f.rooms[o.Name] = o
	}
	return f
}

func (f *fakeRooms) List(context.Context) ([]kube.RoomObject, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := make([]kube.RoomObject, 0, len(f.rooms))
	for _, o := range f.rooms {
		out = append(out, o)
	}
	return out, nil
}

func (f *fakeRooms) CreateStatic(_ context.Context, req kube.StaticRoom) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return "", f.err
	}
	code, err := rooms.NormalizeCode(req.Code)
	if err != nil {
		return "", err
	}
	if _, exists := f.rooms[code]; exists {
		return "", kube.ErrRoomExists
	}
	obj := kube.RoomObject{Name: code, Managed: true}
	obj.Room.Name = code
	obj.Room.Spec = rooms.RoomSpec{Kind: rooms.KindStatic, DisplayCode: req.Code,
		DisplayName: req.DisplayName, MaxBroadcasts: req.MaxBroadcasts}
	secret := ""
	if req.WithAttachSecret {
		f.secrets++
		secret = "SECRET-" + code
		obj.Room.Spec.AttachSecretRef = &rooms.SecretKeyRef{Name: kube.RoomSecretName(code), Key: kube.RoomSecretKey}
	}
	f.rooms[code] = obj
	return secret, nil
}

func (f *fakeRooms) RotateSecret(_ context.Context, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	obj, ok := f.rooms[name]
	if !ok {
		return "", kube.ErrRoomNotFound
	}
	if obj.Room.Spec.Kind != rooms.KindStatic {
		return "", kube.ErrRoomNotStatic
	}
	f.secrets++
	obj.Room.Spec.AttachSecretRef = &rooms.SecretKeyRef{Name: kube.RoomSecretName(name), Key: kube.RoomSecretKey}
	f.rooms[name] = obj
	return "ROTATED-" + name, nil
}

func (f *fakeRooms) DeleteExisting(_ context.Context, name string) (kube.RoomObject, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	obj, ok := f.rooms[name]
	if !ok {
		return kube.RoomObject{}, kube.ErrRoomNotFound
	}
	delete(f.rooms, name)
	return obj, nil
}

func dynamicRoomObject(name, key, holder string, attachments int) kube.RoomObject {
	obj := kube.RoomObject{Name: name}
	obj.Room.Name = name
	obj.Room.Spec = rooms.RoomSpec{Kind: rooms.KindDynamic}
	created := metav1.NewTime(time.Date(2026, 9, 3, 18, 0, 0, 0, time.UTC))
	obj.Room.Status = rooms.RoomStatus{Key: key, CreatedAt: &created, Lease: &rooms.Lease{Holder: holder, Generation: 3}}
	for i := 0; i < attachments; i++ {
		obj.Room.Status.Attachments = append(obj.Room.Status.Attachments, rooms.Attachment{BroadcastID: "ABC234"})
	}
	return obj
}

// memoryRecorder captures events without Postgres, so the room routes — which
// have no row of their own — are testable on a machine with no database.
type memoryRecorder struct {
	mu     sync.Mutex
	events []store.Event
}

func (m *memoryRecorder) Record(_ context.Context, ev store.Event) (store.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ev.ID = int64(len(m.events) + 1)
	m.events = append(m.events, ev)
	return ev, nil
}

func (m *memoryRecorder) all() []store.Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]store.Event(nil), m.events...)
}

func withRooms(f *fakeRooms, rec *memoryRecorder) harnessOption {
	return func(o *api.Options, _ *harness) {
		o.Rooms = f
		o.Recorder = rec
	}
}

// The wire shape, declared independently so a rename fails a test.
type wireRoom struct {
	Name            string `json:"name"`
	Kind            string `json:"kind"`
	Code            string `json:"code"`
	DisplayName     string `json:"displayName"`
	MaxBroadcasts   int    `json:"maxBroadcasts"`
	Attachments     int    `json:"attachments"`
	HomeHolder      string `json:"homeHolder"`
	Key             string `json:"key"`
	CreatedAt       string `json:"createdAt"`
	EmptySince      string `json:"emptySince"`
	HasAttachSecret bool   `json:"hasAttachSecret"`
	Managed         bool   `json:"managed"`
}

type wireRoomWithSecret struct {
	Room         wireRoom `json:"room"`
	AttachSecret string   `json:"attachSecret"`
}

// Rooms OFF is the default: no route exists and /me says so, so the SPA
// renders no navigation to a surface the ServiceAccount has no RBAC for.
func TestRoomsRoutesAbsentWhenTheFeatureIsOff(t *testing.T) {
	h := newHarnessWithoutPostgres(t)
	if code := h.errorCode(http.MethodGet, "/api/v1/rooms", nil, http.StatusNotFound); code != api.CodeNotFound {
		t.Fatalf("rooms off: GET /rooms code = %q", code)
	}
	if code := h.errorCode(http.MethodPost, "/api/v1/rooms", map[string]any{"code": "x"}, http.StatusNotFound); code != api.CodeNotFound {
		t.Fatalf("rooms off: POST /rooms code = %q", code)
	}
	var me struct {
		Features struct {
			Rooms bool `json:"rooms"`
		} `json:"features"`
	}
	h.decode(http.MethodGet, "/api/v1/me", nil, http.StatusOK, &me)
	if me.Features.Rooms {
		t.Fatal("/me reports rooms with the feature off")
	}
}

func TestRoomsListRendersBothKinds(t *testing.T) {
	f := newFakeRooms(dynamicRoomObject("r7k3mx", "9c1d2e3f4a5b", "gawk-server-0", 2))
	rec := &memoryRecorder{}
	h := newHarnessWithoutPostgres(t, withRooms(f, rec))
	if _, err := f.CreateStatic(t.Context(), kube.StaticRoom{Code: "TuhisRoom", DisplayName: "Tuhis' room", WithAttachSecret: true}); err != nil {
		t.Fatal(err)
	}

	var me struct {
		Features struct {
			Rooms bool `json:"rooms"`
		} `json:"features"`
	}
	h.decode(http.MethodGet, "/api/v1/me", nil, http.StatusOK, &me)
	if !me.Features.Rooms {
		t.Fatal("/me does not report rooms with the feature on")
	}

	var body struct {
		Rooms []wireRoom `json:"rooms"`
	}
	h.decode(http.MethodGet, "/api/v1/rooms", nil, http.StatusOK, &body)
	if len(body.Rooms) != 2 {
		t.Fatalf("rooms = %+v", body.Rooms)
	}
	// Static first, then dynamic.
	st, dy := body.Rooms[0], body.Rooms[1]
	if st.Kind != "static" || st.Name != "tuhisroom" || st.Code != "TuhisRoom" || st.DisplayName != "Tuhis' room" ||
		!st.HasAttachSecret || !st.Managed || st.Key != "" {
		t.Fatalf("static row = %+v", st)
	}
	if dy.Kind != "dynamic" || dy.Code != "R7K3MX" || dy.Key != "9c1d2e3f4a5b" || dy.HomeHolder != "gawk-server-0" ||
		dy.Attachments != 2 || dy.HasAttachSecret || dy.Managed || dy.CreatedAt != "2026-09-03T18:00:00Z" {
		t.Fatalf("dynamic row = %+v", dy)
	}
	// The list never carries a secret, under any name.
	_, raw := h.raw(http.MethodGet, "/api/v1/rooms", nil)
	if strings.Contains(raw, `"attachSecret"`) || strings.Contains(raw, "SECRET-") {
		t.Fatalf("the list leaked a secret: %s", raw)
	}
}

func TestRoomsCreateReturnsTheSecretOnceAndRecordsTheEvent(t *testing.T) {
	f := newFakeRooms()
	rec := &memoryRecorder{}
	h := newHarnessWithoutPostgres(t, withRooms(f, rec))

	var out wireRoomWithSecret
	h.decode(http.MethodPost, "/api/v1/rooms", map[string]any{
		"code": "TuhisRoom", "displayName": "Tuhis' room", "maxBroadcasts": 4, "withAttachSecret": true,
	}, http.StatusCreated, &out)
	if out.AttachSecret != "SECRET-tuhisroom" {
		t.Fatalf("attachSecret = %q", out.AttachSecret)
	}
	r := out.Room
	if r.Name != "tuhisroom" || r.Code != "TuhisRoom" || r.Kind != "static" || r.MaxBroadcasts != 4 || !r.HasAttachSecret || !r.Managed {
		t.Fatalf("room = %+v", r)
	}
	if r.CreatedAt == "" {
		t.Fatal("createdAt missing on the created room")
	}
	if h.kicks.count() == 0 {
		t.Fatal("the reconciler was not kicked")
	}

	// One room.created, kind-only summary, the raw code under the portal-only
	// key, no key yet (nobody has homed the room), and never the secret.
	events := rec.all()
	if len(events) != 1 || events[0].Type != store.EventRoomCreated || events[0].Actor != "op@example.com" {
		t.Fatalf("events = %+v", events)
	}
	ev := events[0]
	if s := ev.PayloadString(store.PayloadSummary); s != "a static room was created by op@example.com" {
		t.Fatalf("summary = %q", s)
	}
	if ev.PayloadString(store.PayloadRoom) != "tuhisroom" || ev.RoomKey() != "" {
		t.Fatalf("payload = %s", ev.Payload)
	}
	if strings.Contains(string(ev.Payload), "SECRET-") {
		t.Fatalf("the attach secret reached an event payload: %s", ev.Payload)
	}
	// Nor a log line, at any level.
	if strings.Contains(h.logText(), "SECRET-") {
		t.Fatal("the attach secret was logged")
	}

	// The secret is one-time: a second look at the room has none.
	var list struct {
		Rooms []wireRoom `json:"rooms"`
	}
	_, raw := h.raw(http.MethodGet, "/api/v1/rooms", nil)
	if strings.Contains(raw, "SECRET-") {
		t.Fatalf("the list carries the secret: %s", raw)
	}
	h.decode(http.MethodGet, "/api/v1/rooms", nil, http.StatusOK, &list)
	if len(list.Rooms) != 1 || !list.Rooms[0].HasAttachSecret {
		t.Fatalf("list = %+v", list.Rooms)
	}

	// A room without a secret answers with no attachSecret at all.
	_, raw = h.raw(http.MethodPost, "/api/v1/rooms", map[string]any{"code": "open-room"})
	if strings.Contains(raw, "attachSecret") {
		t.Fatalf("a secret-less create carried an attachSecret key: %s", raw)
	}
}

// The join box takes six characters and resolves rooms first (docs/44 D3,
// §4.2): a static slug of that shape would be typeable, which D2's "link
// only" rules out. Refused before anything reaches the cluster.
func TestRoomsCreateRefusesADynamicShapedSlugAndBadInput(t *testing.T) {
	f := newFakeRooms()
	h := newHarnessWithoutPostgres(t, withRooms(f, &memoryRecorder{}))

	for _, tc := range []struct {
		name string
		body map[string]any
	}{
		{"dynamic shape", map[string]any{"code": "ABC234"}},
		{"dynamic shape lower", map[string]any{"code": "abc234"}},
		{"too short", map[string]any{"code": "ab"}},
		{"bad character", map[string]any{"code": "has space"}},
		{"leading dash", map[string]any{"code": "-room"}},
		{"maxBroadcasts out of range", map[string]any{"code": "fine-room", "maxBroadcasts": 1000}},
		{"unknown field", map[string]any{"code": "fine-room", "attachSecret": "chosen"}},
	} {
		if code := h.errorCode(http.MethodPost, "/api/v1/rooms", tc.body, http.StatusBadRequest); code != api.CodeBadRequest {
			t.Errorf("%s: code = %q", tc.name, code)
		}
	}
	// Six characters that are NOT all in the broadcast alphabet are fine: the
	// join box could never resolve them.
	h.decode(http.MethodPost, "/api/v1/rooms", map[string]any{"code": "room01"}, http.StatusCreated, nil)
	if list, _ := f.List(t.Context()); len(list) != 1 {
		t.Fatalf("rooms after the refusals = %d, want only room01", len(list))
	}
}

func TestRoomsCreateConflictsOnAnExistingCode(t *testing.T) {
	f := newFakeRooms()
	h := newHarnessWithoutPostgres(t, withRooms(f, &memoryRecorder{}))
	h.decode(http.MethodPost, "/api/v1/rooms", map[string]any{"code": "TuhisRoom"}, http.StatusCreated, nil)
	if code := h.errorCode(http.MethodPost, "/api/v1/rooms", map[string]any{"code": "tuhisroom"}, http.StatusConflict); code != api.CodeRoomExists {
		t.Fatalf("duplicate code = %q", code)
	}
}

func TestRoomsRotateSecretIsStaticOnly(t *testing.T) {
	f := newFakeRooms(dynamicRoomObject("r7k3mx", "9c1d2e3f4a5b", "gawk-server-0", 1))
	rec := &memoryRecorder{}
	h := newHarnessWithoutPostgres(t, withRooms(f, rec))
	h.decode(http.MethodPost, "/api/v1/rooms", map[string]any{"code": "TuhisRoom", "withAttachSecret": true}, http.StatusCreated, nil)

	var out wireRoomWithSecret
	h.decode(http.MethodPost, "/api/v1/rooms/TuhisRoom/rotate-secret", nil, http.StatusOK, &out)
	if out.AttachSecret != "ROTATED-tuhisroom" || !out.Room.HasAttachSecret {
		t.Fatalf("rotate = %+v", out)
	}
	events := rec.all()
	if len(events) != 2 || events[1].Type != store.EventRoomSecretRotated {
		t.Fatalf("events = %+v", events)
	}
	if s := events[1].PayloadString(store.PayloadSummary); !strings.Contains(s, "attach secret of a static room was rotated") {
		t.Fatalf("summary = %q", s)
	}
	if strings.Contains(string(events[1].Payload), "ROTATED-") || strings.Contains(h.logText(), "ROTATED-") {
		t.Fatal("the rotated secret reached an event or a log line")
	}

	if code := h.errorCode(http.MethodPost, "/api/v1/rooms/r7k3mx/rotate-secret", nil, http.StatusConflict); code != api.CodeRoomNotStatic {
		t.Fatalf("rotate on a dynamic room code = %q", code)
	}
	if code := h.errorCode(http.MethodPost, "/api/v1/rooms/nope/rotate-secret", nil, http.StatusNotFound); code != api.CodeNotFound {
		t.Fatalf("rotate on a missing room code = %q", code)
	}
}

// DELETE ends either kind; /end is the dynamic-only alias. Both record
// room.ended BEFORE the CR goes, with the operator as actor and the HMAC'd key
// when the room had one — the sweep's dedup depends on that ordering.
func TestRoomsDeleteAndEnd(t *testing.T) {
	f := newFakeRooms(
		dynamicRoomObject("r7k3mx", "9c1d2e3f4a5b", "gawk-server-0", 1),
		dynamicRoomObject("bbb234", "1a2b3c4d5e6f", "gawk-server-1", 0),
	)
	rec := &memoryRecorder{}
	h := newHarnessWithoutPostgres(t, withRooms(f, rec))
	h.decode(http.MethodPost, "/api/v1/rooms", map[string]any{"code": "TuhisRoom", "withAttachSecret": true}, http.StatusCreated, nil)

	// /end refuses a static room and names why.
	if code := h.errorCode(http.MethodPost, "/api/v1/rooms/tuhisroom/end", nil, http.StatusConflict); code != api.CodeRoomNotDynamic {
		t.Fatalf("end on a static room code = %q", code)
	}
	if list, _ := f.List(t.Context()); len(list) != 3 {
		t.Fatalf("the refused end deleted something: %d rooms", len(list))
	}

	// /end on a dynamic room.
	status, body := h.raw(http.MethodPost, "/api/v1/rooms/R7K3MX/end", nil)
	if status != http.StatusNoContent || strings.TrimSpace(body) != "" {
		t.Fatalf("end = %d %q", status, body)
	}
	// DELETE on a dynamic room and on the static one.
	h.decode(http.MethodDelete, "/api/v1/rooms/bbb234", nil, http.StatusNoContent, nil)
	h.decode(http.MethodDelete, "/api/v1/rooms/tuhisroom", nil, http.StatusNoContent, nil)
	if list, _ := f.List(t.Context()); len(list) != 0 {
		t.Fatalf("rooms left = %d", len(list))
	}

	events := rec.all()
	if len(events) != 4 {
		t.Fatalf("events = %+v", events)
	}
	ended := events[1]
	if ended.Type != store.EventRoomEnded || ended.Actor != "op@example.com" || ended.RoomKey() != "9c1d2e3f4a5b" ||
		ended.PayloadString(store.PayloadRoom) != "r7k3mx" {
		t.Fatalf("end event = %+v payload=%s", ended, ended.Payload)
	}
	if s := ended.PayloadString(store.PayloadSummary); s != "a dynamic room was ended by op@example.com" {
		t.Fatalf("dynamic end summary = %q", s)
	}
	if s := events[3].PayloadString(store.PayloadSummary); s != "a static room was deleted by op@example.com" {
		t.Fatalf("static delete summary = %q", s)
	}
	// No summary names a code (docs/44 D16).
	for _, ev := range events {
		s := ev.PayloadString(store.PayloadSummary)
		if strings.Contains(s, "r7k3mx") || strings.Contains(s, "R7K3MX") || strings.Contains(strings.ToLower(s), "tuhisroom") {
			t.Fatalf("summary %q names a room code", s)
		}
	}

	// Gone is a 404, not a silent 204: "end" of a room that was not there is
	// something the operator should hear about.
	if code := h.errorCode(http.MethodDelete, "/api/v1/rooms/tuhisroom", nil, http.StatusNotFound); code != api.CodeNotFound {
		t.Fatalf("delete of a gone room code = %q", code)
	}
	if code := h.errorCode(http.MethodPost, "/api/v1/rooms/r7k3mx/end", nil, http.StatusNotFound); code != api.CodeNotFound {
		t.Fatalf("end of a gone room code = %q", code)
	}
	if code := h.errorCode(http.MethodDelete, "/api/v1/rooms/not%20a%20code", nil, http.StatusNotFound); code != api.CodeNotFound {
		t.Fatalf("delete of a malformed code = %q", code)
	}
}

// A Kubernetes API failure is a 500 with a log line, and nothing is recorded:
// unlike a ban there is no row to be ahead of the CR, so there is no 202.
func TestRoomsMutationFailureRecordsNothing(t *testing.T) {
	f := newFakeRooms()
	f.err = errors.New("kubernetes API is unreachable")
	rec := &memoryRecorder{}
	h := newHarnessWithoutPostgres(t, withRooms(f, rec))
	// The store is unreachable in this harness too, so the generic exit says
	// 503; the point is that it is an error and that no event exists.
	status, _ := h.raw(http.MethodPost, "/api/v1/rooms", map[string]any{"code": "TuhisRoom"})
	if status/100 != 5 {
		t.Fatalf("status = %d, want a 5xx", status)
	}
	if got := rec.all(); len(got) != 0 {
		t.Fatalf("a failed create recorded %+v", got)
	}
	if code := h.errorCode(http.MethodGet, "/api/v1/rooms", nil, http.StatusServiceUnavailable); code != api.CodeUnavailable {
		t.Fatalf("list failure code = %q", code)
	}
}
