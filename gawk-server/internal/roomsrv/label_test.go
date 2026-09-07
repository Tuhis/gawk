package roomsrv

import (
	"testing"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// Tile labels are de-duplicated within a room the way nicknames are (docs/44
// D10): a second broadcast attaching with a label already on the stage gets
// " (2)". Found 2026-09-06 with two tabs of one broadcaster page, where the
// one name field feeds both the nickname and the label.
func TestAttachLabelsAreUniqueWithinARoom(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.MaxBroadcasts = 4 })
	res := f.mint(t, "ABCDEF") // attached with label "pc" by the fixture
	viewer, vc := f.join(t, res.Code, "v", Grants{}, nil)
	_ = viewer
	vc.nextState(t)

	p, c := f.join(t, res.Code, "second", Grants{AttachOK: true}, nil)
	c.nextState(t)
	vc.nextEvent(t, wire.RoomEventParticipantJoined)

	// Same label as the minted attachment → suffixed.
	f.bc.set("GHJKMN", BroadcastState{Live: true})
	p.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandAttach, BroadcastID: "GHJKMN", ResumeToken: f.tokens.MintResume("GHJKMN"), Label: "pc"})
	if e := vc.nextEvent(t, wire.RoomEventAttachmentAdded); e.Attachment.Label != "pc (2)" {
		t.Fatalf("second 'pc' attached as %q, want %q", e.Attachment.Label, "pc (2)")
	}
	vc.nextEvent(t, wire.RoomEventParticipantUpdated)

	// A third takes the next free suffix; a distinct label is left alone.
	f.bc.set("PQRSTU", BroadcastState{Live: true})
	p.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandAttach, BroadcastID: "PQRSTU", ResumeToken: f.tokens.MintResume("PQRSTU"), Label: "pc"})
	if e := vc.nextEvent(t, wire.RoomEventAttachmentAdded); e.Attachment.Label != "pc (3)" {
		t.Fatalf("third 'pc' attached as %q, want %q", e.Attachment.Label, "pc (3)")
	}
	vc.nextEvent(t, wire.RoomEventParticipantUpdated)
	f.bc.set("VWXYZ2", BroadcastState{Live: true})
	p.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandAttach, BroadcastID: "VWXYZ2", ResumeToken: f.tokens.MintResume("VWXYZ2"), Label: "laptop"})
	if e := vc.nextEvent(t, wire.RoomEventAttachmentAdded); e.Attachment.Label != "laptop" {
		t.Fatalf("distinct label changed to %q", e.Attachment.Label)
	}
	vc.nextEvent(t, wire.RoomEventParticipantUpdated)

	// A re-attach that relabels to a taken name is suffixed too, and a
	// re-attach keeping its own label keeps it (its own entry is not a
	// collision with itself).
	p.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandAttach, BroadcastID: "VWXYZ2", ResumeToken: f.tokens.MintResume("VWXYZ2"), Label: "pc"})
	if e := vc.nextEvent(t, wire.RoomEventAttachmentUpdated); e.Attachment.Label != "pc (4)" {
		t.Fatalf("relabel onto a taken name gave %q, want %q", e.Attachment.Label, "pc (4)")
	}
	vc.nextEvent(t, wire.RoomEventParticipantUpdated)
	p.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandAttach, BroadcastID: "GHJKMN", ResumeToken: f.tokens.MintResume("GHJKMN"), Label: "pc (2)"})
	if e := vc.nextEvent(t, wire.RoomEventAttachmentUpdated); e.Attachment.Label != "pc (2)" {
		t.Fatalf("re-attach with its own label gave %q, want it kept", e.Attachment.Label)
	}
}

func TestUniqueLabelRespectsTheWireLimit(t *testing.T) {
	rm := &room{}
	long := ""
	for len(long) < wire.MaxRoomLabelLen {
		long += "x"
	}
	rm.attachments = []*attachment{{id: "A", label: long}}
	got := rm.uniqueLabelLocked(long, "B")
	if len(got) > wire.MaxRoomLabelLen || got[len(got)-4:] != " (2)" {
		t.Fatalf("suffixed label %q (len %d) breaks the %d-byte limit or lost its suffix", got, len(got), wire.MaxRoomLabelLen)
	}
	if rm.uniqueLabelLocked("", "B") != "" {
		t.Fatal("an empty label must stay empty (clients show the ID)")
	}
}
