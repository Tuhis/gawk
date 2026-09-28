package api_test

// R49: the merged room read API and the rooms-reader role (docs/50 RA2, RA3).

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Tuhis/gawk/gawk-admin/internal/api"
	"github.com/Tuhis/gawk/gawk-admin/internal/kube"
	"github.com/Tuhis/gawk/gawk-admin/internal/relayscan"
	"github.com/Tuhis/gawk/gawk-server/rooms"
)

func liveRoom(code, display, pod string) relayscan.RoomAggregate {
	at := time.Date(2026, 9, 29, 10, 0, 0, 0, time.UTC)
	return relayscan.RoomAggregate{Pod: pod, Room: relayscan.Room{
		Code: code, Key: "key-" + code, Kind: rooms.KindStatic, DisplayCode: display, CreatedAt: at,
		Attachments: []relayscan.RoomAttachment{
			{BroadcastID: "ABC234", Label: "main pc", Live: true, Viewers: 3, AttachedAt: at},
			{BroadcastID: "DEF567", Label: "laptop", Live: false, Viewers: 0, AttachedAt: at},
		},
		Participants: []relayscan.RoomParticipant{
			{ID: 1, Nickname: "tuhis", ClientKind: "native", Streaming: true},
			{ID: 2, Nickname: "kaveri", ClientKind: "web-broadcaster", Streaming: true},
			{ID: 3, Nickname: "guest-3", ClientKind: "web-viewer"},
		},
	}}
}

// staticRoomWithLiveHome is the common fixture: one static CR, homed on a pod
// that answered with a three-person roster.
func staticRoomWithLiveHome(t *testing.T, opts ...harnessOption) *harness {
	t.Helper()
	f := newFakeRooms()
	h := newHarnessWithoutPostgres(t, append([]harnessOption{withRooms(f, &memoryRecorder{})}, opts...)...)
	if _, err := f.CreateStatic(t.Context(), kube.StaticRoom{Code: "TuhisRoom"}); err != nil {
		t.Fatal(err)
	}
	h.fleet.set(relayscan.Snapshot{Rooms: []relayscan.RoomAggregate{liveRoom("tuhisroom", "TuhisRoom", "gawk-server-1")}})
	return h
}

// The list row: counts, live and a join link — and never the arrays, which
// are the detail's (D4).
func TestRoomsListMergesTheLiveRoster(t *testing.T) {
	h := staticRoomWithLiveHome(t)
	var body struct {
		Rooms []wireRoom `json:"rooms"`
	}
	h.decode(http.MethodGet, "/api/v1/rooms", nil, http.StatusOK, &body)
	if len(body.Rooms) != 1 {
		t.Fatalf("rooms = %+v", body.Rooms)
	}
	r := body.Rooms[0]
	if !r.Live || r.Counts.Participants != 3 || r.Counts.Streaming != 2 || r.Counts.Watching != 1 || r.Counts.Attachments != 2 {
		t.Fatalf("row = %+v", r)
	}
	if r.Links == nil || r.Links.Join != "https://gawk.example/#/room/TuhisRoom" {
		t.Fatalf("links = %+v", r.Links)
	}
	if r.HomeHolder != "gawk-server-1" || r.Key != "key-tuhisroom" {
		t.Fatalf("home/key from the scan: %+v", r)
	}
	if r.Attachments != nil || r.Participants != nil {
		t.Fatalf("a list row carries the detail arrays: %+v", r)
	}
}

// The detail: the roster and the attachments in full, by any spelling of the
// code; an unknown or malformed name is 404.
func TestRoomsGetReturnsTheRosterByAnySpelling(t *testing.T) {
	h := staticRoomWithLiveHome(t)
	for _, spelling := range []string{"TuhisRoom", "tuhisroom", "TUHISROOM"} {
		var r wireRoom
		h.decode(http.MethodGet, "/api/v1/rooms/"+spelling, nil, http.StatusOK, &r)
		if r.Name != "tuhisroom" || !r.Live || r.Attachments == nil || r.Participants == nil {
			t.Fatalf("%s: room = %+v", spelling, r)
		}
		atts, parts := *r.Attachments, *r.Participants
		if len(atts) != 2 || atts[0].BroadcastID != "ABC234" || atts[0].Live == nil || !*atts[0].Live ||
			atts[0].Viewers == nil || *atts[0].Viewers != 3 || atts[0].Label != "main pc" {
			t.Fatalf("%s: attachments = %+v", spelling, atts)
		}
		if atts[1].Live == nil || *atts[1].Live {
			t.Fatalf("%s: the away attachment reads live: %+v", spelling, atts[1])
		}
		if atts[0].Links == nil || atts[0].Links.Watch != "https://gawk.example/#/view/ABC234" {
			t.Fatalf("%s: watch link = %+v", spelling, atts[0].Links)
		}
		if len(parts) != 3 || parts[0].Nickname != "tuhis" || parts[0].ClientKind != "native" || !parts[0].Streaming ||
			parts[2].Streaming || parts[2].Speaking || parts[2].Identity != "" {
			t.Fatalf("%s: participants = %+v", spelling, parts)
		}
	}
	for _, name := range []string{"nosuchroom", "-bad-", "x"} {
		if code := h.errorCode(http.MethodGet, "/api/v1/rooms/"+name, nil, http.StatusNotFound); code != api.CodeNotFound {
			t.Fatalf("GET /rooms/%s code = %q", name, code)
		}
	}
}

// A homed room whose home pod did not answer renders from its CR: not live,
// roster empty (unknown, not nobody), attachments without live state (D3).
func TestRoomsWithAnUnreachableHomeRenderFromTheCR(t *testing.T) {
	obj := dynamicRoomObject("r7k3mx", "9c1d2e3f4a5b", "gawk-server-0", 1)
	attached := metav1.NewTime(time.Date(2026, 9, 3, 18, 1, 0, 0, time.UTC))
	obj.Room.Status.Attachments[0].Label = "pc"
	obj.Room.Status.Attachments[0].AttachedAt = &attached
	h := newHarnessWithoutPostgres(t, withRooms(newFakeRooms(obj), &memoryRecorder{}))
	h.fleet.set(relayscan.Snapshot{}) // gawk-server-0 did not answer

	var r wireRoom
	h.decode(http.MethodGet, "/api/v1/rooms/R7K3MX", nil, http.StatusOK, &r)
	if r.Live || r.Counts.Participants != 0 || r.Counts.Attachments != 1 {
		t.Fatalf("room = %+v", r)
	}
	if r.Participants == nil || len(*r.Participants) != 0 {
		t.Fatalf("participants = %v, want present and empty", r.Participants)
	}
	atts := *r.Attachments
	if len(atts) != 1 || atts[0].BroadcastID != "ABC234" || atts[0].Label != "pc" ||
		atts[0].Live != nil || atts[0].Viewers != nil || atts[0].AttachedAt != "2026-09-03T18:01:00Z" {
		t.Fatalf("attachments = %+v", atts)
	}

	// A fleet that cannot be enumerated at all degrades the same way rather
	// than failing the read: this view is how an operator ends a room.
	h.fleet.mu.Lock()
	h.fleet.err = errors.New("resolve: no such host")
	h.fleet.mu.Unlock()
	var list struct {
		Rooms []wireRoom `json:"rooms"`
	}
	h.decode(http.MethodGet, "/api/v1/rooms", nil, http.StatusOK, &list)
	if len(list.Rooms) != 1 || list.Rooms[0].Live {
		t.Fatalf("list with no fleet = %+v", list.Rooms)
	}
}

// A CR that cannot be decoded is still listed by name — and when a pod is home
// for that room, its live roster is not thrown away with the CR (review
// finding, 2026-09-29).
func TestAnUndecodableCRKeepsItsLiveRoster(t *testing.T) {
	broken := kube.RoomObject{Name: "tuhisroom", Err: errors.New("spec.kind: bad value")}
	h := newHarnessWithoutPostgres(t, withRooms(newFakeRooms(broken), &memoryRecorder{}))
	h.fleet.set(relayscan.Snapshot{Rooms: []relayscan.RoomAggregate{liveRoom("tuhisroom", "TuhisRoom", "gawk-server-1")}})
	var r wireRoom
	h.decode(http.MethodGet, "/api/v1/rooms/tuhisroom", nil, http.StatusOK, &r)
	if r.Kind != "" || !r.Live || r.Counts.Participants != 3 || r.Participants == nil || len(*r.Participants) != 3 {
		t.Fatalf("room = %+v", r)
	}
}

// A room some pod is home for but that has no CR is still shown (the union).
func TestRoomsOnlyTheScanKnowsAreListed(t *testing.T) {
	h := newHarnessWithoutPostgres(t, withRooms(newFakeRooms(), &memoryRecorder{}))
	h.fleet.set(relayscan.Snapshot{Rooms: []relayscan.RoomAggregate{liveRoom("abcdef", "ABCDEF", "gawk-server-2")}})
	var r wireRoom
	h.decode(http.MethodGet, "/api/v1/rooms/abcdef", nil, http.StatusOK, &r)
	if !r.Live || r.Code != "ABCDEF" || r.HomeHolder != "gawk-server-2" || r.Managed || len(*r.Participants) != 3 {
		t.Fatalf("room = %+v", r)
	}
}

// Without -app-base-url there is no link to build: links are omitted, never
// dead (D4).
func TestRoomLinksAreOmittedWithoutAnAppBaseURL(t *testing.T) {
	h := staticRoomWithLiveHome(t, func(o *api.Options, _ *harness) { o.Config.AppBaseURL = "" })
	_, raw := h.raw(http.MethodGet, "/api/v1/rooms/tuhisroom", nil)
	if strings.Contains(raw, `"links"`) {
		t.Fatalf("links without a base URL: %s", raw)
	}
}

// RA3 (docs/50 D1): a token holding ONLY rooms-reader reaches the two room
// reads and /me, and every other route in the table answers 403. The table
// makes this a loop, so a route added later is covered the day it lands.
func TestRoomsReaderReachesExactlyTheRoomReads(t *testing.T) {
	f := newFakeRooms()
	h := newHarnessWithoutPostgres(t, withRooms(f, &memoryRecorder{}))
	// concretePath fills every {placeholder} with "placeholder".
	if _, err := f.CreateStatic(t.Context(), kube.StaticRoom{Code: "placeholder"}); err != nil {
		t.Fatal(err)
	}
	h.identity.Roles = []string{"rooms-reader"}

	allowed := map[string]bool{
		"GET /api/v1/me":           true,
		"GET /api/v1/rooms":        true,
		"GET /api/v1/rooms/{name}": true,
	}
	for _, r := range api.RouteTable() {
		if len(r.Roles) == 0 {
			continue
		}
		key := r.Method + " " + r.Pattern
		status, body := h.raw(r.Method, concretePath(r.Pattern), nil)
		if allowed[key] {
			if status != http.StatusOK {
				t.Errorf("%s as rooms-reader = %d, want 200; body: %s", key, status, body)
			}
			continue
		}
		if status != http.StatusForbidden {
			t.Errorf("%s as rooms-reader = %d, want 403; body: %s", key, status, body)
		}
	}

	// A token with neither role is refused the reads too.
	h.identity.Roles = []string{"flagger"}
	for key := range allowed {
		method, path, _ := strings.Cut(key, " ")
		if status, _ := h.raw(method, concretePath(path), nil); status != http.StatusForbidden {
			t.Errorf("%s with neither role = %d, want 403", key, status)
		}
	}
}

// A pod whose rooms route failed says so on the relays view — it is why its
// rooms read as not live (docs/50 §6) — but only when rooms are on.
func TestRelaysViewShowsARoomsScrapeFailure(t *testing.T) {
	snap := relayscan.Snapshot{Pods: []relayscan.Pod{{Name: "gawk-server-0", Reachable: true, RoomsErr: "rooms returned 500"}}}
	var page struct {
		Relays []struct {
			Error string `json:"error"`
		} `json:"relays"`
	}
	on := newHarnessWithoutPostgres(t, withRoomsEnabled())
	on.fleet.set(snap)
	on.decode(http.MethodGet, "/api/v1/relays", nil, http.StatusOK, &page)
	if len(page.Relays) != 1 || page.Relays[0].Error != "rooms returned 500" {
		t.Fatalf("rooms on: relays = %+v", page.Relays)
	}
	off := newHarnessWithoutPostgres(t)
	off.fleet.set(snap)
	page.Relays = nil
	off.decode(http.MethodGet, "/api/v1/relays", nil, http.StatusOK, &page)
	if len(page.Relays) != 1 || page.Relays[0].Error != "" {
		t.Fatalf("rooms off: relays = %+v", page.Relays)
	}
}

// A deployment that sets -rooms-reader-role empty grants the role nowhere: the
// reads fall back to operator-only.
func TestAnEmptyRoomsReaderRoleGrantsNothing(t *testing.T) {
	off := func(o *api.Options, _ *harness) { o.Config.RoomsReaderRole = "" }
	h := newHarnessWithoutPostgres(t, withRoomsEnabled(), off)
	h.identity.Roles = []string{"rooms-reader"}
	if status, _ := h.raw(http.MethodGet, "/api/v1/rooms", nil); status != http.StatusForbidden {
		t.Fatalf("GET /rooms with the role unconfigured = %d, want 403", status)
	}
}
