package api_test

// R79 (docs/80 D13, D14): a static room's voice mapping, set by the operator
// on create or later through PUT /rooms/{name}/voice, and by the
// rooms-manager bot in its create body.

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Tuhis/gawk/gawk-admin/internal/api"
	"github.com/Tuhis/gawk/gawk-admin/internal/kube"
	"github.com/Tuhis/gawk/gawk-server/rooms"
)

var errBrokenRoom = errors.New("kubernetes API is unreachable")

// wireMumble is the room's `mumble` object, declared independently so a
// rename fails a test.
type wireMumble struct {
	Channel   string `json:"channel"`
	Temporary bool   `json:"temporary"`
}

type wireVoiceRoom struct {
	Name   string      `json:"name"`
	Kind   string      `json:"kind"`
	Mumble *wireMumble `json:"mumble"`
}

func TestRoomsCreateWithAVoiceChannel(t *testing.T) {
	f := newFakeRooms()
	rec := &memoryRecorder{}
	h := newHarnessWithoutPostgres(t, withRooms(f, rec))

	var out struct {
		Room wireVoiceRoom `json:"room"`
	}
	h.decode(http.MethodPost, "/api/v1/rooms", map[string]any{
		"code": "friends", "mumble": map[string]any{"channel": "Friends/Gaming"},
	}, http.StatusCreated, &out)
	if out.Room.Mumble == nil || out.Room.Mumble.Channel != "Friends/Gaming" || out.Room.Mumble.Temporary {
		t.Fatalf("created room's mumble = %+v", out.Room.Mumble)
	}
	m := f.rooms["friends"].Room.Spec.Integrations.Mumble
	if m == nil || m.Channel != "Friends/Gaming" || m.Temporary {
		t.Fatalf("the CR's integrations.mumble = %+v", m)
	}
	// The list and the detail report it.
	var list struct {
		Rooms []wireVoiceRoom `json:"rooms"`
	}
	h.decode(http.MethodGet, "/api/v1/rooms", nil, http.StatusOK, &list)
	if len(list.Rooms) != 1 || list.Rooms[0].Mumble == nil || list.Rooms[0].Mumble.Channel != "Friends/Gaming" {
		t.Fatalf("list = %+v", list.Rooms)
	}
	var detail wireVoiceRoom
	h.decode(http.MethodGet, "/api/v1/rooms/friends", nil, http.StatusOK, &detail)
	if detail.Mumble == nil || detail.Mumble.Channel != "Friends/Gaming" {
		t.Fatalf("detail = %+v", detail)
	}

	// The root channel is an empty mapping, not "no voice".
	var root struct {
		Room wireVoiceRoom `json:"room"`
	}
	h.decode(http.MethodPost, "/api/v1/rooms", map[string]any{"code": "lobby", "mumble": map[string]any{}}, http.StatusCreated, &root)
	if root.Room.Mumble == nil || root.Room.Mumble.Channel != "" {
		t.Fatalf("root channel room = %+v", root.Room.Mumble)
	}
	if f.rooms["lobby"].Room.Spec.Integrations.Mumble == nil {
		t.Fatal("an empty mapping (the root channel) was dropped")
	}

	// Without the object, or with null, the room has no voice.
	for _, body := range []map[string]any{{"code": "quiet"}, {"code": "quiet-2", "mumble": nil}} {
		_, raw := h.raw(http.MethodPost, "/api/v1/rooms", body)
		if strings.Contains(raw, `"mumble"`) {
			t.Fatalf("a room without voice reports a mapping: %s", raw)
		}
	}
	if f.rooms["quiet"].Room.Spec.Integrations.Mumble != nil || f.rooms["quiet-2"].Room.Spec.Integrations.Mumble != nil {
		t.Fatal("a room created without voice has a mapping")
	}
}

func TestRoomsCreateRefusesABadVoiceMapping(t *testing.T) {
	f := newFakeRooms()
	h := newHarnessWithoutPostgres(t, withRooms(f, &memoryRecorder{}))
	for name, mumble := range map[string]map[string]any{
		"temporary":         {"temporary": true},
		"temporary w/ path": {"temporary": true, "channel": "Friends"},
		"control character": {"channel": "Friends\nGaming"},
		"too long":          {"channel": strings.Repeat("x", rooms.MaxMumbleChannelLen+1)},
		"unknown field":     {"server": "mumble.example"},
	} {
		if code := h.errorCode(http.MethodPost, "/api/v1/rooms", map[string]any{"code": "crew", "mumble": mumble}, http.StatusBadRequest); code != api.CodeBadRequest {
			t.Errorf("%s: code %q", name, code)
		}
	}
	if len(f.rooms) != 0 {
		t.Fatalf("a refused create wrote a room: %+v", f.rooms)
	}
	// The longest path allowed is accepted.
	h.decode(http.MethodPost, "/api/v1/rooms", map[string]any{
		"code": "crew", "mumble": map[string]any{"channel": strings.Repeat("x", rooms.MaxMumbleChannelLen)},
	}, http.StatusCreated, nil)
}

func TestRoomsSetVoice(t *testing.T) {
	open := kube.RoomObject{Name: "lobby", Managed: true}
	open.Room.Name = "lobby"
	open.Room.Spec = rooms.RoomSpec{Kind: rooms.KindStatic, DisplayName: "Lobby"}
	open.Room.Status.Key = "9c1d2e3f4a5b"
	dyn := dynamicRoomObject("r7k3mx", "1c1d2e3f4a5b", "gawk-server-0", 0)
	f := newFakeRooms(open, dyn)
	rec := &memoryRecorder{}
	h := newHarnessWithoutPostgres(t, withRooms(f, rec))

	var room wireVoiceRoom
	h.decode(http.MethodPut, "/api/v1/rooms/Lobby/voice", map[string]any{"mumble": map[string]any{"channel": "Friends/Gaming"}}, http.StatusOK, &room)
	if room.Name != "lobby" || room.Mumble == nil || room.Mumble.Channel != "Friends/Gaming" {
		t.Fatalf("after PUT: %+v", room)
	}
	if got := f.rooms["lobby"].Room.Spec; got.Integrations.Mumble == nil || got.Integrations.Mumble.Channel != "Friends/Gaming" || got.DisplayName != "Lobby" {
		t.Fatalf("stored spec = %+v", got)
	}
	// Another channel replaces it; null, or an empty body, removes it.
	room = wireVoiceRoom{}
	h.decode(http.MethodPut, "/api/v1/rooms/lobby/voice", map[string]any{"mumble": map[string]any{"channel": "Raid"}}, http.StatusOK, &room)
	if room.Mumble == nil || room.Mumble.Channel != "Raid" {
		t.Fatalf("after the second PUT: %+v", room.Mumble)
	}
	for _, body := range []map[string]any{{"mumble": nil}, {}} {
		_, raw := h.raw(http.MethodPut, "/api/v1/rooms/lobby/voice", body)
		if strings.Contains(raw, `"mumble"`) || f.rooms["lobby"].Room.Spec.Integrations.Mumble != nil {
			t.Fatalf("PUT %v kept the mapping: %s", body, raw)
		}
	}

	// Validation, the kind rule and a missing room. None writes.
	for name, mumble := range map[string]map[string]any{
		"temporary":         {"temporary": true},
		"control character": {"channel": "a\x7fb"},
		"too long":          {"channel": strings.Repeat("x", rooms.MaxMumbleChannelLen+1)},
	} {
		if code := h.errorCode(http.MethodPut, "/api/v1/rooms/lobby/voice", map[string]any{"mumble": mumble}, http.StatusBadRequest); code != api.CodeBadRequest {
			t.Errorf("%s: code %q", name, code)
		}
	}
	h.errorCode(http.MethodPut, "/api/v1/rooms/lobby/voice", map[string]any{"voice": true}, http.StatusBadRequest)
	if code := h.errorCode(http.MethodPut, "/api/v1/rooms/r7k3mx/voice", map[string]any{"mumble": map[string]any{"channel": "x"}}, http.StatusConflict); code != api.CodeRoomNotStatic {
		t.Fatalf("a dynamic room: code %q", code)
	}
	if f.rooms["r7k3mx"].Room.Spec.Integrations.Mumble != nil || f.rooms["lobby"].Room.Spec.Integrations.Mumble != nil {
		t.Fatal("a refused PUT wrote a room")
	}
	h.errorCode(http.MethodPut, "/api/v1/rooms/nosuchroom/voice", map[string]any{}, http.StatusNotFound)
	h.errorCode(http.MethodPut, "/api/v1/rooms/-bad-/voice", map[string]any{}, http.StatusNotFound)

	// A broken CR is not written either.
	f.rooms["broken"] = kube.RoomObject{Name: "broken", Err: errBrokenRoom}
	h.errorCode(http.MethodPut, "/api/v1/rooms/broken/voice", map[string]any{}, http.StatusConflict)

	// No event type exists for a voice change; nothing is recorded.
	if evs := rec.all(); len(evs) != 0 {
		t.Fatalf("events = %+v", evs)
	}

	// A Kubernetes failure is the generic exit, a 5xx (503 here: this
	// harness's store is unreachable too).
	f.err = errBrokenRoom
	if status, _ := h.raw(http.MethodPut, "/api/v1/rooms/lobby/voice", map[string]any{}); status/100 != 5 {
		t.Fatalf("a failing write = %d, want a 5xx", status)
	}
}

// The voice route is the operator's: a rooms-manager or rooms-reader caller
// is refused, and with rooms off it does not exist.
func TestRoomsSetVoiceIsOperatorOnly(t *testing.T) {
	f := newFakeRooms()
	if _, err := f.CreateStatic(t.Context(), kube.StaticRoom{Code: "mine", CreatedBy: harnessSubject}); err != nil {
		t.Fatal(err)
	}
	h := newHarnessWithoutPostgres(t, withRooms(f, &memoryRecorder{}))
	for _, role := range []string{"rooms-manager", "rooms-reader"} {
		h.identity.Roles = []string{role}
		if status, _ := h.raw(http.MethodPut, "/api/v1/rooms/mine/voice", map[string]any{}); status != http.StatusForbidden {
			t.Errorf("PUT voice as %s = %d, want 403", role, status)
		}
	}
	off := newHarnessWithoutPostgres(t)
	off.errorCode(http.MethodPut, "/api/v1/rooms/mine/voice", map[string]any{}, http.StatusNotFound)
}
