package roomsrv

import (
	"errors"
	"testing"

	"github.com/Tuhis/gawk/gawk-server/rooms"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// R74 (docs/76 item 33): a static room may carry its own participant cap,
// as it already carries its own broadcast cap; zero keeps the fleet's.
// A party wall needs more than the R42 default of 50.
func TestStaticRoomParticipantCapOverridesTheFleetDefault(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.MaxParticipants = 2 })
	if err := f.reg.UpsertStatic(StaticRoom{Code: "party-wall", Pop: "assembly-2026", MaxParticipants: 3}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := f.reg.CheckJoin("party-wall", "", "", false); err != nil {
			t.Fatalf("CheckJoin #%d under the room's cap of 3: %v", i+1, err)
		}
		f.join(t, "party-wall", "guest", Grants{}, nil)
	}
	if _, err := f.reg.CheckJoin("party-wall", "", "", false); !errors.Is(err, ErrFull) {
		t.Errorf("CheckJoin past the room's cap: %v, want ErrFull", err)
	}
	if _, err := f.reg.Join("party-wall", wire.RoomHello{Protocol: 1}, Grants{}, nil, newFakeConn()); !errors.Is(err, ErrFull) {
		t.Errorf("Join past the room's cap: %v, want ErrFull", err)
	}

	// Lowered on update: applies to the next join.
	if err := f.reg.UpsertStatic(StaticRoom{Code: "party-wall", Pop: "assembly-2026", MaxParticipants: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.reg.CheckJoin("party-wall", "", "", false); !errors.Is(err, ErrFull) {
		t.Errorf("CheckJoin after lowering the cap: %v, want ErrFull", err)
	}

	// Zero keeps the fleet default.
	if err := f.reg.UpsertStatic(StaticRoom{Code: "plain-room"}); err != nil {
		t.Fatal(err)
	}
	f.join(t, "plain-room", "a", Grants{}, nil)
	f.join(t, "plain-room", "b", Grants{}, nil)
	if _, err := f.reg.CheckJoin("plain-room", "", "", false); !errors.Is(err, ErrFull) {
		t.Errorf("CheckJoin past the fleet cap: %v, want ErrFull", err)
	}
}

// The -rooms-file source carries the cap too.
func TestReplaceStaticCarriesMaxParticipants(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.MaxParticipants = 1 })
	f.reg.ReplaceStatic([]rooms.FileRoom{{Code: "lan-wall", MaxParticipants: 2}})
	f.join(t, "lan-wall", "a", Grants{}, nil)
	if _, err := f.reg.CheckJoin("lan-wall", "", "", false); err != nil {
		t.Errorf("second join under the file's cap of 2: %v", err)
	}
}
