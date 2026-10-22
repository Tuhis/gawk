package transport

// R74 PP11 (docs/76 items 33-36): room control over a PoP link, both ends,
// and the party wall's vouched attaches. The home side runs against a
// net.Pipe standing in for one link stream; the PoP side is a real relay
// whose forwarder pipes into a second, real "home" relay.

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/Tuhis/gawk/gawk-server/internal/roomsrv"
	"github.com/Tuhis/gawk/gawk-server/rooms"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

const testWallPop = "assembly-2026"

// linkPeer is the PoP's end of one room stream as a test drives it.
type linkPeer struct {
	c net.Conn
}

func (p *linkPeer) status(t *testing.T) int {
	t.Helper()
	_ = p.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	var b [2]byte
	if _, err := io.ReadFull(p.c, b[:]); err != nil {
		t.Fatalf("read link status: %v", err)
	}
	return int(binary.BigEndian.Uint16(b[:]))
}

func (p *linkPeer) send(t *testing.T, msg []byte) {
	t.Helper()
	rec, err := wire.AppendRoomRecord(nil, msg)
	if err != nil {
		t.Fatal(err)
	}
	_ = p.c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := p.c.Write(rec); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func (p *linkPeer) hello(t *testing.T, nick string) {
	t.Helper()
	msg, err := wire.AppendRoomHello(nil, wire.RoomHello{Protocol: wire.RoomProtocolVersion, ClientKind: wire.RoomClientWebViewer, Nickname: nick})
	if err != nil {
		t.Fatal(err)
	}
	p.send(t, msg)
}

// next reads one record or the close record (returned as *roomLinkClosed).
func (p *linkPeer) next(t *testing.T) ([]byte, *roomLinkClosed) {
	t.Helper()
	_ = p.c.SetReadDeadline(time.Now().Add(5 * time.Second))
	rec, err := readRoomFrame(p.c, true)
	var closed *roomLinkClosed
	if errors.As(err, &closed) {
		return nil, closed
	}
	if err != nil {
		t.Fatalf("read link record: %v", err)
	}
	return rec[2:], nil
}

func (p *linkPeer) nextState(t *testing.T) wire.RoomState {
	t.Helper()
	msg, closed := p.next(t)
	if closed != nil {
		t.Fatalf("closed %+v, want a RoomState", closed)
	}
	st, err := wire.ParseRoomState(msg)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func (p *linkPeer) nextEvent(t *testing.T, kind uint8) wire.RoomEvent {
	t.Helper()
	for {
		msg, closed := p.next(t)
		if closed != nil {
			t.Fatalf("closed %+v, want event kind %d", closed, kind)
		}
		e, err := wire.ParseRoomEvent(msg)
		if err != nil {
			t.Fatal(err)
		}
		if e.Kind == kind {
			return e
		}
	}
}

func (p *linkPeer) closed(t *testing.T) *roomLinkClosed {
	t.Helper()
	for {
		_, closed := p.next(t)
		if closed != nil {
			return closed
		}
	}
}

// serveLinkStream starts ServeRoomStream on one end of a pipe.
func serveLinkStream(t *testing.T, ctx context.Context, srv *Server, pop, path, query string) *linkPeer {
	t.Helper()
	popEnd, homeEnd := net.Pipe()
	t.Cleanup(func() { popEnd.Close() })
	go srv.ServeRoomStream(ctx, pop, path, query, homeEnd)
	return &linkPeer{c: popEnd}
}

func upsertWall(t *testing.T, reg *roomsrv.Registry) {
	t.Helper()
	if err := reg.UpsertStatic(roomsrv.StaticRoom{Code: testWallPop, DisplayCode: "Assembly-2026", Pop: testWallPop, MaxBroadcasts: 40}); err != nil {
		t.Fatal(err)
	}
}

// A party guest's join, sent over the link, lands in the wall on this pod:
// status 200, then the room's own records, the ?name= fallback applied,
// no attach grant; a vouched attach reaches it; the room ending closes it
// with 4007 through the close record.
func TestServeRoomStreamJoinsTheWallAndClosesWith4007(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, _, _, srv, reg := startRoomServer(t, ctx, nil)
	upsertWall(t, reg)

	p := serveLinkStream(t, ctx, srv, testWallPop, "/room/Assembly-2026", "name=party-guest")
	// net.Pipe is unbuffered: read the status before writing (a QUIC
	// stream would buffer the hello).
	if s := p.status(t); s != http.StatusOK {
		t.Fatalf("status = %d, want 200", s)
	}
	p.hello(t, "")
	st := p.nextState(t)
	if st.Code != "Assembly-2026" || st.Flags&wire.RoomStateFlagAttachOK != 0 {
		t.Fatalf("wall state = %+v", st)
	}
	if len(st.Participants) != 1 || st.Participants[0].Nickname != "party-guest" {
		t.Fatalf("participants = %+v, want the ?name= fallback", st.Participants)
	}

	if err := srv.VouchWall(ctx, testWallPop, WallOpAttach, "assembly-2026", "ABCDEF", "Stream 1"); err != nil {
		t.Fatalf("VouchWall: %v", err)
	}
	if e := p.nextEvent(t, wire.RoomEventAttachmentAdded); e.Attachment.BroadcastID != "ABCDEF" || e.Attachment.Label != "Stream 1" {
		t.Fatalf("attach event = %+v", e)
	}
	if err := srv.VouchWall(ctx, testWallPop, WallOpDetach, "assembly-2026", "ABCDEF", ""); err != nil {
		t.Fatalf("VouchWall detach: %v", err)
	}
	p.nextEvent(t, wire.RoomEventAttachmentRemoved)

	reg.EndRoom(testWallPop, wire.RoomEndReasonOperator)
	if c := p.closed(t); c.code != wire.CloseCodeRoomEnded {
		t.Fatalf("close record = %+v, want 4007", c)
	}
}

// The join route's status vocabulary, over the link.
func TestServeRoomStreamStatuses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, _, _, srv, reg := startRoomServer(t, ctx, nil)
	upsertWall(t, reg)
	for _, tc := range []struct {
		name, path, query string
		want              int
	}{
		{"unknown room", "/room/no-such-room", "", http.StatusNotFound},
		{"a mint never travels", "/room/new", "", http.StatusNotFound},
		{"not a room path", "/publish", "", http.StatusNotFound},
		{"an attach secret on the wall", "/room/assembly-2026", "attach=guess", http.StatusForbidden},
		{"a forged creator token", "/room/assembly-2026", "creator=00000000000000000000000000000000", http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := serveLinkStream(t, ctx, srv, testWallPop, tc.path, tc.query)
			if s := p.status(t); s != tc.want {
				t.Fatalf("status = %d, want %d", s, tc.want)
			}
		})
	}
	// No registry: 404.
	_, _, _, plain, _ := startRoomServer(t, ctx, nil)
	plain.SetRooms(nil)
	if s := serveLinkStream(t, ctx, plain, testWallPop, "/room/assembly-2026", "").status(t); s != http.StatusNotFound {
		t.Fatalf("no registry: status %d", s)
	}
}

func TestVouchWallRefusals(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, _, _, srv, reg := startRoomServer(t, ctx, nil)
	upsertWall(t, reg)
	if err := srv.VouchWall(ctx, "another-pop", WallOpAttach, "assembly-2026", "ABCDEF", ""); !errors.Is(err, roomsrv.ErrForbidden) {
		t.Errorf("another PoP: %v", err)
	}
	if err := srv.VouchWall(ctx, testWallPop, WallOpAttach, "nosuch", "ABCDEF", ""); !errors.Is(err, roomsrv.ErrNotFound) {
		t.Errorf("unknown wall: %v", err)
	}
	if err := srv.VouchWall(ctx, testWallPop, "explode", "assembly-2026", "ABCDEF", ""); !errors.Is(err, roomsrv.ErrForbidden) {
		t.Errorf("unknown op: %v", err)
	}
	srv.SetRooms(nil)
	if err := srv.VouchWall(ctx, testWallPop, WallOpAttach, "assembly-2026", "ABCDEF", ""); !errors.Is(err, roomsrv.ErrDisabled) {
		t.Errorf("no rooms: %v", err)
	}
}

// fakeForwarder is a PoP's link as transport sees it: OpenRoom pipes into
// a "home" relay's ServeRoomStream.
type fakeForwarder struct {
	home   *Server
	ctx    context.Context
	wall   string
	mu     sync.Mutex
	opened []string
	cmds   []wire.RoomCommand
}

func (f *fakeForwarder) ForwardRoom(code string, local bool) bool { return code == f.wall || !local }

func (f *fakeForwarder) OpenRoom(_ context.Context, path, rawQuery string) (io.ReadWriteCloser, error) {
	f.mu.Lock()
	f.opened = append(f.opened, path+"?"+rawQuery)
	f.mu.Unlock()
	a, b := net.Pipe()
	go f.home.ServeRoomStream(f.ctx, testWallPop, path, rawQuery, b)
	return a, nil
}

func (f *fakeForwarder) WallCommand(code string, cmd wire.RoomCommand) bool {
	if code != f.wall {
		return false
	}
	f.mu.Lock()
	f.cmds = append(f.cmds, cmd)
	f.mu.Unlock()
	return true
}

func (f *fakeForwarder) commands() []wire.RoomCommand {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]wire.RoomCommand(nil), f.cmds...)
}

// The PoP side: a guest's join of the wall goes home and back, the home's
// refusal is the guest's status, the home's 4007 is the guest's close, the
// PoP's own resume token never travels, and Hide/Show from the broadcaster
// that proved its broadcast become vouched wall commands instead of
// commands home would refuse.
func TestPopForwardsWallJoinsHomeAndInterceptsHideShow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, _, _, home, homeReg := startRoomServer(t, ctx, nil)
	upsertWall(t, homeReg)
	popPort, clientTLS, _, pop, popReg := startRoomServer(t, ctx, nil)
	fwd := &fakeForwarder{home: home, ctx: ctx, wall: testWallPop}
	pop.SetRoomForwarder(fwd)

	url := func(p string) string { return fmt.Sprintf("https://127.0.0.1:%d%s", popPort, p) }
	expectStatus(t, ctx, url("/room/no-such-room"), clientTLS, http.StatusNotFound)
	expectStatus(t, ctx, url("/room/Assembly-2026?attach=guess"), clientTLS, http.StatusForbidden)

	// A broadcaster at the PoP: its resume token is the PoP's.
	pubSess, id, tokenHex := dialPublisherHandshake(t, ctx, popPort, clientTLS)
	defer pubSess.CloseWithError(0, "")
	bc := openControl(t, ctx, url("/room/Assembly-2026?broadcast="+id+"&resume="+tokenHex+"&name=pc"), clientTLS, "")
	st := bc.nextState(t)
	if st.Code != "Assembly-2026" || len(st.Participants) != 1 || st.Participants[0].Nickname != "pc" {
		t.Fatalf("state from home = %+v", st)
	}
	fwd.mu.Lock()
	opened := strings.Join(fwd.opened, " ")
	fwd.mu.Unlock()
	if strings.Contains(opened, "resume=") || strings.Contains(opened, "broadcast=") {
		t.Fatalf("the PoP's resume proof travelled home: %s", opened)
	}

	// Hide: a Detach of its own (proven) broadcast is the forwarder's.
	bc.command(t, wire.RoomCommand{Kind: wire.RoomCommandDetach, BroadcastID: id})
	// A Detach of a broadcast it never proved goes home, which refuses it.
	bc.command(t, wire.RoomCommand{Kind: wire.RoomCommandDetach, BroadcastID: "ZZZZZZ"})
	if e := bc.nextEvent(t, wire.RoomEventCommandRejected); e.Command != wire.RoomCommandDetach {
		t.Fatalf("home's answer = %+v", e)
	}
	// Show from a second session that proves the broadcast with the
	// Attach's own token.
	other := openControl(t, ctx, url("/room/assembly-2026"), clientTLS, "other")
	other.nextState(t)
	tok, _ := hex.DecodeString(tokenHex)
	other.command(t, wire.RoomCommand{Kind: wire.RoomCommandAttach, BroadcastID: id, ResumeToken: tok, Label: "pc"})
	// A forged token is not a proof: it goes home and is refused there
	// (the wall takes no ordinary attach).
	other.command(t, wire.RoomCommand{Kind: wire.RoomCommandAttach, BroadcastID: "ABCDEF", ResumeToken: make([]byte, 16)})
	if e := other.nextEvent(t, wire.RoomEventCommandRejected); e.Command != wire.RoomCommandAttach || e.Reason != wire.RoomRejectForbidden {
		t.Fatalf("forged attach answer = %+v", e)
	}
	waitFor(t, 5*time.Second, func() bool { return len(fwd.commands()) == 2 }, "two vouched wall commands")
	cmds := fwd.commands()
	if cmds[0].Kind != wire.RoomCommandDetach || cmds[0].BroadcastID != id || cmds[1].Kind != wire.RoomCommandAttach || cmds[1].BroadcastID != id {
		t.Fatalf("wall commands = %+v", cmds)
	}

	// A room the PoP holds locally stays local (not forwarded).
	if err := popReg.UpsertStatic(roomsrv.StaticRoom{Code: "lan-team"}); err != nil {
		t.Fatal(err)
	}
	local := openControl(t, ctx, url("/room/lan-team"), clientTLS, "local")
	if lst := local.nextState(t); lst.Code != "lan-team" {
		t.Fatalf("local room state = %+v", lst)
	}

	// The home ends the wall: 4007 reaches the guest through the PoP.
	homeReg.EndRoom(testWallPop, wire.RoomEndReasonOperator)
	if c := sessionCloseCode(t, bc.sess); c != wire.CloseCodeRoomEnded {
		t.Fatalf("guest close = %d, want 4007", c)
	}
}

// A home stream that dies without a close record reads as a lost home:
// 4002, so the guest reconnects (non-terminal).
func TestPopLostHomeIsDraining(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	popPort, clientTLS, _, pop, _ := startRoomServer(t, ctx, nil)
	var homeEnd net.Conn
	ready := make(chan struct{})
	pop.SetRoomForwarder(roomForwarderFunc(func(path, q string) (io.ReadWriteCloser, error) {
		a, b := net.Pipe()
		homeEnd = b
		go func() {
			var s [2]byte
			binary.BigEndian.PutUint16(s[:], http.StatusOK)
			_, _ = b.Write(s[:])
			close(ready)
			_, _ = io.Copy(io.Discard, b)
		}()
		return a, nil
	}))
	c := openControl(t, ctx, fmt.Sprintf("https://127.0.0.1:%d/room/assembly-2026", popPort), clientTLS, "guest")
	<-ready
	time.Sleep(50 * time.Millisecond)
	homeEnd.Close()
	if code := sessionCloseCode(t, c.sess); code != wire.CloseCodeServerDraining {
		t.Fatalf("close = %d, want 4002", code)
	}
}

type roomForwarderFunc func(path, rawQuery string) (io.ReadWriteCloser, error)

func (f roomForwarderFunc) ForwardRoom(string, bool) bool { return true }
func (f roomForwarderFunc) OpenRoom(_ context.Context, p, q string) (io.ReadWriteCloser, error) {
	return f(p, q)
}
func (f roomForwarderFunc) WallCommand(string, wire.RoomCommand) bool { return false }

func wallRoomObject(t *testing.T, name, pop string) *unstructured.Unstructured {
	t.Helper()
	r := &rooms.Room{TypeMeta: metav1.TypeMeta{APIVersion: rooms.SchemeGroupVersion.String(), Kind: rooms.Kind},
		Spec: rooms.RoomSpec{Kind: rooms.KindStatic, DisplayCode: "Assembly-2026", Pop: pop, MaxBroadcasts: 40}}
	r.Name = name
	r.Namespace = "gawk"
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(r)
	if err != nil {
		t.Fatal(err)
	}
	return &unstructured.Unstructured{Object: raw}
}

// Across the fleet: the pod holding the PoP's link is not the wall's home.
// A vouched attach goes to the home over POST /internal/roomvouch (real
// HTTP/3), and a linked guest's join is piped to the home's /internal/room;
// both land in the one wall. With nobody homing the wall yet, the link pod
// adopts it.
func TestWallVouchAndLinkJoinReachTheWallsHomePod(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := newRoomFleet(t, wallRoomObject(t, "assembly-2026", testWallPop))
	a := f.startRoomPod(t, ctx, "pod-a")
	b := f.startRoomPod(t, ctx, "pod-b")
	b.srv.roomClusterWiring().vouch = newRoomVoucher("localhost", "fleet-psk", f.pool)

	first := openControl(t, ctx, a.url("/room/assembly-2026"), f.clientTLS, "outsider")
	if st := first.nextState(t); st.Flags&wire.RoomStateFlagAttachOK != 0 {
		t.Fatalf("a wall from its CR granted attach: %+v", st)
	}
	waitFor(t, 15*time.Second, func() bool {
		h, ok := b.store.Resolve("assembly-2026")
		return ok && h.Live && h.Holder == "pod-a"
	}, "pod-b to resolve the wall's home")

	if err := b.srv.VouchWall(ctx, testWallPop, WallOpAttach, "assembly-2026", "ABCDEF", "Stream 1"); err != nil {
		t.Fatalf("remote VouchWall: %v", err)
	}
	if e := first.nextEvent(t, wire.RoomEventAttachmentAdded); e.Attachment.BroadcastID != "ABCDEF" {
		t.Fatalf("home saw %+v", e)
	}
	if err := b.srv.VouchWall(ctx, "another-pop", WallOpAttach, "assembly-2026", "ABCDEG", ""); !errors.Is(err, roomsrv.ErrForbidden) {
		t.Fatalf("another PoP's remote vouch: %v, want ErrForbidden", err)
	}
	if _, held := b.store.Holding("assembly-2026"); held {
		t.Fatal("pod-b took the wall it should vouch to")
	}

	p := serveLinkStream(t, ctx, b.srv, testWallPop, "/room/assembly-2026", "name=party")
	if s := p.status(t); s != http.StatusOK {
		t.Fatalf("piped join status = %d", s)
	}
	p.hello(t, "")
	if st := p.nextState(t); len(st.Participants) != 2 || len(st.Attachments) != 1 {
		t.Fatalf("piped state = %+v", st)
	}
	if e := first.nextEvent(t, wire.RoomEventParticipantJoined); e.Participant.Nickname != "party" {
		t.Fatalf("home saw %+v", e)
	}
	// The wall ends at home (its CR deleted): 4007 comes back as the close
	// record through both hops.
	if err := f.client.Resource(rooms.GroupVersionResource).Namespace("gawk").Delete(ctx, "assembly-2026", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if c := p.closed(t); c.code != wire.CloseCodeRoomEnded {
		t.Fatalf("piped close = %+v, want 4007", c)
	}
}

func TestWallVouchAdoptsAWallNobodyHomes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	f := newRoomFleet(t, wallRoomObject(t, "assembly-2026", testWallPop))
	b := f.startRoomPod(t, ctx, "pod-b")
	if err := b.srv.VouchWall(ctx, testWallPop, WallOpAttach, "assembly-2026", "ABCDEF", ""); err != nil {
		t.Fatalf("VouchWall: %v", err)
	}
	if _, held := b.store.Holding("assembly-2026"); !held {
		t.Fatal("the vouch did not adopt the unhomed wall")
	}
	if got := b.reg.Attachments("assembly-2026"); len(got) != 1 || got[0].BroadcastID != "ABCDEF" {
		t.Fatalf("attachments = %+v", got)
	}
}

// The internal vouch route's gates, in order.
func TestInternalRoomVouchGates(t *testing.T) {
	fc := &fakeRoomCluster{held: map[string]int64{"assembly-2026": 3}}
	srv, _, reg := newFakeClusterServer(t, fc, &scriptedDialer{})
	upsertWall(t, reg)
	post := func(code, query, body string) int {
		r := httptest.NewRequest(http.MethodPost, "https://relay/internal/roomvouch/"+code+"?"+query, strings.NewReader(body))
		r.SetPathValue("code", code)
		w := httptest.NewRecorder()
		srv.handleInternalRoomVouch(w, r)
		return w.Code
	}
	body := `{"op":"attach","pop":"` + testWallPop + `","id":"ABCDEF"}`
	for _, tc := range []struct {
		name, code, query, body string
		want                    int
	}{
		{"bad psk", "assembly-2026", "psk=wrong&gen=3", body, http.StatusUnauthorized},
		{"not the home", "elsewhere", "psk=fleet-psk&gen=3", body, http.StatusNotFound},
		{"stale generation", "assembly-2026", "psk=fleet-psk&gen=2", body, http.StatusConflict},
		{"bad body", "assembly-2026", "psk=fleet-psk&gen=3", "{", http.StatusBadRequest},
		{"another pop", "assembly-2026", "psk=fleet-psk&gen=3", `{"op":"attach","pop":"x","id":"ABCDEF"}`, http.StatusForbidden},
		{"ok", "assembly-2026", "psk=fleet-psk&gen=3", body, http.StatusNoContent},
	} {
		if got := post(tc.code, tc.query, tc.body); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
	if len(reg.Attachments("assembly-2026")) != 1 {
		t.Fatal("the accepted vouch did not attach")
	}
	// The status vocabulary round-trips.
	for _, err := range []error{nil, roomsrv.ErrNotFound, roomsrv.ErrForbidden, roomsrv.ErrAlreadyAttached, roomsrv.ErrMaxBroadcasts} {
		if back := vouchError(vouchStatus(err)); !errors.Is(back, err) && !(err == nil && back == nil) {
			t.Errorf("%v -> %d -> %v", err, vouchStatus(err), back)
		}
	}
	if !errors.Is(vouchError(http.StatusBadGateway), roomsrv.ErrUnavailable) {
		t.Error("an unexpected status is not ErrUnavailable")
	}
}
