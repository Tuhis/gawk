package readapi

// R42 rooms, chunk RM8 (docs/44 §4.10): a session that started inside a room
// carries the relay's HMAC'd room key, so the R31 UI can group it with its
// room. These tests pin the read surface of that — and its one hard rule,
// which is that nothing here moves the MCP defaults.

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const roomKey = "0a1b2c3d4e5f"

// A session's room is on its DETAIL envelope, next to the broadcast key it
// hangs off, and only there: the default (MCP) response stays byte-identical
// (TestR31DefaultsAreByteIdentical covers the negative half).
func TestSessionDetailCarriesItsRoomKey(t *testing.T) {
	f := newFixture(t)
	f.roomKey = roomKey
	sid := "0a0000000000000000000001"
	f.seed(t, sid, "viewer", healthyViewerStats(5), nil)

	tl, err := f.api.GetSessionDetail(sid, SessionQuery{Detail: true})
	if err != nil {
		t.Fatal(err)
	}
	if tl.RoomKey != roomKey {
		t.Errorf("detail roomKey = %q, want %q", tl.RoomKey, roomKey)
	}
	// And a session outside any room has no key, rather than an empty one.
	f.roomKey = ""
	f.seed(t, "0a0000000000000000000002", "viewer", healthyViewerStats(5), nil)
	b, _ := json.Marshal(mustDetail(t, f, "0a0000000000000000000002"))
	if strings.Contains(string(b), "roomKey") {
		t.Errorf("a room-less session's detail mentions roomKey: %s", b)
	}
}

func mustDetail(t *testing.T, f *fixture, sid string) *Timeline {
	t.Helper()
	tl, err := f.api.GetSessionDetail(sid, SessionQuery{Detail: true})
	if err != nil {
		t.Fatal(err)
	}
	return tl
}

// The rooms view is `/v1/history/sessions?room=<key>` grouped by broadcast on
// the client — so the filter must return EVERY session of the room across
// its broadcasts, and nothing from outside it, and every row must say which
// broadcast it belongs to so the grouping is possible.
func TestHistoryFiltersByRoomAcrossBroadcasts(t *testing.T) {
	f := newFixture(t)
	f.roomKey = roomKey
	f.seed(t, "0b0000000000000000000001", "broadcaster", healthyBroadcasterStats(5), nil)
	f.seed(t, "0b0000000000000000000002", "viewer", healthyViewerStats(5), nil)
	f.roomKey = "ffffffffffff"
	f.seed(t, "0b0000000000000000000003", "viewer", healthyViewerStats(5), nil)
	f.roomKey = ""
	f.seed(t, "0b0000000000000000000004", "viewer", healthyViewerStats(5), nil)

	page, err := f.api.SearchSessions(HistoryQuery{From: f.now.Add(-time.Hour), RoomKey: roomKey})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || len(page.Rows) != 2 {
		t.Fatalf("room filter returned %d rows (total %d), want 2", len(page.Rows), page.Total)
	}
	for _, r := range page.Rows {
		if r.RoomKey != roomKey {
			t.Errorf("row %s roomKey = %q, want %q", r.SessionID, r.RoomKey, roomKey)
		}
		if r.BroadcastKey == "" {
			t.Errorf("row %s has no broadcastKey; the rooms view groups by it", r.SessionID)
		}
	}
	// Unfiltered, the key is on the rows that have one and absent otherwise.
	all, err := f.api.SearchSessions(HistoryQuery{From: f.now.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]string{}
	for _, r := range all.Rows {
		keys[r.SessionID] = r.RoomKey
	}
	if keys["0b0000000000000000000003"] != "ffffffffffff" || keys["0b0000000000000000000004"] != "" {
		t.Errorf("unfiltered rows carry the wrong room keys: %v", keys)
	}

	// The broadcast half: the room's broadcast row names the room, and the
	// filter keeps only broadcasts with a session in it.
	bp, err := f.api.SearchBroadcasts(HistoryQuery{From: f.now.Add(-time.Hour), RoomKey: roomKey})
	if err != nil {
		t.Fatal(err)
	}
	if len(bp.Rows) != 1 || bp.Rows[0].RoomKey != roomKey || bp.Rows[0].Sessions != 2 {
		t.Errorf("broadcast rows for the room = %+v, want one row in the room with 2 sessions", bp.Rows)
	}
}

// The room filter is a query parameter on the same route, `room=`, so the
// browser's fetch is one line longer and no new endpoint had to be documented.
func TestHistoryRoomFilterIsAQueryParameter(t *testing.T) {
	f := newFixture(t)
	f.roomKey = roomKey
	f.seed(t, "0c0000000000000000000001", "viewer", healthyViewerStats(5), nil)
	f.roomKey = ""
	f.seed(t, "0c0000000000000000000002", "viewer", healthyViewerStats(5), nil)

	srv := httptest.NewServer(f.api.Handler())
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/v1/history/sessions?since=1h&room=" + roomKey)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var page HistoryPage
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 1 || page.Rows[0].SessionID != "0c0000000000000000000001" {
		t.Errorf("room= over HTTP returned %+v, want only the room's session", page.Rows)
	}
}

// `/v1/history/rooms/{key}` is the same page with the key in the path — the
// URL a room is addressed by — and must agree byte-for-byte with the query
// form, or the two would drift into two projections of one thing.
func TestHistoryRoomsRouteIsTheRoomFilterByPath(t *testing.T) {
	f := newFixture(t)
	f.roomKey = roomKey
	f.seed(t, "0e0000000000000000000001", "broadcaster", healthyBroadcasterStats(5), nil)
	f.seed(t, "0e0000000000000000000002", "viewer", healthyViewerStats(5), nil)
	f.roomKey = ""
	f.seed(t, "0e0000000000000000000003", "viewer", healthyViewerStats(5), nil)

	srv := httptest.NewServer(f.api.Handler())
	defer srv.Close()
	get := func(path string) []byte {
		t.Helper()
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("GET %s: HTTP %d", path, resp.StatusCode)
		}
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	byPath := get("/v1/history/rooms/" + roomKey + "?since=1h")
	byQuery := get("/v1/history/sessions?since=1h&room=" + roomKey)
	if string(byPath) != string(byQuery) {
		t.Errorf("path and query forms differ:\n%s\n%s", byPath, byQuery)
	}
	var page HistoryPage
	if err := json.Unmarshal(byPath, &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 {
		t.Errorf("room page total = %d, want 2 (the room-less session excluded)", page.Total)
	}
	// The path wins over a contradicting query filter: the URL names the room.
	other := get("/v1/history/rooms/" + roomKey + "?since=1h&room=ffffffffffff")
	if string(other) != string(byPath) {
		t.Errorf("room= in the query overrode the path key:\n%s", other)
	}
	// An unknown room is an empty page, like an unknown broadcast= filter.
	if err := json.Unmarshal(get("/v1/history/rooms/ffffffffffff?since=1h"), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 0 || len(page.Rows) != 0 {
		t.Errorf("unknown room returned %+v, want an empty page", page.Rows)
	}
}

// `/v1/sessions` is the MCP default and must not grow a column because a
// browser wanted one (UD1): the room key lives on the history row, not on
// SessionSummary. Pinned here because the two projections share a struct and
// a well-meaning "add it to the summary" would pass every other test.
func TestListSessionsDoesNotCarryTheRoomKey(t *testing.T) {
	f := newFixture(t)
	f.roomKey = roomKey
	f.seed(t, "0d0000000000000000000001", "viewer", healthyViewerStats(5), nil)

	for _, path := range []string{"/v1/sessions?since=1h", "/v1/broadcasts?since=1h"} {
		srv := httptest.NewServer(f.api.Handler())
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		srv.Close()
		body := string(raw)
		if strings.Contains(body, "roomKey") {
			t.Errorf("%s carries roomKey; the MCP default must stay byte-identical:\n%s", path, body)
		}
		if !strings.Contains(body, bkey) {
			t.Errorf("%s does not even list the seeded session: %s", path, body)
		}
	}
}

// --- resolve: room code -> room key ----------------------------------------

// A GOLDEN VECTOR for the room half of the mirror. roomsrv keys a room by
// Registry.ObfuscateID(rooms.NormalizeCode(code)), and NormalizeCode LOWER-cases
// (the code doubles as a DNS-1123 CR name) where broadcast IDs upper-case. So
// the room key for "ABC234" is the digest of "abc234", NOT the broadcast key
// for the same six characters — and the two must differ, or the UI would send
// an operator looking for a room to the broadcast of the same name.
//
// Verified 2026-09-04:
//
//	printf 'abc234' | openssl dgst -sha256 -mac HMAC \
//	    -macopt hexkey:$(printf 'ab%.0s' {1..32}) -binary | xxd -p | head -c 12
func TestRoomObfuscationLowerCasesLikeTheRelay(t *testing.T) {
	key, _ := hex.DecodeString(strings.Repeat("ab", 32))
	want := obfuscate(key, "abc234")
	if want == obfuscate(key, "ABC234") {
		t.Fatal("room and broadcast digests coincide; the test cannot tell the cases apart")
	}
	api := newResolveAPI(t, key)
	for _, code := range []string{"abc234", "ABC234", "  Abc234 "} {
		rec := post(t, api.Handler(), "/v1/resolve", `{"room":"`+code+`"}`)
		if rec.Code != 200 {
			t.Fatalf("HTTP %d for %q, body %s", rec.Code, code, rec.Body.String())
		}
		var got resolveResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.RoomKey != want {
			t.Errorf("room %q -> %q, want %q (the relay's key for the lower-cased code)", code, got.RoomKey, want)
		}
		if got.BroadcastKey != "" {
			t.Errorf("room resolve also returned a broadcastKey %q; a room answer names a room", got.BroadcastKey)
		}
	}
}

// A static slug resolves too — rooms are not only six-character codes.
func TestRoomResolveAcceptsAStaticSlug(t *testing.T) {
	key, _ := hex.DecodeString(strings.Repeat("cd", 32))
	api := newResolveAPI(t, key)
	rec := post(t, api.Handler(), "/v1/resolve", `{"room":"Friday-Night"}`)
	if rec.Code != 200 {
		t.Fatalf("HTTP %d, body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), obfuscate(key, "friday-night")) {
		t.Errorf("slug resolved to %s, want the digest of its lower-cased form", rec.Body.String())
	}
}

// Same posture as the broadcast half: nothing without a stats key, and junk is
// rejected before any digest is computed.
func TestRoomResolveSharesTheBroadcastGates(t *testing.T) {
	off := newResolveAPI(t, nil)
	if rec := post(t, off.Handler(), "/v1/resolve", `{"room":"abc234"}`); rec.Code != http.StatusNotImplemented {
		t.Errorf("HTTP %d without a stats key, want 501", rec.Code)
	}
	key, _ := hex.DecodeString(strings.Repeat("ef", 32))
	api := newResolveAPI(t, key)
	for name, body := range map[string]string{
		"blank room":  `{"room":"   "}`,
		"oversized":   `{"room":"` + strings.Repeat("a", 500) + `"}`,
		"neither set": `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			if rec := post(t, api.Handler(), "/v1/resolve", body); rec.Code != 400 {
				t.Errorf("HTTP %d, want 400", rec.Code)
			}
		})
	}
	// The broadcast answer did not change shape: still exactly {"broadcastKey"}.
	rec := post(t, api.Handler(), "/v1/resolve", `{"code":"ABC234"}`)
	if strings.Contains(rec.Body.String(), "roomKey") {
		t.Errorf("a code resolve now mentions roomKey: %s", rec.Body.String())
	}
}
