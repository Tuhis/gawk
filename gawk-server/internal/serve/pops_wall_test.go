package serve

import (
	"io"
	"log/slog"
	"testing"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/hub"
	"github.com/Tuhis/gawk/gawk-server/internal/popsrv"
	"github.com/Tuhis/gawk/gawk-server/internal/roomsrv"
	"github.com/Tuhis/gawk/gawk-server/internal/transport"
)

// The party wall rides on home's rooms: with -rooms off there is nothing to
// vouch into and the link's room streams stay unserved; with them on, the
// wall takes the links' streams.
func TestWirePopWall(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := hub.NewRegistry(log, hub.Options{})
	cfg := config.Config{Pops: true, Rooms: true}
	srv := transport.New(cfg, r, nil, log, nil)

	off := popsrv.New(popsrv.Options{Registry: r, Log: log})
	if err := wirePopWall(t.Context(), Wired{Registry: r, Server: srv, Cfg: config.Config{Pops: true}}, off, log); err != nil {
		t.Fatal(err)
	}
	if off.ServesLinkStreams() {
		t.Fatal("a home without rooms serves room streams")
	}

	on := popsrv.New(popsrv.Options{Registry: r, Log: log})
	rooms := roomsrv.NewRegistry(roomsrv.Options{Broadcasts: srv.RoomBroadcasts(), Log: log})
	if err := wirePopWall(t.Context(), Wired{Registry: r, Server: srv, Rooms: rooms, Cfg: cfg,
		Reservations: popsrv.NewMemReservations()}, on, log); err != nil {
		t.Fatal(err)
	}
	if !on.ServesLinkStreams() {
		t.Fatal("the wall is not wired onto the links")
	}
}
