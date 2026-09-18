package roomsrv

import (
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Tuhis/gawk/gawk-server/events"
	"github.com/Tuhis/gawk/gawk-server/internal/eventbus"
	"github.com/Tuhis/gawk/gawk-server/rooms"
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
	if data.RoomCode != res.Code || data.Kind != "dynamic" || data.DisplayCode != res.Display {
		t.Errorf("room.opened data = %+v", data)
	}

	// Mint attaches the broadcast, so the attach is already on the bus.
	att := rec.only(events.TypeRoomAttached)
	if len(att) != 1 {
		t.Fatalf("got %d room.attached, want 1", len(att))
	}
	if d := att[0].Data.(events.RoomAttachedData); d.BroadcastID != "ABCDEF" || d.Label != "pc" {
		t.Errorf("room.attached data = %+v", d)
	}

	p, _ := f.join(t, res.Code, "tuhis", Grants{Creator: true, AttachOK: true}, res.CreatorToken)
	joined := rec.only(events.TypeRoomParticipantJoined)
	if len(joined) != 1 {
		t.Fatalf("got %d room.participant_joined, want 1", len(joined))
	}
	jd := joined[0].Data.(events.RoomParticipantJoinedData)
	if jd.ParticipantID != int(p.ID()) || jd.Nickname != "tuhis" || jd.Rejoin {
		t.Errorf("participant_joined data = %+v", jd)
	}
	if jd.ClientKind != events.ClientKindWebViewer {
		t.Errorf("clientKind = %q, want the wire kind's name", jd.ClientKind)
	}

	p.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandSetNickname, Nickname: "renamed"})
	upd := rec.only(events.TypeRoomParticipantUpdated)
	if len(upd) == 0 {
		t.Fatal("a rename produced no room.participant_updated")
	}
	if d := upd[len(upd)-1].Data.(events.RoomParticipantUpdatedData); d.Nickname != "renamed" {
		t.Errorf("participant_updated data = %+v", d)
	}

	f.reg.EndRoom(res.Code, wire.RoomEndReasonCreator)
	closed := rec.only(events.TypeRoomClosed)
	if len(closed) != 1 {
		t.Fatalf("got %d room.closed, want 1: %v", len(closed), rec.types())
	}
	if d := closed[0].Data.(events.RoomClosedData); d.Reason != events.RoomClosedCreator {
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
	if d := det[0].Data.(events.RoomDetachedData); d.BroadcastID != "ABCDEF" || d.RoomCode != res.Code {
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

// TestLeaveSaysWhy walks the four ways a participant's session ends. Only the
// first is somebody leaving; the other three are the room happening to them,
// and a consumer that announced "tuhis left" for a pod rollout would be lying
// (docs/51 D9).
func TestLeaveSaysWhy(t *testing.T) {
	t.Run("on their own", func(t *testing.T) {
		f, rec := busFixture(t)
		res := f.mint(t, "ABCDEF")
		p, _ := f.join(t, res.Code, "tuhis", Grants{Creator: true, AttachOK: true}, res.CreatorToken)

		p.Leave()

		left := rec.only(events.TypeRoomParticipantLeft)
		if len(left) != 1 {
			t.Fatalf("got %d room.participant_left, want 1: %v", len(left), rec.types())
		}
		d := left[0].Data.(events.RoomParticipantLeftData)
		if d.Reason != events.ParticipantLeft {
			t.Errorf("reason = %q, want %q", d.Reason, events.ParticipantLeft)
		}
		// The record is gone from the roster by now; the event still carries it.
		if d.ParticipantID != int(p.ID()) || d.Nickname != "tuhis" {
			t.Errorf("participant_left data = %+v", d)
		}
	})

	t.Run("the room ended under them", func(t *testing.T) {
		f, rec := busFixture(t)
		res := f.mint(t, "ABCDEF")
		p, _ := f.join(t, res.Code, "tuhis", Grants{Creator: true, AttachOK: true}, res.CreatorToken)

		f.reg.EndRoom(res.Code, wire.RoomEndReasonCreator)
		p.Leave() // what the transport does once the session is closed

		left := rec.only(events.TypeRoomParticipantLeft)
		if len(left) != 1 {
			t.Fatalf("got %d room.participant_left, want 1", len(left))
		}
		if d := left[0].Data.(events.RoomParticipantLeftData); d.Reason != events.ParticipantLeftRoomEnded {
			t.Errorf("reason = %q, want %q — a room.closed precedes it", d.Reason, events.ParticipantLeftRoomEnded)
		}
	})

	t.Run("the room moved", func(t *testing.T) {
		f, rec := busFixture(t)
		res := f.mint(t, "ABCDEF")
		p, _ := f.join(t, res.Code, "tuhis", Grants{Creator: true, AttachOK: true}, res.CreatorToken)

		// What the transport does on losing the home lease: mark, close the
		// sessions, end the room here.
		f.reg.ReleaseHome(res.Code)
		p.Leave()
		f.reg.EndRoom(res.Code, wire.RoomEndReasonOperator)

		left := rec.only(events.TypeRoomParticipantLeft)
		if len(left) != 1 {
			t.Fatalf("got %d room.participant_left, want 1", len(left))
		}
		if d := left[0].Data.(events.RoomParticipantLeftData); d.Reason != events.ParticipantLeftHomeMoved {
			t.Errorf("reason = %q, want %q — nobody left, the room moved", d.Reason, events.ParticipantLeftHomeMoved)
		}
		// And a moved room did not close: a consumer seeing a close here would
		// think a live room had ended.
		if closed := rec.only(events.TypeRoomClosed); len(closed) != 0 {
			t.Errorf("a re-homed room published %d room.closed events, want none", len(closed))
		}
	})

	t.Run("evicted", func(t *testing.T) {
		f, rec := busFixture(t)
		res := f.mint(t, "ABCDEF")
		f.join(t, res.Code, "tuhis", Grants{Creator: true, AttachOK: true}, res.CreatorToken)

		// A participant with no writer draining it, so the overflow is the
		// test's and not a race with the drain: one slot, two records.
		f.reg.mu.Lock()
		rm := f.reg.rooms[res.Code]
		evicted := &Participant{
			reg: f.reg, room: rm, conn: newFakeConn(), id: 99, nick: "stalled",
			outbox: make(chan []byte, 1), closed: make(chan struct{}),
		}
		evicted.enqueueLocked([]byte("first"))
		evicted.enqueueLocked([]byte("overflows"))
		got := evicted.leaveReason
		f.reg.busParticipantLeftLocked(rm, evicted)
		f.reg.mu.Unlock()

		if got != events.ParticipantLeftTimeout {
			t.Fatalf("an evicted session recorded %q, want %q", got, events.ParticipantLeftTimeout)
		}
		left := rec.only(events.TypeRoomParticipantLeft)
		if len(left) != 1 {
			t.Fatalf("got %d room.participant_left, want 1", len(left))
		}
		d := left[0].Data.(events.RoomParticipantLeftData)
		if d.Reason != events.ParticipantLeftTimeout || d.Nickname != "stalled" {
			t.Errorf("participant_left data = %+v", d)
		}
	})
}

// TestAdoptionAnnouncesTheMove is the other half of docs/51 D9, and the half a
// consumer can actually rely on: the pod that ADOPTS a room publishes
// room.home_changed. The pod that lost it may have been deleted without
// publishing anything at all.
func TestAdoptionAnnouncesTheMove(t *testing.T) {
	f, rec := busFixture(t)
	cr := &rooms.Room{
		ObjectMeta: metav1.ObjectMeta{Name: "pf4tzn"},
		Spec:       rooms.RoomSpec{Kind: rooms.KindDynamic},
		Status: rooms.RoomStatus{
			Lease: &rooms.Lease{Holder: "relay-1", Generation: 3},
		},
	}
	if !f.reg.AdoptDynamic(cr) {
		t.Fatal("AdoptDynamic refused the room")
	}

	moved := rec.only(events.TypeRoomHomeChanged)
	if len(moved) != 1 {
		t.Fatalf("got %d room.home_changed, want 1: %v", len(moved), rec.types())
	}
	d := moved[0].Data.(events.RoomHomeChangedData)
	if d.RoomCode != "pf4tzn" || d.Kind != rooms.KindDynamic {
		t.Errorf("home_changed data = %+v", d)
	}
	if d.PreviousPod != "relay-1" {
		t.Errorf("previousPod = %q, want the lease holder the record named", d.PreviousPod)
	}
	// An adoption is not a birth: room.opened would tell a consumer a new room
	// appeared, and none did.
	if opened := rec.only(events.TypeRoomOpened); len(opened) != 0 {
		t.Errorf("an adoption published %d room.opened events, want none", len(opened))
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
