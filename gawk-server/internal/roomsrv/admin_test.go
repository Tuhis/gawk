package roomsrv

import (
	"testing"

	"github.com/Tuhis/gawk/gawk-server/events"
	"github.com/Tuhis/gawk/gawk-server/rooms"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// R49 RA1 (docs/50 D2): the admin snapshot carries the raw code, the roster
// with who is streaming, and each attachment's live state — the facts the
// Room CR never holds.
func TestAdminRoomsSnapshotsRosterAndAttachments(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.Obfuscate = func(s string) string { return "key-" + s } })
	if got := f.reg.AdminRooms(); len(got) != 0 {
		t.Fatalf("empty registry: %+v", got)
	}
	res := f.mint(t, "ABCDEF")
	streamer, sc := f.join(t, res.Code, "tuhis", Grants{Creator: true, AttachOK: true}, res.CreatorToken)
	sc.nextState(t)
	_, vc := f.join(t, res.Code, "viewer", Grants{}, nil)
	vc.nextState(t)

	// A second broadcast, attached while its broadcaster is away.
	f.bc.set("GHJKMN", BroadcastState{Live: false, Viewers: 3})
	streamer.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandAttach, BroadcastID: "GHJKMN", ResumeToken: f.tokens.MintResume("GHJKMN"), Label: "laptop"})
	vc.nextEvent(t, wire.RoomEventAttachmentAdded)

	got := f.reg.AdminRooms()
	if len(got) != 1 {
		t.Fatalf("rooms = %+v", got)
	}
	rm := got[0]
	if rm.Code != res.Code || rm.Key != "key-"+res.Code || rm.Kind != rooms.KindDynamic || rm.DisplayCode != res.Display {
		t.Errorf("identity fields = %+v", rm)
	}
	if rm.CreatedAt.IsZero() || rm.EmptySince != nil {
		t.Errorf("createdAt %v, emptySince %v", rm.CreatedAt, rm.EmptySince)
	}
	if len(rm.Attachments) != 2 {
		t.Fatalf("attachments = %+v", rm.Attachments)
	}
	if a := rm.Attachments[0]; a.BroadcastID != "ABCDEF" || a.Label != "pc" || !a.Live || a.Viewers != 1 || a.AttachedAt.IsZero() {
		t.Errorf("first attachment = %+v", a)
	}
	if a := rm.Attachments[1]; a.BroadcastID != "GHJKMN" || a.Label != "laptop" || a.Live || a.Viewers != 3 {
		t.Errorf("away attachment = %+v", a)
	}
	if len(rm.Participants) != 2 {
		t.Fatalf("participants = %+v", rm.Participants)
	}
	p0, p1 := rm.Participants[0], rm.Participants[1]
	if p0.ID >= p1.ID {
		t.Errorf("participants not sorted by id: %+v", rm.Participants)
	}
	if p0.Nickname != "tuhis" || !p0.Streaming || p0.Speaking || p0.Identity != "" || p0.ClientKind != events.ClientKindWebViewer {
		t.Errorf("streamer = %+v", p0)
	}
	if p1.Nickname != "viewer" || p1.Streaming || p1.Speaking {
		t.Errorf("viewer = %+v", p1)
	}
}

// An empty room reports when it went empty; an ended one is gone.
func TestAdminRoomsReportsEmptySinceAndDropsEndedRooms(t *testing.T) {
	f := newFixture(t, nil)
	res := f.mint(t, "ABCDEF")
	p, c := f.join(t, res.Code, "v", Grants{}, nil)
	c.nextState(t)
	p.Leave()
	waitFor(t, func() bool {
		got := f.reg.AdminRooms()
		return len(got) == 1 && got[0].EmptySince != nil && len(got[0].Participants) == 0
	}, "emptySince after the last leave")

	f.reg.EndRoom(res.Code, wire.RoomEndReasonCreator)
	waitFor(t, func() bool { return len(f.reg.AdminRooms()) == 0 }, "ended room gone from the snapshot")
}
