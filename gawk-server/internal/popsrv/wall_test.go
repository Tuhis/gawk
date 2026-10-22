package popsrv

// The PP11 acceptance rows (docs/76 §11) in-process: one home pod with
// rooms and the wall wired, one linked PoP, real QUIC everywhere. The
// reservation W3-uplink makes at the PoP's mint is made here by hand (the
// wall only vouches for an ID the PoP reserved).

import (
	"context"
	"encoding/hex"
	"io"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/popclient"
	"github.com/Tuhis/gawk/gawk-server/internal/roomsrv"
	"github.com/Tuhis/gawk/gawk-server/pops"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

const wallCode = "Assembly-2026"

// wallFleet is the harness with home rooms, the wall on the home pod, and
// the PoP linked with a wall in its spec.
type wallFleet struct {
	f    *HomeFleet
	pop  *PoP
	home *HomePod
	reg  *roomsrv.Registry
	res  Reservations
}

func startWallFleet(t *testing.T) *wallFleet {
	t.Helper()
	f, pop := StartHomeAndPop(t, 1)
	home := f.Pods[0]
	// The fleet's -max-room-broadcasts default of 4: the wall's own cap
	// must win over it.
	reg := roomsrv.NewRegistry(roomsrv.Options{Broadcasts: home.Server.RoomBroadcasts(), Obfuscate: home.Registry.ObfuscateID,
		Log: harnessLog(t), EmptyGrace: time.Hour, MaxBroadcasts: 4})
	home.Server.SetRooms(reg)
	// gawk-admin's wizard creates the wall Room with the Pop: spec.pop and
	// spec.maxBroadcasts from limits.maxBroadcasts.
	if err := reg.UpsertStatic(roomsrv.StaticRoom{Code: wallCode, DisplayCode: wallCode, Pop: popName, MaxBroadcasts: 40, MaxParticipants: 500}); err != nil {
		t.Fatal(err)
	}
	res := NewClusterReservations(home.Coord)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	StartWall(ctx, home.Manager, WallOptions{Host: home.Server, Owner: res, Log: harnessLog(t)})
	pairLinked(t, f, pop)
	f.UpdateSpec(t, popName, func(s *pops.PopSpec) { s.Wall = &pops.Wall{RoomCode: wallCode} })
	WaitFor(t, "the PoP's config to carry the wall", func() bool {
		c := pop.Client.State().Config
		return c != nil && c.WallCode == wallCode
	})
	WaitFor(t, "the PoP's local wall", func() bool { _, ok := pop.Rooms.WallPop(wallCode); return ok })
	return &wallFleet{f: f, pop: pop, home: home, reg: reg, res: res}
}

// publisher is a broadcaster session at the PoP, with every uni-stream
// message it got.
type publisher struct {
	sess      *webtransport.Session
	id, token string
	mu        sync.Mutex
	infos     []wire.LocalRelayInfo
}

func (w *wallFleet) publish(t *testing.T, query string) *publisher {
	t.Helper()
	sess := clientDial(t, w.f, "https://"+w.pop.Addr()+"/publish?"+query)
	t.Cleanup(func() { sess.CloseWithError(0, "") })
	p := &publisher{sess: sess}
	got := make(chan struct{}, 4)
	go func() {
		for {
			st, err := sess.AcceptUniStream(context.Background())
			if err != nil {
				return
			}
			data, _ := io.ReadAll(st)
			_, typ, err := wire.PeekType(data)
			if err != nil {
				continue
			}
			p.mu.Lock()
			switch typ {
			case wire.TypeBroadcastAnnounce:
				p.id, _ = wire.ParseBroadcastAnnounce(data)
			case wire.TypeResumeToken:
				tok, _ := wire.ParseResumeToken(data)
				p.token = hex.EncodeToString(tok)
			case wire.TypeLocalRelayInfo:
				if info, err := wire.ParseLocalRelayInfo(data); err == nil {
					p.infos = append(p.infos, info)
				}
			}
			p.mu.Unlock()
			select {
			case got <- struct{}{}:
			default:
			}
		}
	}()
	WaitFor(t, "the publisher handshake", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.id != "" && p.token != ""
	})
	// What W3-uplink's reserve-before-announce does at the mint.
	if r, err := w.res.Reserve(context.Background(), p.id, popName, 40); err != nil || r != ReserveOK {
		t.Fatalf("reserve %s: %v %v", p.id, r, err)
	}
	return p
}

func (p *publisher) lastInfo(t *testing.T) (wire.LocalRelayInfo, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.infos) == 0 {
		return wire.LocalRelayInfo{}, false
	}
	return p.infos[len(p.infos)-1], true
}

func (p *publisher) waitInfo(t *testing.T, what string, ok func(wire.LocalRelayInfo) bool) {
	t.Helper()
	WaitFor(t, what, func() bool {
		info, got := p.lastInfo(t)
		return got && ok(info)
	})
}

// room is one control session as a test client.
type room struct {
	sess   *webtransport.Session
	stream *webtransport.Stream
}

func openRoom(t *testing.T, f *HomeFleet, target, nick string) *room {
	t.Helper()
	sess := clientDial(t, f, target)
	t.Cleanup(func() { sess.CloseWithError(0, "") })
	stream, err := sess.OpenStreamSync(pairCtx(t))
	if err != nil {
		t.Fatal(err)
	}
	r := &room{sess: sess, stream: stream}
	hello, _ := wire.AppendRoomHello(nil, wire.RoomHello{Protocol: wire.RoomProtocolVersion, ClientKind: wire.RoomClientWebViewer, Nickname: nick})
	r.send(t, hello)
	return r
}

func (r *room) send(t *testing.T, msg []byte) {
	t.Helper()
	rec, _ := wire.AppendRoomRecord(nil, msg)
	if _, err := r.stream.Write(rec); err != nil {
		t.Fatal(err)
	}
}

func (r *room) command(t *testing.T, cmd wire.RoomCommand) {
	t.Helper()
	msg, err := wire.AppendRoomCommand(nil, cmd)
	if err != nil {
		t.Fatal(err)
	}
	r.send(t, msg)
}

// next reads one record and parses it as a protocol-1 client does: every
// record the wall produces must parse.
func (r *room) next(t *testing.T, within time.Duration) (any, bool) {
	t.Helper()
	_ = r.stream.SetReadDeadline(time.Now().Add(within))
	var hdr [2]byte
	if _, err := io.ReadFull(r.stream, hdr[:]); err != nil {
		return nil, false
	}
	n, err := wire.ParseRoomRecordLength(hdr[:])
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r.stream, buf); err != nil {
		t.Fatal(err)
	}
	_, typ, _ := wire.PeekType(buf)
	switch typ {
	case wire.TypeRoomState:
		st, err := wire.ParseRoomState(buf)
		if err != nil {
			t.Fatalf("a protocol-1 client cannot parse the snapshot: %v", err)
		}
		return st, true
	case wire.TypeRoomEvent:
		ev, err := wire.ParseRoomEvent(buf)
		if err != nil {
			t.Fatalf("a protocol-1 client cannot parse the event: %v", err)
		}
		return ev, true
	}
	t.Fatalf("unexpected record type 0x%02x", typ)
	return nil, false
}

func (r *room) state(t *testing.T) wire.RoomState {
	t.Helper()
	m, ok := r.next(t, 5*time.Second)
	st, isState := m.(wire.RoomState)
	if !ok || !isState {
		t.Fatalf("want a RoomState, got %T", m)
	}
	return st
}

func (r *room) event(t *testing.T, kind uint8) wire.RoomEvent {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		m, ok := r.next(t, time.Until(deadline))
		if !ok {
			break
		}
		if ev, isEv := m.(wire.RoomEvent); isEv && ev.Kind == kind {
			return ev
		}
	}
	t.Fatalf("no room event of kind 0x%02x", kind)
	return wire.RoomEvent{}
}

func (w *wallFleet) onWall(id string) bool {
	for _, a := range w.reg.Attachments(wallCode) {
		if a.BroadcastID == id {
			return true
		}
	}
	return false
}

// Party broadcasts appear on the wall, a fifth and later included, labelled
// with the broadcaster's name or "Stream n"; an outsider's ordinary Attach
// is refused; and a broadcaster's 0x18 carries the wall code and its
// on-wall state.
func TestPartyBroadcastsJoinTheWallAndOutsidersCannot(t *testing.T) {
	w := startWallFleet(t)
	outsider := openRoom(t, w.f, "https://"+w.home.Addr()+"/room/"+wallCode, "outsider")
	if st := outsider.state(t); st.Flags&wire.RoomStateFlagAttachOK != 0 || st.Code != wallCode {
		t.Fatalf("outsider's wall state = %+v", st)
	}
	var pubs []*publisher
	for i := range 6 {
		q := ""
		if i == 0 {
			q = url.Values{"name": {"tuhis"}}.Encode()
		}
		p := w.publish(t, q)
		pubs = append(pubs, p)
		ev := outsider.event(t, wire.RoomEventAttachmentAdded)
		if ev.Attachment.BroadcastID != p.id {
			t.Fatalf("wall tile #%d = %s, want %s", i+1, ev.Attachment.BroadcastID, p.id)
		}
		if i == 0 && ev.Attachment.Label != "tuhis" {
			t.Errorf("named broadcaster's tile label = %q", ev.Attachment.Label)
		}
		if i == 1 && ev.Attachment.Label != "Stream 2" {
			t.Errorf("unnamed broadcaster's tile label = %q, want Stream 2", ev.Attachment.Label)
		}
	}
	if n := len(w.reg.Attachments(wallCode)); n != 6 {
		t.Fatalf("wall has %d tiles, want 6", n)
	}
	pubs[0].waitInfo(t, "0x18 with the wall", func(i wire.LocalRelayInfo) bool {
		return i.Linked && i.OnWall && i.WallCode == wallCode
	})

	// An outsider's home broadcast: refused at the gate with an attach
	// secret, and as a command without one.
	homePub := clientDial(t, w.f, "https://"+w.home.Addr()+"/publish")
	defer homePub.CloseWithError(0, "")
	hid, htok := publisherHandshake(t, pairCtx(t), homePub)
	tok, _ := hex.DecodeString(htok)
	outsider.command(t, wire.RoomCommand{Kind: wire.RoomCommandAttach, BroadcastID: hid, ResumeToken: tok, Label: "mine"})
	if ev := outsider.event(t, wire.RoomEventCommandRejected); ev.Reason != wire.RoomRejectForbidden {
		t.Fatalf("outsider attach rejected with %d, want forbidden", ev.Reason)
	}
	if w.onWall(hid) {
		t.Fatal("an outsider's broadcast is on the wall")
	}
	_, status, _ := rawDialPublic(t, w.f, w.home, "/room/"+wallCode+"?attach=guess")
	if status != 403 {
		t.Fatalf("attach secret on the wall: %d, want 403", status)
	}
}

// Hide removes and Show restores, through a party guest's room session at
// the PoP proxied home over the link; the broadcaster's 0x18 follows; a
// broadcaster that remembered Hide (wall=hidden) never appears.
func TestWallHideShowAndHiddenNeverFlashes(t *testing.T) {
	w := startWallFleet(t)
	outsider := openRoom(t, w.f, "https://"+w.home.Addr()+"/room/"+wallCode, "outsider")
	outsider.state(t)
	p := w.publish(t, "")
	outsider.event(t, wire.RoomEventAttachmentAdded)
	p.waitInfo(t, "on-wall 0x18", func(i wire.LocalRelayInfo) bool { return i.OnWall })

	// The broadcaster's own wall session at the PoP, proving its broadcast.
	q := url.Values{"broadcast": {p.id}, "resume": {p.token}}
	bc := openRoom(t, w.f, "https://"+w.pop.Addr()+"/room/"+wallCode+"?"+q.Encode(), "tuhis")
	if st := bc.state(t); st.Code != wallCode || len(st.Participants) != 2 {
		t.Fatalf("the party guest's state did not come from home: %+v", st)
	}
	bc.command(t, wire.RoomCommand{Kind: wire.RoomCommandDetach, BroadcastID: p.id})
	if ev := outsider.event(t, wire.RoomEventAttachmentRemoved); ev.Attachment.BroadcastID != p.id {
		t.Fatalf("hide removed %+v", ev)
	}
	p.waitInfo(t, "0x18 after Hide", func(i wire.LocalRelayInfo) bool { return !i.OnWall && i.WallCode == wallCode })
	tok, _ := hex.DecodeString(p.token)
	bc.command(t, wire.RoomCommand{Kind: wire.RoomCommandAttach, BroadcastID: p.id, ResumeToken: tok})
	if ev := outsider.event(t, wire.RoomEventAttachmentAdded); ev.Attachment.BroadcastID != p.id {
		t.Fatalf("show restored %+v", ev)
	}
	p.waitInfo(t, "0x18 after Show", func(i wire.LocalRelayInfo) bool { return i.OnWall })

	// wall=hidden: never on the wall, not even for a moment.
	hidden := w.publish(t, "wall=hidden")
	hidden.waitInfo(t, "hidden 0x18", func(i wire.LocalRelayInfo) bool { return !i.OnWall && i.WallCode == wallCode })
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if w.onWall(hidden.id) {
			t.Fatal("a wall=hidden broadcaster flashed onto the wall")
		}
		m, ok := outsider.next(t, 100*time.Millisecond)
		if ev, isEv := m.(wire.RoomEvent); ok && isEv && ev.Kind == wire.RoomEventAttachmentAdded && ev.Attachment.BroadcastID == hidden.id {
			t.Fatal("the outsider saw the hidden broadcaster's tile")
		}
	}
}

// Unlinked, party guests see the local wall: the PoP's own broadcasts as
// tiles, read-only.
func TestUnlinkedPartyGuestsSeeTheLocalWall(t *testing.T) {
	w := startWallFleet(t)
	p := w.publish(t, url.Values{"name": {"tuhis"}}.Encode())
	WaitFor(t, "the tile on the local wall", func() bool {
		for _, a := range w.pop.Rooms.Attachments(wallCode) {
			if a.BroadcastID == p.id {
				return true
			}
		}
		return false
	})
	w.home.Drain(t)
	WaitFor(t, "the PoP to notice the link is down", func() bool {
		return w.pop.Client.State().Header != popclient.HeaderLinked
	})
	guest := openRoom(t, w.f, "https://"+w.pop.Addr()+"/room/"+wallCode, "guest")
	st := guest.state(t)
	if st.Flags&wire.RoomStateFlagAttachOK != 0 {
		t.Fatalf("the local wall is not read-only: flags 0x%02x", st.Flags)
	}
	if len(st.Attachments) != 1 || st.Attachments[0].BroadcastID != p.id || st.Attachments[0].Label != "tuhis" {
		t.Fatalf("local wall tiles = %+v", st.Attachments)
	}
	// The broadcaster's 0x18 now says unlinked, wall code kept.
	p.waitInfo(t, "unlinked 0x18", func(i wire.LocalRelayInfo) bool { return !i.Linked && i.WallCode == wallCode })
}

// rawDialPublic dials a client route on a pod and returns a refusal's
// status.
func rawDialPublic(t *testing.T, f *HomeFleet, pod *HomePod, path string) (*webtransport.Session, int, string) {
	t.Helper()
	return rawDial(t, f, pod, path, nil)
}
