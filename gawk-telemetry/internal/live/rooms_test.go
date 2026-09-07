package live

import (
	"encoding/json"
	"strings"
	"testing"
)

// R42 (RM8): the live projection carries the room a client said it was in,
// so the fleet page can chip it, and a session outside a room says nothing at
// all — `/live` for a room-less fleet is byte-identical to pre-R42.
func TestLiveSessionCarriesTheClientsRoomKey(t *testing.T) {
	p, _ := newProj()
	inRoom := batch("0a0000000000000000000001", "viewer", healthyViewer())
	inRoom.RoomKey = "0a1b2c3d4e5f"
	p.ObserveClient(inRoom, "Chrome 152", "Windows", "0.33.2")
	p.ObserveClient(batch("0a0000000000000000000002", "viewer", healthyViewer()), "Chrome 152", "Windows", "0.33.2")

	snap := p.Snapshot()
	if s := findSession(snap, "0a0000000000000000000001"); s == nil || s.RoomKey != "0a1b2c3d4e5f" {
		t.Errorf("room session = %+v, want roomKey 0a1b2c3d4e5f", s)
	}
	b, _ := json.Marshal(findSession(snap, "0a0000000000000000000002"))
	if strings.Contains(string(b), "roomKey") {
		t.Errorf("room-less session mentions roomKey: %s", b)
	}

	// A key that arrives on a later batch still attaches; a later batch that
	// omits it does not detach.
	late := batch("0a0000000000000000000002", "viewer", healthyViewer())
	late.RoomKey = "ffffffffffff"
	p.ObserveClient(late, "Chrome 152", "Windows", "0.33.2")
	p.ObserveClient(batch("0a0000000000000000000002", "viewer", healthyViewer()), "Chrome 152", "Windows", "0.33.2")
	if s := findSession(p.Snapshot(), "0a0000000000000000000002"); s == nil || s.RoomKey != "ffffffffffff" {
		t.Errorf("late-keyed session = %+v, want roomKey ffffffffffff", s)
	}
}
