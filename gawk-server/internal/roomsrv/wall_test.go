package roomsrv

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/Tuhis/gawk/gawk-server/rooms"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// R74 PP11 (docs/76 items 33-34): a static room with spec.pop is a party
// wall. It accepts only VOUCHED attaches — the PoP's link vouches for a
// broadcast it reserved — and refuses an ordinary Attach as a wrong attach
// secret would be, whoever sends it.

const wallPop = "assembly-2026"

func newWallFixture(t *testing.T, maxBroadcasts int) *fixture {
	t.Helper()
	f := newFixture(t, nil)
	if err := f.reg.UpsertStatic(StaticRoom{Code: "assembly-2026", Pop: wallPop, MaxBroadcasts: maxBroadcasts, MaxParticipants: 500}); err != nil {
		t.Fatal(err)
	}
	return f
}

// partyID is the n-th distinct broadcast ID (all in the broadcast alphabet).
func partyID(n int) string {
	const a = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"
	return fmt.Sprintf("PA%c%c%c%c", a[(n/27000)%31], a[(n/900)%31], a[(n/30)%31], a[n%30])
}

// Every party broadcast joins the wall, a fifth and later included: the
// wall's own spec.maxBroadcasts (the Pop's limit, 40 by default) applies,
// never the fleet's -max-room-broadcasts default of 4.
func TestWallTakesAFifthAndLaterVouchedBroadcast(t *testing.T) {
	f := newWallFixture(t, 40)
	_, c := f.join(t, "assembly-2026", "outsider", Grants{}, nil)
	c.nextState(t)
	for i := range 6 {
		id := partyID(i)
		if err := f.reg.AttachVouched("assembly-2026", wallPop, id, ""); err != nil {
			t.Fatalf("vouched attach #%d: %v", i+1, err)
		}
		ev := c.nextEvent(t, wire.RoomEventAttachmentAdded)
		if ev.Attachment.BroadcastID != id {
			t.Fatalf("attach #%d announced %s, want %s", i+1, ev.Attachment.BroadcastID, id)
		}
	}
	if got := len(f.reg.Attachments("assembly-2026")); got != 6 {
		t.Fatalf("wall has %d attachments, want 6", got)
	}
	// An unlabelled vouched attach gets "Stream n" from the PoP, but the
	// registry also takes a label as sent.
	if err := f.reg.AttachVouched("assembly-2026", wallPop, partyID(7), "tuhis"); err != nil {
		t.Fatal(err)
	}
	if ev := c.nextEvent(t, wire.RoomEventAttachmentAdded); ev.Attachment.Label != "tuhis" {
		t.Errorf("label = %q, want tuhis", ev.Attachment.Label)
	}
}

// The wall's own cap still holds, as ErrMaxBroadcasts.
func TestWallVouchedAttachPastTheCapIsRefused(t *testing.T) {
	f := newWallFixture(t, 2)
	for i := range 2 {
		if err := f.reg.AttachVouched("assembly-2026", wallPop, partyID(i), ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.reg.AttachVouched("assembly-2026", wallPop, partyID(2), ""); !errors.Is(err, ErrMaxBroadcasts) {
		t.Fatalf("third attach on a 2-slot wall: %v, want ErrMaxBroadcasts", err)
	}
	// Idempotent re-attach of one already there is not a new slot.
	if err := f.reg.AttachVouched("assembly-2026", wallPop, partyID(1), "renamed"); err != nil {
		t.Fatalf("re-attach: %v", err)
	}
}

// Only the wall's own PoP can vouch, only for a wall, only for a room that
// exists.
func TestWallVouchRefusals(t *testing.T) {
	f := newWallFixture(t, 40)
	if err := f.reg.UpsertStatic(StaticRoom{Code: "plain-room"}); err != nil {
		t.Fatal(err)
	}
	id := partyID(1)
	if err := f.reg.AttachVouched("assembly-2026", "another-pop", id, ""); !errors.Is(err, ErrForbidden) {
		t.Errorf("another PoP's vouch: %v, want ErrForbidden", err)
	}
	if err := f.reg.AttachVouched("plain-room", wallPop, id, ""); !errors.Is(err, ErrForbidden) {
		t.Errorf("vouch into a room that is no wall: %v, want ErrForbidden", err)
	}
	if err := f.reg.AttachVouched("no-such-wall", wallPop, id, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("vouch into a missing room: %v, want ErrNotFound", err)
	}
	if err := f.reg.AttachVouched("assembly-2026", wallPop, "not an id", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("vouch for a malformed ID: %v, want ErrNotFound", err)
	}
	if err := f.reg.DetachVouched("assembly-2026", "another-pop", id); !errors.Is(err, ErrForbidden) {
		t.Errorf("another PoP's detach: %v, want ErrForbidden", err)
	}
}

// An outsider's home broadcast cannot join the wall: the attach secret is
// refused at the gate (a wall has none to match), a session without one
// carries no attach grant, and the Attach itself is rejected.
func TestWallRefusesAnOrdinaryAttach(t *testing.T) {
	f := newWallFixture(t, 40)
	if _, err := f.reg.CheckJoin("assembly-2026", "", "", true); !errors.Is(err, ErrForbidden) {
		t.Errorf("CheckJoin with an (empty) attach secret: %v, want ErrForbidden", err)
	}
	if _, err := f.reg.CheckJoin("assembly-2026", "", "guess", true); !errors.Is(err, ErrForbidden) {
		t.Errorf("CheckJoin with an attach secret: %v, want ErrForbidden", err)
	}
	g, err := f.reg.CheckJoin("assembly-2026", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if g.AttachOK {
		t.Fatal("a wall granted AttachOK to a session without a vouch")
	}
	id := "ABCDEF"
	f.bc.set(id, BroadcastState{Live: true})
	// Even a session that somehow holds the grant is refused.
	p, c := f.join(t, "assembly-2026", "outsider", Grants{AttachOK: true}, nil)
	if st := c.nextState(t); st.Flags&wire.RoomStateFlagAttachOK == 0 {
		t.Fatal("test setup: the forced grant did not reach the state")
	}
	p.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandAttach, BroadcastID: id, ResumeToken: f.tokens.MintResume(id), Label: "mine"})
	ev := c.nextEvent(t, wire.RoomEventCommandRejected)
	if ev.Reason != wire.RoomRejectForbidden {
		t.Errorf("ordinary attach to the wall rejected with reason %d, want forbidden", ev.Reason)
	}
	if len(f.reg.Attachments("assembly-2026")) != 0 {
		t.Fatal("the outsider's broadcast is on the wall")
	}
}

// Hide (a vouched detach) removes; Show (a vouched attach) restores. An
// ordinary participant cannot detach a vouched tile: it has no owner
// participant, and a static room has no creator.
func TestWallHideAndShow(t *testing.T) {
	f := newWallFixture(t, 40)
	id := partyID(3)
	if err := f.reg.AttachVouched("assembly-2026", wallPop, id, "Stream 1"); err != nil {
		t.Fatal(err)
	}
	p, c := f.join(t, "assembly-2026", "guest", Grants{}, nil)
	if st := c.nextState(t); len(st.Attachments) != 1 {
		t.Fatalf("state attachments = %+v", st.Attachments)
	}
	p.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandDetach, BroadcastID: id})
	if ev := c.nextEvent(t, wire.RoomEventCommandRejected); ev.Reason != wire.RoomRejectForbidden {
		t.Errorf("a guest's detach of a vouched tile: reason %d, want forbidden", ev.Reason)
	}
	if err := f.reg.DetachVouched("assembly-2026", wallPop, id); err != nil {
		t.Fatalf("hide: %v", err)
	}
	if ev := c.nextEvent(t, wire.RoomEventAttachmentRemoved); ev.Attachment.BroadcastID != id || ev.Reason != wire.RoomDetachReasonPublisher {
		t.Errorf("hide announced %+v", ev)
	}
	if err := f.reg.DetachVouched("assembly-2026", wallPop, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("hiding twice: %v, want ErrNotFound", err)
	}
	if err := f.reg.AttachVouched("assembly-2026", wallPop, id, "Stream 1"); err != nil {
		t.Fatalf("show: %v", err)
	}
	if ev := c.nextEvent(t, wire.RoomEventAttachmentAdded); ev.Attachment.BroadcastID != id || ev.Attachment.Label != "Stream 1" {
		t.Errorf("show announced %+v", ev)
	}
}

// One room per broadcast (R42 D1), and the broadcaster's own choice wins:
// an explicit attach to another room moves it off the wall, and a mint
// from it does too. A vouched attach never pulls a broadcast out of a room
// its broadcaster chose.
func TestExplicitAttachElsewhereMovesABroadcastOffTheWall(t *testing.T) {
	f := newWallFixture(t, 40)
	_, wallC := f.join(t, "assembly-2026", "guest", Grants{}, nil)
	wallC.nextState(t)
	if err := f.reg.UpsertStatic(StaticRoom{Code: "lan-team"}); err != nil {
		t.Fatal(err)
	}
	id := "ABCDEF"
	f.bc.set(id, BroadcastState{Live: true})
	if err := f.reg.AttachVouched("assembly-2026", wallPop, id, ""); err != nil {
		t.Fatal(err)
	}
	wallC.nextEvent(t, wire.RoomEventAttachmentAdded)

	p, c := f.join(t, "lan-team", "tuhis", Grants{AttachOK: true}, nil)
	c.nextState(t)
	p.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandAttach, BroadcastID: id, ResumeToken: f.tokens.MintResume(id)})
	if ev := c.nextEvent(t, wire.RoomEventAttachmentAdded); ev.Attachment.BroadcastID != id {
		t.Fatalf("attach elsewhere = %+v", ev)
	}
	if ev := wallC.nextEvent(t, wire.RoomEventAttachmentRemoved); ev.Attachment.BroadcastID != id {
		t.Fatalf("the wall did not lose the broadcast: %+v", ev)
	}
	if err := f.reg.AttachVouched("assembly-2026", wallPop, id, ""); !errors.Is(err, ErrAlreadyAttached) {
		t.Fatalf("vouched attach of a broadcast in another room: %v, want ErrAlreadyAttached", err)
	}

	// A mint from a wall broadcast moves it too.
	id2 := "ABCDEG"
	if err := f.reg.AttachVouched("assembly-2026", wallPop, id2, ""); err != nil {
		t.Fatal(err)
	}
	wallC.nextEvent(t, wire.RoomEventAttachmentAdded)
	res := f.mint(t, id2)
	if !rooms.IsDynamicShape(res.Code) {
		t.Fatalf("mint = %+v", res)
	}
	if ev := wallC.nextEvent(t, wire.RoomEventAttachmentRemoved); ev.Attachment.BroadcastID != id2 {
		t.Fatalf("the mint did not move the broadcast off the wall: %+v", ev)
	}
}

// A protocol-1 client parses every snapshot and delta a wall produces:
// vouched tiles have no owner participant, and nothing new is on the wire.
func TestWallRecordsParseAsProtocol1(t *testing.T) {
	f := newWallFixture(t, 40)
	_, c := f.join(t, "assembly-2026", "guest", Grants{}, nil)
	st := c.nextState(t) // fakeConn.next already parses with the protocol-1 parsers
	if st.Flags&wire.RoomStateFlagAttachOK != 0 {
		t.Errorf("wall state flags 0x%02x grant attach", st.Flags)
	}
	for i := range 5 {
		if err := f.reg.AttachVouched("assembly-2026", wallPop, partyID(i), fmt.Sprintf("Stream %d", i+1)); err != nil {
			t.Fatal(err)
		}
		c.nextEvent(t, wire.RoomEventAttachmentAdded)
	}
	if err := f.reg.DetachVouched("assembly-2026", wallPop, partyID(2)); err != nil {
		t.Fatal(err)
	}
	c.nextEvent(t, wire.RoomEventAttachmentRemoved)
	_, c2 := f.join(t, "assembly-2026", "late", Grants{}, nil)
	st2 := c2.nextState(t)
	if len(st2.Attachments) != 4 {
		t.Fatalf("late joiner's snapshot has %d tiles, want 4", len(st2.Attachments))
	}
	for _, p := range st2.Participants {
		if p.Flags&wire.RoomParticipantFlagStreaming != 0 {
			t.Errorf("participant %d flagged streaming for a vouched tile", p.ID)
		}
	}
}

// A PoP's local wall comes from its link, never from -rooms-file: a file
// re-read must not end it.
func TestReplaceStaticKeepsAWall(t *testing.T) {
	f := newWallFixture(t, 40)
	f.reg.ReplaceStatic([]rooms.FileRoom{{Code: "lan-team"}})
	if _, ok := f.reg.WallPop("assembly-2026"); !ok {
		t.Fatal("a -rooms-file re-read ended the party wall")
	}
}

// IsWall names the PoP a wall belongs to (the PoP side's local wall and the
// home's vouch router both ask).
func TestIsWall(t *testing.T) {
	f := newWallFixture(t, 40)
	if pop, ok := f.reg.WallPop("Assembly-2026"); !ok || pop != wallPop {
		t.Errorf("WallPop = %q, %v", pop, ok)
	}
	if err := f.reg.UpsertStatic(StaticRoom{Code: "plain-room"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.reg.WallPop("plain-room"); ok {
		t.Error("a plain room reads as a wall")
	}
	if _, ok := f.reg.WallPop("missing"); ok {
		t.Error("a missing room reads as a wall")
	}
}

// A mint that is refused (here the fleet is at -max-rooms) leaves the
// broadcast on the wall: the wall tile goes only once the new room exists.
func TestARefusedMintKeepsTheBroadcastOnTheWall(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.MaxRooms = 1 })
	if err := f.reg.UpsertStatic(StaticRoom{Code: "assembly-2026", Pop: wallPop, MaxBroadcasts: 40, MaxParticipants: 500}); err != nil {
		t.Fatal(err)
	}
	f.mint(t, "ZZZZZ2") // the one dynamic room the fleet allows
	id := "ABCDEF"
	f.bc.set(id, BroadcastState{Live: true})
	if err := f.reg.AttachVouched("assembly-2026", wallPop, id, ""); err != nil {
		t.Fatal(err)
	}
	_, err := f.reg.Mint(context.Background(), MintRequest{BroadcastID: id, ResumeToken: f.tokens.MintResume(id)})
	if !errors.Is(err, ErrMaxRooms) {
		t.Fatalf("Mint = %v, want ErrMaxRooms", err)
	}
	if !slices.ContainsFunc(f.reg.Attachments("assembly-2026"), func(a rooms.Attachment) bool { return a.BroadcastID == id }) {
		t.Fatal("a refused mint took the broadcast off the wall")
	}
}

// The wall's owner (the PoP's popclient) hears when a broadcast leaves its
// wall by its broadcaster's choice, a mint or an attach elsewhere, so it
// can stop vouching for it home and stop re-attaching it on a resync.
func TestLeavingTheWallIsReportedToItsOwner(t *testing.T) {
	f := newWallFixture(t, 40)
	var left []string
	f.reg.OnWallLeave(func(code, id string) { left = append(left, code+"/"+id) })
	if err := f.reg.UpsertStatic(StaticRoom{Code: "lan-team"}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"ABCDEF", "ABCDEG"} {
		f.bc.set(id, BroadcastState{Live: true})
		if err := f.reg.AttachVouched("assembly-2026", wallPop, id, ""); err != nil {
			t.Fatal(err)
		}
	}
	f.mint(t, "ABCDEF")
	p, c := f.join(t, "lan-team", "tuhis", Grants{AttachOK: true}, nil)
	c.nextState(t)
	p.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandAttach, BroadcastID: "ABCDEG", ResumeToken: f.tokens.MintResume("ABCDEG")})
	c.nextEvent(t, wire.RoomEventAttachmentAdded)
	if want := []string{"assembly-2026/ABCDEF", "assembly-2026/ABCDEG"}; !slices.Equal(left, want) {
		t.Fatalf("wall leaves = %v, want %v", left, want)
	}
}
