package transport

// R42 control-session protocol failures over real QUIC (docs/44 §4.6): the
// paths serveRoomSession takes when a participant misbehaves after the
// upgrade — the session dies before any stream opens, the stream ends
// before a whole RoomHello arrives, a command record that does not parse,
// and the post-upgrade participant-limit race — plus two tolerances the
// protocol promises: an unknown command kind is rejected in-band rather
// than closing the session, and a hello without a nickname falls back to
// the ?name= query.

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/internal/config"
	"github.com/Tuhis/gawk/gawk-server/internal/roomsrv"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// A session that dies before opening its control stream: the relay logs
// the no-stream outcome and moves on; the room never sees a participant.
func TestRoomSessionClosedBeforeControlStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var logs syncBuffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	cfg := config.Config{MaxSubscribers: 5, MaxIdleTimeout: 30 * time.Second, KeepAlivePeriod: 10 * time.Second, BroadcastGrace: 5 * time.Minute, Rooms: true}
	port, clientTLS, r, _, srv := startTestServerCfgLogSrv(t, ctx, cfg, log)
	reg := roomsrv.NewRegistry(roomsrv.Options{Broadcasts: hubBroadcastsAdapter{r}, Obfuscate: r.ObfuscateID, Log: discardLog, EmptyGrace: time.Hour})
	srv.SetRooms(reg)
	if err := reg.UpsertStatic(roomsrv.StaticRoom{Code: "abc"}); err != nil {
		t.Fatal(err)
	}
	sess := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/room/abc", port), clientTLS)
	sess.CloseWithError(0, "changed my mind")
	waitFor(t, 5*time.Second, func() bool { return strings.Contains(logs.String(), "room session opened no control stream") }, "the no-stream log line")
	if info, _ := reg.Lookup("abc"); info.Participants != 0 {
		t.Fatalf("a session that never said hello joined the room: %+v", info)
	}
}

// The stream ends mid-hello: close code 400 ("expected RoomHello").
func TestRoomSessionTruncatedHelloIsClosed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	port, clientTLS, _, _, reg := startRoomServer(t, ctx, nil)
	if err := reg.UpsertStatic(roomsrv.StaticRoom{Code: "abc"}); err != nil {
		t.Fatal(err)
	}
	sess := dial(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/room/abc", port), clientTLS)
	stream, err := sess.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Half a length prefix, then FIN.
	if _, err := stream.Write([]byte{0x00}); err != nil {
		t.Fatal(err)
	}
	if err := stream.Close(); err != nil {
		t.Fatal(err)
	}
	if code := sessionCloseCode(t, sess); code != roomCloseBadRequest {
		t.Fatalf("close = %d, want %d", code, roomCloseBadRequest)
	}
}

// Two joiners clear the pre-upgrade gate against an empty room with a
// participant limit of one; the second hello finds the room full and is
// closed post-upgrade with 429, while the first is unaffected.
func TestRoomSessionFullAfterUpgradeIs429(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	port, clientTLS, _, _, reg := startRoomServer(t, ctx, func(o *roomsrv.Options) { o.MaxParticipants = 1 })
	if err := reg.UpsertStatic(roomsrv.StaticRoom{Code: "abc"}); err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("https://127.0.0.1:%d/room/abc", port)
	first := dial(t, ctx, url, clientTLS)
	second := dial(t, ctx, url, clientTLS)
	c1 := helloOn(t, ctx, first, "one")
	if st := c1.nextState(t); len(st.Participants) != 1 {
		t.Fatalf("first state = %+v", st)
	}
	helloOn(t, ctx, second, "two")
	if code := sessionCloseCode(t, second); code != http.StatusTooManyRequests {
		t.Fatalf("second close = %d, want 429", code)
	}
	// The first participant is still in, and still served.
	c1.command(t, wire.RoomCommand{Kind: wire.RoomCommandResync})
	if st := c1.nextState(t); len(st.Participants) != 1 || st.Participants[0].Nickname != "one" {
		t.Fatalf("first participant after the rejected second = %+v", st)
	}
}

// A command record that does not parse closes the session with 400; an
// unknown command KIND does not — it is rejected in-band so a newer client
// keeps its session (docs/44: forward-compatible kinds).
func TestRoomSessionMalformedCommandClosesUnknownKindIsRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	port, clientTLS, _, _, reg := startRoomServer(t, ctx, nil)
	if err := reg.UpsertStatic(roomsrv.StaticRoom{Code: "abc"}); err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("https://127.0.0.1:%d/room/abc", port)

	c := openControl(t, ctx, url, clientTLS, "v")
	c.nextState(t)
	// Unknown kind 0x7f: rejected as unsupported, session intact.
	c.send(t, []byte{wire.Version, wire.TypeRoomCommand, 0x7f})
	if e := c.nextEvent(t, wire.RoomEventCommandRejected); e.Reason != wire.RoomRejectUnsupported {
		t.Fatalf("unknown kind rejection = %+v", e)
	}
	c.command(t, wire.RoomCommand{Kind: wire.RoomCommandResync})
	c.nextState(t)

	// A SetNickname whose length byte promises more than the record
	// carries: malformed, 400.
	msg, err := wire.AppendRoomCommand(nil, wire.RoomCommand{Kind: wire.RoomCommandSetNickname, Nickname: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	c.send(t, msg[:len(msg)-1])
	if code := sessionCloseCode(t, c.sess); code != roomCloseBadRequest {
		t.Fatalf("close = %d, want %d", code, roomCloseBadRequest)
	}
	waitFor(t, 5*time.Second, func() bool { info, _ := reg.Lookup("abc"); return info.Participants == 0 }, "the participant to leave")
}

// A hello with no nickname takes the ?name= fallback (the native
// broadcasters pass it in the URL).
func TestRoomSessionNicknameFallsBackToQuery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	port, clientTLS, _, _, reg := startRoomServer(t, ctx, nil)
	if err := reg.UpsertStatic(roomsrv.StaticRoom{Code: "abc"}); err != nil {
		t.Fatal(err)
	}
	c := openControl(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/room/abc?name=fromquery", port), clientTLS, "")
	st := c.nextState(t)
	if len(st.Participants) != 1 || st.Participants[0].Nickname != "fromquery" {
		t.Fatalf("state = %+v, want the query nickname", st)
	}
}
