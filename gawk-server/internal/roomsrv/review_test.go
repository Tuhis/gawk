package roomsrv

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/rooms"
)

// PR #302 review, minor 6: the empty-grace callback must end the room only
// for ITS arm. Sequence: Leave arms T1 → T1 fires but its callback has to
// wait for the lock → Join stops T1 (too late) → Leave arms T2 → T1's
// callback runs. The old check ("some timer is armed") ended the room at
// once instead of after T2's fresh grace — the reload-at-expiry case D7 is
// about. The fake AfterFunc lets the test run T1's callback exactly there.
func TestStaleEmptyGraceCallbackYieldsToTheFreshArm(t *testing.T) {
	var (
		mu        sync.Mutex
		callbacks []func()
	)
	f := newFixture(t, func(o *Options) {
		o.AfterFunc = func(_ time.Duration, fn func()) *time.Timer {
			mu.Lock()
			callbacks = append(callbacks, fn)
			mu.Unlock()
			return time.NewTimer(time.Hour) // never fires on its own
		}
	})
	res := f.mint(t, "ABCDEF") // arms T0 (mint starts the grace)
	p, c := f.join(t, res.Code, "a", Grants{}, nil)
	c.nextState(t)
	p.Leave() // arms T1
	p2, c2 := f.join(t, res.Code, "a", Grants{}, nil)
	c2.nextState(t)
	p2.Leave() // arms T2
	mu.Lock()
	if len(callbacks) != 3 {
		t.Fatalf("%d arms, want 3 (mint, leave, leave)", len(callbacks))
	}
	t1 := callbacks[1]
	mu.Unlock()
	// A participant is back; T1's late callback must not end the room.
	p3, c3 := f.join(t, res.Code, "a", Grants{}, nil)
	c3.nextState(t)
	p3.Leave() // arms T3; T2 was stopped
	t1()
	if !f.reg.Has(res.Code) {
		t.Fatal("a stale grace callback ended the room")
	}
	// The live arm does end it.
	mu.Lock()
	t3 := callbacks[len(callbacks)-1]
	mu.Unlock()
	t3()
	if f.reg.Has(res.Code) {
		t.Fatal("the current grace arm did not end the room")
	}
}

// PR #302 review, minor 10: participant IDs wrap after 65535 joins; a
// newcomer must never take an ID a live participant holds.
func TestParticipantIDsSkipLiveEntriesOnWrap(t *testing.T) {
	f := newFixture(t, nil)
	res := f.mint(t, "ABCDEF")
	a, ac := f.join(t, res.Code, "a", Grants{}, nil)
	ac.nextState(t)
	// Fast-forward the counter to the edge of the space.
	f.reg.mu.Lock()
	rm := f.reg.rooms[res.Code]
	rm.nextPID = 65535
	f.reg.mu.Unlock()
	b, bc := f.join(t, res.Code, "b", Grants{}, nil)
	bc.nextState(t)
	if b.ID() != 65535 {
		t.Fatalf("b = %d, want 65535", b.ID())
	}
	// Wrapped: 0 is never an ID and a's ID (1) is live, so c gets 2.
	c, cc := f.join(t, res.Code, "c", Grants{}, nil)
	cc.nextState(t)
	if c.ID() != 2 || a.ID() != 1 {
		t.Fatalf("c = %d (a = %d), want 2 with a still 1", c.ID(), a.ID())
	}
	f.reg.mu.Lock()
	if rm.participants[1] != a {
		t.Fatal("a's entry was overwritten on wrap")
	}
	f.reg.mu.Unlock()
}

// PR #302 review, finding 4 (D16): the errors the transport logs at Warn
// must not carry a room code.
func TestErrorsCarryNoRoomCode(t *testing.T) {
	f := newFixture(t, nil)
	res := f.mint(t, "ABCDEF")
	_, err := f.reg.Mint(context.Background(), MintRequest{BroadcastID: "ABCDEF", ResumeToken: f.tokens.MintResume("ABCDEF")})
	if !errors.Is(err, ErrAlreadyAttached) || strings.Contains(strings.ToLower(err.Error()), res.Code) {
		t.Fatalf("second mint error leaks the room code: %v", err)
	}
	err = f.reg.UpsertStatic(StaticRoom{Code: res.Code})
	if err == nil || strings.Contains(strings.ToLower(err.Error()), res.Code) {
		t.Fatalf("static-over-dynamic error leaks the room code: %v", err)
	}
}

// PR #302 review, finding 3: the attach secret is resolved per join through
// the AttachSecret seam, so a rotation is honoured by the next join with
// no registry change; an unreadable Secret fails closed.
func TestAttachSecretIsResolvedAtJoinTime(t *testing.T) {
	var (
		mu     sync.Mutex
		secret = "v1"
		fail   bool
	)
	f := newFixture(t, func(o *Options) {
		o.AttachSecret = func(code string) (string, bool, error) {
			mu.Lock()
			defer mu.Unlock()
			if code != "tuhisroom" {
				return "", false, nil
			}
			if fail {
				return "", true, errors.New("secret get: connection refused")
			}
			return secret, true, nil
		}
	})
	if err := f.reg.UpsertStatic(StaticRoom{Code: "TuhisRoom", AttachSecret: "stale-inline"}); err != nil {
		t.Fatal(err)
	}
	if g, err := f.reg.CheckJoin("TuhisRoom", "", "v1", true); err != nil || !g.AttachOK {
		t.Fatalf("v1: %+v, %v", g, err)
	}
	if _, err := f.reg.CheckJoin("TuhisRoom", "", "stale-inline", true); !errors.Is(err, ErrForbidden) {
		t.Fatalf("the inline secret was honoured over the resolved one: %v", err)
	}
	mu.Lock()
	secret = "v2"
	mu.Unlock()
	if _, err := f.reg.CheckJoin("TuhisRoom", "", "v1", true); !errors.Is(err, ErrForbidden) {
		t.Fatalf("rotated-away secret still admits: %v", err)
	}
	if g, err := f.reg.CheckJoin("TuhisRoom", "", "v2", true); err != nil || !g.AttachOK {
		t.Fatalf("v2 after rotation: %+v, %v", g, err)
	}
	// A viewer presenting no secret never triggers the resolver's failure.
	mu.Lock()
	fail = true
	mu.Unlock()
	if g, err := f.reg.CheckJoin("TuhisRoom", "", "", false); err != nil || g.AttachOK {
		t.Fatalf("viewer join: %+v, %v", g, err)
	}
	if _, err := f.reg.CheckJoin("TuhisRoom", "", "v2", true); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("unreadable Secret must fail closed: %v", err)
	}
	// A room the resolver does not know keeps its inline secret.
	if err := f.reg.UpsertStatic(StaticRoom{Code: "other", AttachSecret: "inline"}); err != nil {
		t.Fatal(err)
	}
	if g, err := f.reg.CheckJoin("other", "", "inline", true); err != nil || !g.AttachOK {
		t.Fatalf("inline fallback: %+v, %v", g, err)
	}
}

// PR #302 review, minor 7: a Reserve whose code turns out to be taken
// locally is handed back through Unreserve.
func TestMintUnreservesOnTheLocalRace(t *testing.T) {
	var (
		mu         sync.Mutex
		unreserved []string
		raced      bool
	)
	f := newFixture(t, func(o *Options) {
		o.Unreserve = func(_ context.Context, code string) {
			mu.Lock()
			defer mu.Unlock()
			unreserved = append(unreserved, code)
		}
	})
	f.reg.opts.Reserve = func(_ context.Context, cr *rooms.Room) error {
		// The first reservation is beaten by an adoption of the same code
		// landing between Mint's pre-check and its insert.
		mu.Lock()
		first := !raced
		raced = true
		mu.Unlock()
		if first {
			adopt := &rooms.Room{Spec: rooms.RoomSpec{Kind: rooms.KindDynamic}}
			adopt.Name = cr.Name
			f.reg.AdoptDynamic(adopt)
		}
		return nil
	}
	res := f.mint(t, "ABCDEF")
	mu.Lock()
	defer mu.Unlock()
	if len(unreserved) != 1 || unreserved[0] == res.Code {
		t.Fatalf("unreserved = %v (minted %s): want exactly the lost code", unreserved, res.Code)
	}
}
