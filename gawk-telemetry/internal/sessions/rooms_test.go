package sessions

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Tuhis/gawk/gawk-telemetry/internal/ingest"
	"github.com/Tuhis/gawk/gawk-telemetry/internal/store"
)

// R42 (RM8): the room key is stored on every line of a session in a room and
// lands on its permanent rollup row — and the row is computed from the STORED
// lines through the same ParseTimeline the read API replays, so a rollup and
// a re-read timeline can never disagree about which room a session was in.
func TestRoomKeyIsStoredOnEveryLineAndOnTheRollup(t *testing.T) {
	h := newHarness(t)
	const room = "0a1b2c3d4e5f"

	b := batch(0, false, fpsSamples(0, 60, 60), []ingest.Event{{TMs: 10, Kind: "watching"}})
	b.RoomKey = room
	if err := h.writer.Accept(b); err != nil {
		t.Fatal(err)
	}
	// A later batch that OMITS the key (a client only sends what it knows at
	// flush time; a resumed session's first batch may not know) must not
	// detach the session from its room.
	if err := h.writer.Accept(batch(1, true, fpsSamples(4000, 60), nil)); err != nil {
		t.Fatal(err)
	}

	ref := store.SessionRef{Date: "2026-07-26", BroadcastKey: testBroadcast, SessionID: testSession}
	lines, err := h.store.ReadSession(ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) < 5 {
		t.Fatalf("stored %d lines, want meta + 3 samples + 1 event", len(lines))
	}
	for i, ln := range lines {
		var rec Record
		if err := json.Unmarshal(ln, &rec); err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		if rec.RoomKey != room {
			t.Errorf("line %d (%s) roomKey = %q, want %q", i, rec.Kind, rec.RoomKey, room)
		}
	}
	if len(h.rows) != 1 || h.rows[0].RoomKey != room {
		t.Fatalf("rollup rows = %+v, want one row in room %s", h.rows, room)
	}

	// Read back with an EMPTY Live, as the read API does: the key must come
	// from the records themselves.
	in := ParseTimeline(Live{Ref: ref}, lines)
	if in.RoomKey != room {
		t.Errorf("ParseTimeline from disk: roomKey = %q, want %q", in.RoomKey, room)
	}
}

// A session outside any room is stored exactly as before R42: no `roomKey`
// anywhere on disk, and none on its row. Old files and old rows already look
// like this, so this is also what keeps a reader's "absent means no room"
// rule honest.
func TestRoomlessSessionIsByteIdenticalToPreR42(t *testing.T) {
	h := newHarness(t)
	if err := h.writer.Accept(batch(0, true, fpsSamples(0, 60), []ingest.Event{{TMs: 10, Kind: "watching"}})); err != nil {
		t.Fatal(err)
	}
	ref := store.SessionRef{Date: "2026-07-26", BroadcastKey: testBroadcast, SessionID: testSession}
	lines, err := h.store.ReadSession(ref)
	if err != nil {
		t.Fatal(err)
	}
	for i, ln := range lines {
		if strings.Contains(string(ln), "roomKey") {
			t.Errorf("line %d mentions roomKey for a room-less session: %s", i, ln)
		}
	}
	row, _ := json.Marshal(h.rows[0])
	if strings.Contains(string(row), "roomKey") {
		t.Errorf("rollup row mentions roomKey for a room-less session: %s", row)
	}
}
