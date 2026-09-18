package roomsrv

import (
	"sync"
	"testing"

	"github.com/Tuhis/gawk/gawk-server/events"
	"github.com/Tuhis/gawk/gawk-server/internal/eventbus"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// busRecorder collects what the registry would publish. It stands in for the
// publisher, whose only requirement of a hook is that it does not block.
type busRecorder struct {
	mu  sync.Mutex
	evs []eventbus.Event
}

func (b *busRecorder) hook(ev eventbus.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.evs = append(b.evs, ev)
}

func (b *busRecorder) types() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]string, len(b.evs))
	for i, ev := range b.evs {
		out[i] = ev.Type
	}
	return out
}

// only returns the events of one type.
func (b *busRecorder) only(typ string) []eventbus.Event {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []eventbus.Event
	for _, ev := range b.evs {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

func busFixture(t *testing.T) (*fixture, *busRecorder) {
	t.Helper()
	rec := &busRecorder{}
	f := newFixture(t, func(o *Options) {
		o.OnEvent = rec.hook
		// A fixed obfuscator: the assertions are about WHICH identity travels,
		// not about the HMAC.
		o.Obfuscate = func(s string) string { return "key-" + s }
	})
	return f, rec
}

// TestRoomLifecycleOnTheBus walks a room's whole life and checks each
// transition produces exactly one event with the documented fields — and that
// the subject is the HMAC'd key while the raw code stays in the body, the
// internal/public split docs/51 D2 rests on.
func TestRoomLifecycleOnTheBus(t *testing.T) {
	f, rec := busFixture(t)
	res := f.mint(t, "ABCDEF")

	opened := rec.only(events.TypeRoomOpened)
	if len(opened) != 1 {
		t.Fatalf("mint produced %d room.opened events, want 1", len(opened))
	}
	if opened[0].Key != "key-"+res.Code {
		t.Errorf("subject = %q, want the obfuscated key", opened[0].Key)
	}
	data := opened[0].Data.(events.RoomOpenedData)
	if data.Code != res.Code || data.Kind != "dynamic" || data.DisplayCode != res.Display {
		t.Errorf("room.opened data = %+v", data)
	}

	// Mint attaches the broadcast, so the attach is already on the bus.
	att := rec.only(events.TypeRoomAttached)
	if len(att) != 1 {
		t.Fatalf("got %d room.attached, want 1", len(att))
	}
	if d := att[0].Data.(events.RoomAttachmentData); d.BroadcastID != "ABCDEF" || d.Label != "pc" {
		t.Errorf("room.attached data = %+v", d)
	}

	p, _ := f.join(t, res.Code, "tuhis", Grants{Creator: true, AttachOK: true}, res.CreatorToken)
	joined := rec.only(events.TypeRoomParticipantJoined)
	if len(joined) != 1 {
		t.Fatalf("got %d room.participant_joined, want 1", len(joined))
	}
	jd := joined[0].Data.(events.RoomParticipantData)
	if jd.ParticipantID != int(p.ID()) || jd.Nickname != "tuhis" || jd.Rejoin {
		t.Errorf("participant_joined data = %+v", jd)
	}
	if jd.ClientKind != events.ClientWebViewer {
		t.Errorf("clientKind = %q, want the wire kind's name", jd.ClientKind)
	}

	p.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandSetNickname, Nickname: "renamed"})
	upd := rec.only(events.TypeRoomParticipantUpdate)
	if len(upd) == 0 {
		t.Fatal("a rename produced no room.participant_updated")
	}
	if d := upd[len(upd)-1].Data.(events.RoomParticipantData); d.Nickname != "renamed" {
		t.Errorf("participant_updated data = %+v", d)
	}

	f.reg.EndRoom(res.Code, wire.RoomEndReasonCreator)
	closed := rec.only(events.TypeRoomClosed)
	if len(closed) != 1 {
		t.Fatalf("got %d room.closed, want 1: %v", len(closed), rec.types())
	}
	if d := closed[0].Data.(events.RoomClosedData); d.Reason != events.ReasonCreator {
		t.Errorf("room.closed reason = %q, want creator", d.Reason)
	}
}

// TestDetachIsPublished: a broadcast leaving the room is its own event, not an
// attachment update with live:false.
func TestDetachIsPublished(t *testing.T) {
	f, rec := busFixture(t)
	res := f.mint(t, "ABCDEF")
	f.bc.del("ABCDEF")
	f.reg.BroadcastExpired("ABCDEF")

	det := rec.only(events.TypeRoomDetached)
	if len(det) != 1 {
		t.Fatalf("got %d room.detached, want 1: %v", len(det), rec.types())
	}
	if d := det[0].Data.(events.RoomAttachmentData); d.BroadcastID != "ABCDEF" || d.Code != res.Code {
		t.Errorf("room.detached data = %+v", d)
	}
}

// TestRefreshPublishesAttachmentUpdates: the registry's own refresh is the one
// internal poll (docs/51 §1), and what it notices becomes the coalesced delta.
func TestRefreshPublishesAttachmentUpdates(t *testing.T) {
	f, rec := busFixture(t)
	f.mint(t, "ABCDEF")
	f.bc.set("ABCDEF", BroadcastState{Live: true, Viewers: 12})
	f.reg.Refresh()

	upd := rec.only(events.TypeRoomAttachmentUpdated)
	if len(upd) == 0 {
		t.Fatalf("refresh published nothing: %v", rec.types())
	}
	d := upd[len(upd)-1].Data.(events.RoomAttachmentUpdatedData)
	if !d.Live || d.Viewers != 12 {
		t.Errorf("attachment_updated data = %+v", d)
	}
}

// TestReleaseHomeSaysTheRoomMoved is the publishing half of docs/51 D9: when a
// pod loses the home lease the room is MOVING, so its participants leave with
// reason home_moved and no room.closed is published at all. A consumer that
// saw a close here would think a live room had ended.
func TestReleaseHomeSaysTheRoomMoved(t *testing.T) {
	f, rec := busFixture(t)
	res := f.mint(t, "ABCDEF")
	p, _ := f.join(t, res.Code, "tuhis", Grants{Creator: true, AttachOK: true}, res.CreatorToken)

	f.reg.ReleaseHome(res.Code)
	left := rec.only(events.TypeRoomParticipantLeft)
	if len(left) != 1 {
		t.Fatalf("got %d room.participant_left, want one per participant", len(left))
	}
	d := left[0].Data.(events.RoomParticipantLeftData)
	if d.Reason != events.ReasonHomeMoved || d.ParticipantID != int(p.ID()) || d.Nickname != "tuhis" {
		t.Errorf("participant_left data = %+v", d)
	}

	f.reg.EndRoom(res.Code, wire.RoomEndReasonOperator)
	if closed := rec.only(events.TypeRoomClosed); len(closed) != 0 {
		t.Errorf("a re-homed room published %d room.closed events, want none", len(closed))
	}
}

// TestNoHookNoWork: with the bus off the registry must behave exactly as it
// did before R50 — the acceptance criterion the whole milestone rests on.
func TestNoHookNoWork(t *testing.T) {
	f := newFixture(t, nil) // no OnEvent
	res := f.mint(t, "ABCDEF")
	f.join(t, res.Code, "tuhis", Grants{Creator: true, AttachOK: true}, res.CreatorToken)
	f.reg.Refresh()
	f.reg.ReleaseHome(res.Code)
	f.reg.EndRoom(res.Code, wire.RoomEndReasonCreator)
}
