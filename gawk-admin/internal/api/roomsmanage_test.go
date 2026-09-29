package api_test

// R60: the rooms-manager role (docs/62 RW2) — static room create and delete,
// and a delete that reaches only the rooms the caller itself created.

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Tuhis/gawk/gawk-admin/internal/api"
	"github.com/Tuhis/gawk/gawk-admin/internal/kube"
	"github.com/Tuhis/gawk/gawk-server/rooms"
)

// The harness identity's subject; a manager owns what it stamps with it.
const harnessSubject = "sub-1"

// D1: a token holding ONLY rooms-manager reaches create, delete and /me, and
// every other route in the table answers 403 — the R49 loop, so a route added
// later is covered the day it lands.
func TestRoomsManagerReachesExactlyCreateDeleteAndMe(t *testing.T) {
	f := newFakeRooms()
	h := newHarnessWithoutPostgres(t, withRooms(f, &memoryRecorder{}))
	// concretePath fills every {placeholder} with "placeholder"; make that the
	// manager's own room, so the delete is admitted by ownership too.
	if _, err := f.CreateStatic(t.Context(), kube.StaticRoom{Code: "placeholder", CreatedBy: harnessSubject}); err != nil {
		t.Fatal(err)
	}
	h.identity.Roles = []string{"rooms-manager"}

	allowed := map[string]bool{
		"GET /api/v1/me":              true,
		"POST /api/v1/rooms":          true,
		"DELETE /api/v1/rooms/{name}": true,
	}
	for _, r := range api.RouteTable() {
		if len(r.Roles) == 0 {
			continue
		}
		key := r.Method + " " + r.Pattern
		status, body := h.raw(r.Method, concretePath(r.Pattern), nil)
		if allowed[key] {
			// POST with no body is a 400 from the handler — past the role.
			if status == http.StatusForbidden || status >= 500 {
				t.Errorf("%s as rooms-manager = %d, want it admitted; body: %s", key, status, body)
			}
			continue
		}
		if status != http.StatusForbidden {
			t.Errorf("%s as rooms-manager = %d, want 403; body: %s", key, status, body)
		}
	}
}

// D2, D3: a manager creates a room stamped with its own subject and deletes
// it again; the audit rows name it as the actor.
func TestRoomsManagerCreatesAndDeletesItsOwnRoom(t *testing.T) {
	f := newFakeRooms()
	rec := &memoryRecorder{}
	h := newHarnessWithoutPostgres(t, withRooms(f, rec))
	h.identity.Email = ""
	h.identity.Roles = []string{"rooms-manager"}

	status, body := h.raw(http.MethodPost, "/api/v1/rooms", map[string]any{"code": "Gaming-CS2", "displayName": "CS2"})
	if status != http.StatusCreated {
		t.Fatalf("create as rooms-manager = %d; body: %s", status, body)
	}
	if got := f.rooms["gaming-cs2"].CreatedBy; got != harnessSubject {
		t.Fatalf("the created room's creator = %q, want the caller's sub %q", got, harnessSubject)
	}
	if strings.Contains(body, harnessSubject) {
		t.Fatalf("the creator's sub is served: %s", body)
	}
	// Idempotent provisioning: the same code again is 409 room_exists.
	if status, body := h.raw(http.MethodPost, "/api/v1/rooms", map[string]any{"code": "gaming-cs2"}); status != http.StatusConflict || !strings.Contains(body, api.CodeRoomExists) {
		t.Fatalf("second create = %d %s, want 409 room_exists", status, body)
	}

	if status, body := h.raw(http.MethodDelete, "/api/v1/rooms/GAMING-cs2", nil); status != http.StatusNoContent {
		t.Fatalf("delete of its own room = %d; body: %s", status, body)
	}
	if _, ok := f.rooms["gaming-cs2"]; ok {
		t.Fatal("the room is still there")
	}
	for _, ev := range rec.all() {
		if ev.Actor != harnessSubject {
			t.Errorf("%s recorded actor %q, want the service account's sub", ev.Type, ev.Actor)
		}
	}
	if len(rec.all()) != 2 {
		t.Fatalf("recorded %d events, want created + ended", len(rec.all()))
	}
}

// D3: everything a manager did not create is refused with 403 room_not_owned
// and left in place — another subject's room, an unstamped (pre-R60 or
// kubectl'd) room, a dynamic room and an undecodable one. An operator deletes
// every one of them.
func TestRoomsManagerCannotDeleteARoomItDidNotCreate(t *testing.T) {
	undecodable := kube.RoomObject{Name: "broken", Err: errors.New("bad spec"), CreatedBy: harnessSubject}
	unstamped := kube.RoomObject{Name: "legacy", Managed: true}
	unstamped.Room.Spec = rooms.RoomSpec{Kind: rooms.KindStatic, DisplayCode: "legacy"}
	// A dynamic room stamped with the caller's sub cannot happen through the
	// API; the kind is checked anyway.
	dynamic := dynamicRoomObject("r7k3mx", "9c1d2e3f4a5b", "", 0)
	dynamic.CreatedBy = harnessSubject

	f := newFakeRooms(undecodable, unstamped, dynamic)
	if _, err := f.CreateStatic(t.Context(), kube.StaticRoom{Code: "theirs", CreatedBy: "someone-else"}); err != nil {
		t.Fatal(err)
	}
	h := newHarnessWithoutPostgres(t, withRooms(f, &memoryRecorder{}))

	names := []string{"theirs", "legacy", "r7k3mx", "broken"}
	h.identity.Roles = []string{"rooms-manager"}
	for _, name := range names {
		status, body := h.raw(http.MethodDelete, "/api/v1/rooms/"+name, nil)
		if status != http.StatusForbidden || !strings.Contains(body, api.CodeRoomNotOwned) {
			t.Errorf("delete %s as rooms-manager = %d %s, want 403 %s", name, status, body, api.CodeRoomNotOwned)
		}
		if _, ok := f.rooms[name]; !ok {
			t.Errorf("a refused delete removed %s", name)
		}
	}
	// A room that is not there is still a 404, not a 403.
	if status, _ := h.raw(http.MethodDelete, "/api/v1/rooms/nothere", nil); status != http.StatusNotFound {
		t.Errorf("delete of a missing room as rooms-manager = %d, want 404", status)
	}

	// Holding operator as well lifts the rule: the operator deletes anything.
	h.identity.Roles = []string{"rooms-manager", "operator"}
	for _, name := range names {
		if status, body := h.raw(http.MethodDelete, "/api/v1/rooms/"+name, nil); status != http.StatusNoContent {
			t.Errorf("delete %s as operator = %d %s, want 204", name, status, body)
		}
	}
}

// D2: an operator's create is stamped too, which is what keeps a manager off
// it later.
func TestOperatorCreatedRoomsAreStampedAndNotAManagers(t *testing.T) {
	f := newFakeRooms()
	h := newHarnessWithoutPostgres(t, withRooms(f, &memoryRecorder{}))
	h.identity.Subject = "operator-sub"
	if status, body := h.raw(http.MethodPost, "/api/v1/rooms", map[string]any{"code": "standup"}); status != http.StatusCreated {
		t.Fatalf("operator create = %d %s", status, body)
	}
	if got := f.rooms["standup"].CreatedBy; got != "operator-sub" {
		t.Fatalf("operator-created room's creator = %q", got)
	}
	h.identity.Subject = harnessSubject
	h.identity.Roles = []string{"rooms-manager"}
	if status, _ := h.raw(http.MethodDelete, "/api/v1/rooms/standup", nil); status != http.StatusForbidden {
		t.Fatalf("a manager deleted an operator's room: %d", status)
	}
}

// D4: `end` keeps refusing a static room, now through the guarded delete.
func TestEndStillRefusesAStaticRoom(t *testing.T) {
	f := newFakeRooms()
	if _, err := f.CreateStatic(t.Context(), kube.StaticRoom{Code: "standup"}); err != nil {
		t.Fatal(err)
	}
	h := newHarnessWithoutPostgres(t, withRooms(f, &memoryRecorder{}))
	status, body := h.raw(http.MethodPost, "/api/v1/rooms/standup/end", nil)
	if status != http.StatusConflict || !strings.Contains(body, api.CodeRoomNotDynamic) {
		t.Fatalf("end of a static room = %d %s, want 409 %s", status, body, api.CodeRoomNotDynamic)
	}
	if _, ok := f.rooms["standup"]; !ok {
		t.Fatal("end removed a static room")
	}
}

// D6: a deployment that sets -rooms-manager-role off grants the role nowhere.
func TestAnEmptyRoomsManagerRoleGrantsNothing(t *testing.T) {
	off := func(o *api.Options, _ *harness) { o.Config.RoomsManagerRole = "" }
	h := newHarnessWithoutPostgres(t, withRoomsEnabled(), off)
	h.identity.Roles = []string{"rooms-manager"}
	if status, _ := h.raw(http.MethodPost, "/api/v1/rooms", map[string]any{"code": "nope-room"}); status != http.StatusForbidden {
		t.Fatalf("POST /rooms with the role unconfigured = %d, want 403", status)
	}
}

// D6: the served document names the manager role as this deployment spells
// it, and drops it where the deployment grants it nowhere.
func TestServedDocumentNamesTheConfiguredManagerRole(t *testing.T) {
	renamed := func(o *api.Options, _ *harness) { o.Config.RoomsManagerRole = "mumble-provisioner" }
	h := newHarnessWithoutPostgres(t, renamed)
	if _, body := h.raw(http.MethodGet, "/api/v1/openapi.json", nil); !strings.Contains(body, `"mumble-provisioner"`) ||
		strings.Contains(body, `"rooms-manager"]`) {
		t.Fatal("the served document does not name the configured rooms-manager role in place of the symbolic one")
	}
	off := func(o *api.Options, _ *harness) { o.Config.RoomsManagerRole = "" }
	h = newHarnessWithoutPostgres(t, off)
	if _, body := h.raw(http.MethodGet, "/api/v1/openapi.json", nil); strings.Contains(body, `"rooms-manager"]`) {
		t.Fatal("the served document names rooms-manager although this deployment grants it nowhere")
	}
}
