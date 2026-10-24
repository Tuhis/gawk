package popclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/internal/roomsrv"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

const wallCode = "assembly-2026"

var wallConfig = poplink.Config{PopName: "assembly", DisplayName: "Assembly 2026", WallCode: "Assembly-2026", HomeRooms: true}

type wallPoP struct {
	c     *Client
	l     *homeLink
	f     *fakeHome
	r     *hub.Registry
	rooms *roomsrv.Registry
}

func newWallPoP(t *testing.T) *wallPoP {
	t.Helper()
	f := newFakeHome(t)
	r, srv := relayParts(t)
	rooms := roomsrv.NewRegistry(roomsrv.Options{Broadcasts: srv.RoomBroadcasts(), Obfuscate: r.ObfuscateID,
		Log: discard(), MaxRooms: 8, EmptyGrace: time.Hour})
	srv.SetRooms(rooms)
	o := testOptions(t, f)
	o.Registry, o.Server = r, srv
	c, l := linkUp(t, f, o, wallConfig)
	p := &wallPoP{c: c, l: l, f: f, r: r, rooms: rooms}
	waitUntil(t, "the local wall", func() bool { return rooms.Has(wallCode) })
	return p
}

func waitUntil(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitFor)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("never: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (p *wallPoP) localLabels() []string {
	var out []string
	for _, a := range p.rooms.Attachments(wallCode) {
		out = append(out, a.BroadcastID+"="+a.Label)
	}
	slices.Sort(out)
	return out
}

func (p *wallPoP) waitLocal(t *testing.T, want ...string) {
	t.Helper()
	slices.Sort(want)
	deadline := time.Now().Add(waitFor)
	for {
		got := p.localLabels()
		if slices.Equal(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("local wall %v, want %v", got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func expectWall(t *testing.T, l *homeLink, op, id, label string) {
	t.Helper()
	w := expect[*poplink.Wall](t, l)
	if w.Op != op || w.ID != id || w.Label != label {
		t.Fatalf("wall record %+v, want %s %s %q", w, op, id, label)
	}
}

// docs/76 §6.10: linked, a party broadcast is vouched for home with a wall
// record and shown on the local read-only wall too; Hide and Show follow
// both; a broadcast that ends comes off both.
func TestWallSyncsHomeAndLocal(t *testing.T) {
	p := newWallPoP(t)
	if info, ok := p.rooms.Lookup(wallCode); !ok || info.Code != wallCode {
		t.Fatalf("local wall %+v %v", info, ok)
	}
	id, pub, err := p.r.StartPublish("")
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	p.c.wall.PublisherLive(httptest.NewRequest("CONNECT", "https://pop/publish?name=tuhis", nil), id)
	expectWall(t, p.l, poplink.WallAttach, id, "tuhis")
	p.waitLocal(t, id+"=tuhis")

	if !p.c.wall.WallCommand(wallCode, wire.RoomCommand{Kind: wire.RoomCommandDetach, BroadcastID: id}) {
		t.Fatal("Hide not taken")
	}
	expectWall(t, p.l, poplink.WallDetach, id, "tuhis")
	p.waitLocal(t)
	if !p.c.wall.WallCommand(wallCode, wire.RoomCommand{Kind: wire.RoomCommandAttach, BroadcastID: id, Label: " renamed "}) {
		t.Fatal("Show not taken")
	}
	expectWall(t, p.l, poplink.WallAttach, id, "renamed")
	p.waitLocal(t, id+"=renamed")
	// An unknown command kind and a malformed ID are not the wall's.
	if p.c.wall.WallCommand(wallCode, wire.RoomCommand{Kind: 99, BroadcastID: id}) {
		t.Fatal("an unknown command kind was taken")
	}
	if p.c.wall.WallCommand(wallCode, wire.RoomCommand{Kind: wire.RoomCommandDetach, BroadcastID: "nope!"}) {
		t.Fatal("a malformed ID was taken")
	}

	// The broadcast ends: the sweep takes it off home's wall and the local
	// one, and the console row goes.
	pub.Close()
	p.r.EndBroadcast(id)
	expectWall(t, p.l, poplink.WallDetach, id, "")
	p.waitLocal(t)
	if wall, label := p.c.WallRow(id); wall != "" || label != "" {
		t.Fatalf("WallRow after the end = %q %q", wall, label)
	}
}

// A broadcaster that joins a room keeps its wall tile (docs/76 item 34):
// a room minted here leaves it on the local wall, sends home no detach,
// and a relink's resync still attaches it.
func TestARoomDoesNotTakeABroadcastOffTheWall(t *testing.T) {
	p := newWallPoP(t)
	id, pub, err := p.r.StartPublish("")
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	p.c.wall.PublisherLive(httptest.NewRequest("CONNECT", "https://pop/publish", nil), id)
	expectWall(t, p.l, poplink.WallAttach, id, "Stream 1")
	p.waitLocal(t, id+"=Stream 1")

	p.rooms.SetTokens(anyResume{})
	res, err := p.rooms.Mint(context.Background(), roomsrv.MintRequest{BroadcastID: id})
	if err != nil {
		t.Fatal(err)
	}
	none[*poplink.Wall](t, p.l, 50*time.Millisecond)
	p.waitLocal(t, id+"=Stream 1")
	if len(p.rooms.Attachments(res.Code)) != 1 {
		t.Fatalf("minted room %v", p.rooms.Attachments(res.Code))
	}
	if wall, _ := p.c.WallRow(id); wall != "on" {
		t.Fatalf("WallRow after the mint = %q", wall)
	}

	p.l.close(wire.CloseCodeServerDraining, "drain")
	l2 := p.f.nextLink(t)
	l2.send(wallConfig)
	expectWall(t, l2, poplink.WallAttach, id, "Stream 1")
}

// anyResume takes every resume token (the test mints without one).
type anyResume struct{}

func (anyResume) MintCreator(code string) []byte               { return []byte(code) }
func (anyResume) VerifyCreator(code string, token []byte) bool { return string(token) == code }
func (anyResume) VerifyResume(string, []byte) bool             { return true }

// A changed wall code rebuilds the local wall; a home without rooms ends
// it and the 0x18 carries no wall code.
func TestWallConfigChanges(t *testing.T) {
	p := newWallPoP(t)
	id, pub, err := p.r.StartPublish("")
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()
	p.c.wall.PublisherLive(httptest.NewRequest("CONNECT", "https://pop/publish?name=a", nil), id)
	expectWall(t, p.l, poplink.WallAttach, id, "a")

	cfg := wallConfig
	cfg.WallCode = "Party-2027"
	p.l.send(cfg)
	waitUntil(t, "the new local wall", func() bool { return p.rooms.Has("party-2027") })
	waitUntil(t, "the old local wall to end", func() bool { return !p.rooms.Has(wallCode) })
	// The resync attaches the broadcast to the new wall.
	expectWall(t, p.l, poplink.WallAttach, id, "a")
	waitUntil(t, "the tile on the new wall", func() bool { return len(p.rooms.Attachments("party-2027")) == 1 })
	if got := p.c.LocalRelayInfo(id); got.WallCode != "Party-2027" || !got.OnWall {
		t.Fatalf("0x18 on the new wall = %+v", got)
	}

	cfg.HomeRooms = false
	p.l.send(cfg)
	waitUntil(t, "the wall to end", func() bool { return !p.rooms.Has("party-2027") })
	if got := p.c.LocalRelayInfo(id); got.WallCode != "" || got.OnWall {
		t.Fatalf("0x18 without a wall = %+v", got)
	}
	if wall, label := p.c.WallRow(id); wall != "" || label != "a" {
		t.Fatalf("WallRow without a wall = %q %q", wall, label)
	}
	// With no wall, Hide is not taken.
	if p.c.wall.WallCommand("party-2027", wire.RoomCommand{Kind: wire.RoomCommandDetach, BroadcastID: id}) {
		t.Fatal("Hide taken with no wall")
	}
}

// OpenRoom opens a room stream on the link with the RoomOpen first;
// unlinked it is ErrNotLinked.
func TestWallOpenRoom(t *testing.T) {
	p := newWallPoP(t)
	accepted := make(chan any, 1)
	go func() {
		s, err := p.l.sess.AcceptStream(t.Context())
		if err != nil {
			accepted <- err
			return
		}
		rec, err := poplink.Read(s)
		if err != nil {
			accepted <- err
			return
		}
		accepted <- rec
	}()
	s, err := p.c.wall.OpenRoom(t.Context(), "/room/lan-team", "x=1")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	select {
	case got := <-accepted:
		ro, ok := got.(*poplink.RoomOpen)
		if !ok || ro.Path != "/room/lan-team" || ro.RawQuery != "x=1" {
			t.Fatalf("home read %+v", got)
		}
	case <-time.After(waitFor):
		t.Fatal("home saw no room stream")
	}
	// The link goes: OpenRoom fails.
	p.f.route(func(dialRecord) (Session, *http.Response, error) { return nil, nil, errors.New("down") })
	p.l.close(4999, "")
	waitState(t, p.c, headerIs(HeaderOffline))
	if _, err := p.c.wall.OpenRoom(t.Context(), "/room/x", ""); !errors.Is(err, ErrNotLinked) {
		t.Fatalf("OpenRoom unlinked = %v", err)
	}
}

// A link session whose streams cannot open makes OpenRoom fail with that
// error.
func TestWallOpenRoomStreamFails(t *testing.T) {
	c := New(Options{Log: discard()})
	pop, _ := poplink.Pipe(popAddr, homeAddr)
	_ = pop.CloseWithError(0, "gone")
	c.ctrl = pop
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := c.wall.OpenRoom(ctx, "/room/x", ""); err == nil {
		t.Fatal("OpenRoom on a closed session succeeded")
	}
}
