package popclient

import (
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/poplink"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

func linkedWithWall(t *testing.T, homeRooms bool) *Client {
	t.Helper()
	c := New(Options{Cfg: config.Config{ServerName: "relay"}})
	cfg := &poplink.Config{PopName: "assembly-2026", DisplayName: "Assembly 2026", WallCode: "Assembly-2026", HomeRooms: homeRooms}
	c.setState(func(s *LinkState) { s.Header, s.Config = HeaderLinked, cfg })
	c.wall.onConfig(cfg)
	return c
}

// Which joins go home (docs/76 item 16): linked to a home that runs rooms,
// the wall and any code this relay does not hold; never a local room, and
// nothing while unlinked or when home runs no rooms.
func TestWallForwardRoom(t *testing.T) {
	c := linkedWithWall(t, true)
	w := c.wall
	for _, tc := range []struct {
		code  string
		local bool
		want  bool
	}{
		{"assembly-2026", true, true}, // the wall, even with the local copy present
		{"lan-team", false, true},     // a home room typed at the party
		{"abcdef", true, false},       // a room minted here
	} {
		if got := w.ForwardRoom(tc.code, tc.local); got != tc.want {
			t.Errorf("ForwardRoom(%q, local=%v) = %v, want %v", tc.code, tc.local, got, tc.want)
		}
	}
	c.setState(func(s *LinkState) { s.Header = HeaderOffline })
	if w.ForwardRoom("assembly-2026", true) || w.ForwardRoom("lan-team", false) {
		t.Error("an unlinked PoP forwards joins")
	}
	off := linkedWithWall(t, false)
	if off.wall.ForwardRoom("lan-team", false) || off.wall.ForwardRoom("assembly-2026", false) {
		t.Error("forwarding to a home that runs no rooms")
	}
}

// 0x18's wall fields: the code whenever there is a wall, and on a publish
// session whether that broadcast is on it; wall=hidden and Hide clear it,
// Show sets it, and the watcher hears each change.
func TestWallFillsLocalRelayInfo(t *testing.T) {
	c := linkedWithWall(t, true)
	if got := c.LocalRelayInfo(""); got.WallCode != "Assembly-2026" || got.OnWall {
		t.Fatalf("non-publish 0x18 = %+v", got)
	}
	c.wall.PublisherLive(httptest.NewRequest("CONNECT", "https://pop/publish?name=tuhis", nil), "ABC234")
	if got := c.LocalRelayInfo("ABC234"); !got.OnWall || got.WallCode != "Assembly-2026" || !got.Linked {
		t.Fatalf("on-wall 0x18 = %+v", got)
	}
	c.wall.PublisherLive(httptest.NewRequest("CONNECT", "https://pop/publish?wall=hidden", nil), "XYZ789")
	if got := c.LocalRelayInfo("XYZ789"); got.OnWall {
		t.Fatalf("wall=hidden 0x18 = %+v", got)
	}
	changed := make(chan struct{}, 8)
	cancel := c.WatchLocalRelayInfo("ABC234", func() { changed <- struct{}{} })
	defer cancel()
	if !c.wall.WallCommand("assembly-2026", wire.RoomCommand{Kind: wire.RoomCommandDetach, BroadcastID: "ABC234"}) {
		t.Fatal("Hide was not taken")
	}
	select {
	case <-changed:
	case <-time.After(5 * time.Second):
		t.Fatal("the watcher did not hear the Hide")
	}
	if got := c.LocalRelayInfo("ABC234"); got.OnWall {
		t.Fatalf("0x18 after Hide = %+v", got)
	}
	if !c.wall.WallCommand("assembly-2026", wire.RoomCommand{Kind: wire.RoomCommandAttach, BroadcastID: "ABC234"}) {
		t.Fatal("Show was not taken")
	}
	if got := c.LocalRelayInfo("ABC234"); !got.OnWall {
		t.Fatalf("0x18 after Show = %+v", got)
	}
	// A remembered Hide survives a reclaim without wall=hidden.
	c.wall.WallCommand("assembly-2026", wire.RoomCommand{Kind: wire.RoomCommandDetach, BroadcastID: "ABC234"})
	c.wall.PublisherLive(httptest.NewRequest("CONNECT", "https://pop/publish/ABC234", nil), "ABC234")
	if got := c.LocalRelayInfo("ABC234"); got.OnWall {
		t.Fatalf("a reclaim put a hidden broadcast back: %+v", got)
	}
	// Another room's commands are not the wall's.
	if c.wall.WallCommand("lan-team", wire.RoomCommand{Kind: wire.RoomCommandDetach, BroadcastID: "ABC234"}) {
		t.Fatal("a command for another room was taken as Hide")
	}
	// No wall: no wall fields.
	none := New(Options{})
	if got := none.LocalRelayInfo("ABC234"); got.WallCode != "" || got.OnWall {
		t.Fatalf("0x18 without a wall = %+v", got)
	}
}
