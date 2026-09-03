package roomsrv

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/rooms"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

var discardLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeBroadcasts is a BroadcastSource under test control.
type fakeBroadcasts struct {
	mu   sync.Mutex
	live map[string]BroadcastState
}

func (f *fakeBroadcasts) set(id string, st BroadcastState) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.live == nil {
		f.live = make(map[string]BroadcastState)
	}
	f.live[id] = st
}

func (f *fakeBroadcasts) del(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.live, id)
}

func (f *fakeBroadcasts) BroadcastState(id string) (BroadcastState, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	st, ok := f.live[strings.ToUpper(id)]
	return st, ok
}

// fakeTokens is an HMAC Tokens with a fixed key, mirroring the transport's
// construction closely enough that "a token for another ID" is wrong.
type fakeTokens struct{ key []byte }

func (t fakeTokens) mac(domain, s string) []byte {
	m := hmac.New(sha256.New, t.key)
	m.Write([]byte(domain))
	m.Write([]byte{0})
	m.Write([]byte(s))
	return m.Sum(nil)[:16]
}
func (t fakeTokens) MintCreator(code string) []byte { return t.mac("creator", code) }
func (t fakeTokens) VerifyCreator(code string, tok []byte) bool {
	return hmac.Equal(tok, t.mac("creator", code))
}
func (t fakeTokens) MintResume(id string) []byte { return t.mac("", id) }
func (t fakeTokens) VerifyResume(id string, tok []byte) bool {
	return hmac.Equal(tok, t.mac("", strings.ToUpper(id)))
}

// fakeConn records everything the registry writes and any close.
type fakeConn struct {
	mu      sync.Mutex
	records [][]byte
	closed  bool
	code    uint32
	reason  string
	ch      chan []byte
}

func newFakeConn() *fakeConn { return &fakeConn{ch: make(chan []byte, 1024)} }

func (c *fakeConn) Write(_ context.Context, rec []byte) error {
	c.mu.Lock()
	cp := append([]byte{}, rec...)
	c.records = append(c.records, cp)
	c.mu.Unlock()
	c.ch <- cp
	return nil
}

func (c *fakeConn) Close(code uint32, reason string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed, c.code, c.reason = true, code, reason
	}
}

func (c *fakeConn) closeCode() (uint32, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.code, c.closed
}

// next waits for the next record, unframes it and returns the parsed
// message as one of RoomState / RoomEvent.
func (c *fakeConn) next(t *testing.T) any {
	t.Helper()
	select {
	case rec := <-c.ch:
		n, err := wire.ParseRoomRecordLength(rec)
		if err != nil || n != len(rec)-2 {
			t.Fatalf("bad record framing: %v (len %d, framed %d)", err, n, len(rec)-2)
		}
		msg := rec[2:]
		_, typ, _ := wire.PeekType(msg)
		switch typ {
		case wire.TypeRoomState:
			s, err := wire.ParseRoomState(msg)
			if err != nil {
				t.Fatalf("ParseRoomState: %v", err)
			}
			return s
		case wire.TypeRoomEvent:
			e, err := wire.ParseRoomEvent(msg)
			if err != nil {
				t.Fatalf("ParseRoomEvent: %v", err)
			}
			return e
		}
		t.Fatalf("unexpected record type 0x%02x", typ)
	case <-time.After(5 * time.Second):
		t.Fatal("no record within 5s")
	}
	return nil
}

func (c *fakeConn) nextState(t *testing.T) wire.RoomState {
	t.Helper()
	s, ok := c.next(t).(wire.RoomState)
	if !ok {
		t.Fatal("expected a RoomState")
	}
	return s
}

func (c *fakeConn) nextEvent(t *testing.T, kind uint8) wire.RoomEvent {
	t.Helper()
	for {
		e, ok := c.next(t).(wire.RoomEvent)
		if !ok {
			t.Fatal("expected a RoomEvent")
		}
		if e.Kind == kind {
			return e
		}
	}
}

type fixture struct {
	reg    *Registry
	bc     *fakeBroadcasts
	tokens fakeTokens
}

func newFixture(t *testing.T, mutate func(*Options)) *fixture {
	t.Helper()
	bc := &fakeBroadcasts{}
	tok := fakeTokens{key: []byte("k")}
	opts := Options{Broadcasts: bc, Tokens: tok, Log: discardLog, EmptyGrace: time.Hour}
	if mutate != nil {
		mutate(&opts)
	}
	return &fixture{reg: NewRegistry(opts), bc: bc, tokens: tok}
}

func (f *fixture) mint(t *testing.T, id string) MintResult {
	t.Helper()
	f.bc.set(id, BroadcastState{Live: true, Viewers: 1})
	res, err := f.reg.Mint(context.Background(), MintRequest{BroadcastID: id, ResumeToken: f.tokens.MintResume(id), Label: "pc"})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	return res
}

func (f *fixture) join(t *testing.T, code, nick string, g Grants, creatorToken []byte) (*Participant, *fakeConn) {
	t.Helper()
	c := newFakeConn()
	p, err := f.reg.Join(code, wire.RoomHello{Protocol: 1, Nickname: nick}, g, creatorToken, c)
	if err != nil {
		t.Fatalf("Join(%s): %v", code, err)
	}
	return p, c
}

func TestMintCreatesDynamicRoomWithBroadcastAttached(t *testing.T) {
	f := newFixture(t, nil)
	res := f.mint(t, "ABCDEF")
	if !rooms.IsDynamicShape(res.Code) || len(res.CreatorToken) != wire.RoomCreatorTokenSize {
		t.Fatalf("mint = %+v", res)
	}
	if !f.reg.Has(res.Code) || !f.reg.Has(strings.ToUpper(res.Code)) {
		t.Fatal("Has() does not see the minted room by either spelling")
	}
	p, c := f.join(t, res.Code, "tuhis", Grants{Creator: true, AttachOK: true}, res.CreatorToken)
	st := c.nextState(t)
	if st.Flags != wire.RoomStateFlagDynamic|wire.RoomStateFlagCreator|wire.RoomStateFlagAttachOK {
		t.Errorf("flags = 0x%02x", st.Flags)
	}
	if !bytes.Equal(st.CreatorToken, res.CreatorToken) {
		t.Error("first RoomState after mint does not carry the creator token")
	}
	if st.Code != res.Display || st.YourID != p.ID() {
		t.Errorf("state = %+v", st)
	}
	if len(st.Attachments) != 1 || st.Attachments[0].BroadcastID != "ABCDEF" || st.Attachments[0].Label != "pc" || !st.Attachments[0].Live {
		t.Errorf("attachments = %+v", st.Attachments)
	}
	if len(st.Participants) != 1 || st.Participants[0].Nickname != "tuhis" {
		t.Errorf("participants = %+v", st.Participants)
	}
	// A resync repeats the snapshot without the token.
	p.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandResync})
	if again := c.nextState(t); again.CreatorToken != nil {
		t.Error("resync leaked the creator token again")
	}
}

// docs/44 §4.2 / RM2: minting never yields a code that names a live
// broadcast, and a broadcast ID that names a live room is refused to the
// hub (Has). Both directions of D3.
func TestMintIsDisjointFromLiveBroadcasts(t *testing.T) {
	f := newFixture(t, nil)
	// Make EVERY minted code collide with a live broadcast: the fake says
	// every ID is live. Mint must give up rather than hand out a taken code.
	all := &allLive{}
	f.reg.opts.Broadcasts = all
	_, err := f.reg.Mint(context.Background(), MintRequest{BroadcastID: "ABCDEF", ResumeToken: f.tokens.MintResume("ABCDEF")})
	if !errors.Is(err, errCollision) {
		t.Fatalf("mint against an all-live fleet: %v, want collision exhaustion", err)
	}
	// And the other way round: a live room reserves its code from the hub.
	f.reg.opts.Broadcasts = f.bc
	res := f.mint(t, "ABCDEF")
	if !f.reg.Has(res.Display) {
		t.Fatal("the hub would be allowed to mint a broadcast named like a live room")
	}
	if f.reg.Has("ZZZZZZ") {
		t.Fatal("Has() claims an unknown code")
	}
}

type allLive struct{}

func (allLive) BroadcastState(string) (BroadcastState, bool) {
	return BroadcastState{Live: true}, true
}

func TestMintGates(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.CreateSecret = "s3cret"; o.MaxRooms = 1 })
	ctx := context.Background()
	f.bc.set("ABCDEF", BroadcastState{Live: true})
	tok := f.tokens.MintResume("ABCDEF")
	if _, err := f.reg.Mint(ctx, MintRequest{BroadcastID: "ABCDEF", ResumeToken: tok}); !errors.Is(err, ErrForbidden) {
		t.Errorf("missing create secret: %v", err)
	}
	if _, err := f.reg.Mint(ctx, MintRequest{BroadcastID: "ABCDEF", ResumeToken: f.tokens.MintResume("OTHER1"), CreateSecret: "s3cret"}); !errors.Is(err, ErrForbidden) {
		t.Errorf("wrong resume token: %v", err)
	}
	if _, err := f.reg.Mint(ctx, MintRequest{BroadcastID: "ZZZZZZ", ResumeToken: f.tokens.MintResume("ZZZZZZ"), CreateSecret: "s3cret"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown broadcast: %v", err)
	}
	res, err := f.reg.Mint(ctx, MintRequest{BroadcastID: "ABCDEF", ResumeToken: tok, CreateSecret: "s3cret"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := f.reg.Mint(ctx, MintRequest{BroadcastID: "ABCDEF", ResumeToken: tok, CreateSecret: "s3cret"}); !errors.Is(err, ErrAlreadyAttached) {
		t.Errorf("second room for the same broadcast: %v", err)
	}
	f.bc.set("GHJKMN", BroadcastState{Live: true})
	if _, err := f.reg.Mint(ctx, MintRequest{BroadcastID: "GHJKMN", ResumeToken: f.tokens.MintResume("GHJKMN"), CreateSecret: "s3cret"}); !errors.Is(err, ErrMaxRooms) {
		t.Errorf("max rooms: %v", err)
	}
	f.reg.EndRoom(res.Code, wire.RoomEndReasonCreator)
	if _, err := f.reg.Mint(ctx, MintRequest{BroadcastID: "GHJKMN", ResumeToken: f.tokens.MintResume("GHJKMN"), CreateSecret: "s3cret"}); err != nil {
		t.Errorf("mint after the first room ended: %v", err)
	}
}

func TestAttachRequiresProofAndGrant(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.MaxBroadcasts = 2 })
	res := f.mint(t, "ABCDEF")
	_, viewer := f.join(t, res.Code, "v", Grants{AttachOK: true}, nil)
	viewer.nextState(t)
	p, c := f.join(t, res.Code, "second", Grants{AttachOK: true}, nil)
	c.nextState(t)
	viewer.nextEvent(t, wire.RoomEventParticipantJoined)

	f.bc.set("GHJKMN", BroadcastState{Live: true, Viewers: 2})
	// Wrong token → BadProof, and nothing attached.
	p.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandAttach, BroadcastID: "GHJKMN", ResumeToken: f.tokens.MintResume("ABCDEF"), Label: "x"})
	if e := c.nextEvent(t, wire.RoomEventCommandRejected); e.Reason != wire.RoomRejectBadProof || e.Command != wire.RoomCommandAttach {
		t.Fatalf("wrong token: %+v", e)
	}
	// Unknown broadcast → NotFound.
	p.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandAttach, BroadcastID: "PQRSTU", ResumeToken: f.tokens.MintResume("PQRSTU")})
	if e := c.nextEvent(t, wire.RoomEventCommandRejected); e.Reason != wire.RoomRejectNotFound {
		t.Fatalf("unknown broadcast: %+v", e)
	}
	// Right token → attached, everyone told, attacher flagged streaming.
	p.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandAttach, BroadcastID: "ghjkmn", ResumeToken: f.tokens.MintResume("GHJKMN"), Label: "laptop"})
	if e := viewer.nextEvent(t, wire.RoomEventAttachmentAdded); e.Attachment.BroadcastID != "GHJKMN" || e.Attachment.Label != "laptop" || e.Attachment.ViewerCount != 2 {
		t.Fatalf("attachment added: %+v", e)
	}
	if e := viewer.nextEvent(t, wire.RoomEventParticipantUpdated); e.Participant.ID != p.ID() || e.Participant.Flags&wire.RoomParticipantFlagStreaming == 0 {
		t.Fatalf("attacher not flagged streaming: %+v", e)
	}
	// Limit (2) reached.
	f.bc.set("VWXYZ2", BroadcastState{Live: true})
	p.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandAttach, BroadcastID: "VWXYZ2", ResumeToken: f.tokens.MintResume("VWXYZ2")})
	if e := c.nextEvent(t, wire.RoomEventCommandRejected); e.Reason != wire.RoomRejectLimit {
		t.Fatalf("limit: %+v", e)
	}
	// A participant without the attach grant is refused before any proof
	// is looked at.
	q, qc := f.join(t, res.Code, "nogrant", Grants{}, nil)
	qc.nextState(t)
	q.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandAttach, BroadcastID: "VWXYZ2", ResumeToken: f.tokens.MintResume("VWXYZ2")})
	if e := qc.nextEvent(t, wire.RoomEventCommandRejected); e.Reason != wire.RoomRejectForbidden {
		t.Fatalf("no grant: %+v", e)
	}
	// Attaching to a second room is refused (D1).
	f.bc.set("QRSTUV", BroadcastState{Live: true})
	res2, err := f.reg.Mint(context.Background(), MintRequest{BroadcastID: "QRSTUV", ResumeToken: f.tokens.MintResume("QRSTUV")})
	if err != nil {
		t.Fatal(err)
	}
	o, oc := f.join(t, res2.Code, "other", Grants{AttachOK: true}, nil)
	oc.nextState(t)
	o.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandAttach, BroadcastID: "GHJKMN", ResumeToken: f.tokens.MintResume("GHJKMN")})
	if e := oc.nextEvent(t, wire.RoomEventCommandRejected); e.Reason != wire.RoomRejectAlreadyAttached {
		t.Fatalf("already attached: %+v", e)
	}
}

func TestDetachRules(t *testing.T) {
	f := newFixture(t, nil)
	res := f.mint(t, "ABCDEF")
	creator, cc := f.join(t, res.Code, "c", Grants{Creator: true, AttachOK: true}, res.CreatorToken)
	cc.nextState(t)
	p, pc := f.join(t, res.Code, "p", Grants{AttachOK: true}, nil)
	pc.nextState(t)
	cc.nextEvent(t, wire.RoomEventParticipantJoined)
	f.bc.set("GHJKMN", BroadcastState{Live: true})
	p.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandAttach, BroadcastID: "GHJKMN", ResumeToken: f.tokens.MintResume("GHJKMN")})
	cc.nextEvent(t, wire.RoomEventAttachmentAdded)
	pc.nextEvent(t, wire.RoomEventAttachmentAdded)

	// A stranger may not detach what it did not attach.
	q, qc := f.join(t, res.Code, "q", Grants{AttachOK: true}, nil)
	qc.nextState(t)
	q.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandDetach, BroadcastID: "GHJKMN"})
	if e := qc.nextEvent(t, wire.RoomEventCommandRejected); e.Reason != wire.RoomRejectForbidden {
		t.Fatalf("stranger detach: %+v", e)
	}
	q.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandDetach, BroadcastID: "ZZZZZZ"})
	if e := qc.nextEvent(t, wire.RoomEventCommandRejected); e.Reason != wire.RoomRejectNotFound {
		t.Fatalf("detach unknown: %+v", e)
	}
	// The creator may detach anyone (reason creator).
	creator.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandDetach, BroadcastID: "GHJKMN"})
	if e := pc.nextEvent(t, wire.RoomEventAttachmentRemoved); e.Attachment.BroadcastID != "GHJKMN" || e.Reason != wire.RoomDetachReasonCreator {
		t.Fatalf("creator detach: %+v", e)
	}
	if e := pc.nextEvent(t, wire.RoomEventParticipantUpdated); e.Participant.ID != p.ID() || e.Participant.Flags&wire.RoomParticipantFlagStreaming != 0 {
		t.Fatalf("attacher still streaming: %+v", e)
	}
	// The broadcast is free to attach elsewhere now.
	if _, taken := f.reg.attached["GHJKMN"]; taken {
		t.Fatal("detached broadcast still reserved")
	}
	// The attacher may detach its own (reason publisher).
	p.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandAttach, BroadcastID: "GHJKMN", ResumeToken: f.tokens.MintResume("GHJKMN")})
	pc.nextEvent(t, wire.RoomEventAttachmentAdded)
	p.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandDetach, BroadcastID: "GHJKMN"})
	if e := pc.nextEvent(t, wire.RoomEventAttachmentRemoved); e.Reason != wire.RoomDetachReasonPublisher {
		t.Fatalf("own detach: %+v", e)
	}
	// Only the creator ends a dynamic room.
	p.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandEndRoom})
	if e := pc.nextEvent(t, wire.RoomEventCommandRejected); e.Reason != wire.RoomRejectForbidden {
		t.Fatalf("non-creator end: %+v", e)
	}
	creator.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandEndRoom})
	if e := pc.nextEvent(t, wire.RoomEventRoomEnding); e.Reason != wire.RoomEndReasonCreator {
		t.Fatalf("ending: %+v", e)
	}
	waitFor(t, func() bool { code, closed := pc.closeCode(); return closed && code == wire.CloseCodeRoomEnded }, "4007 to the participant")
	waitFor(t, func() bool { code, closed := cc.closeCode(); return closed && code == wire.CloseCodeRoomEnded }, "4007 to the creator")
	if f.reg.Has(res.Code) {
		t.Fatal("ended room still registered")
	}
	// The broadcasts themselves are untouched (D1): the fake still has them.
	if _, ok := f.bc.BroadcastState("ABCDEF"); !ok {
		t.Fatal("ending the room touched the broadcast")
	}
}

// docs/44 D7 / RM2: the empty grace survives a reconnect shorter than it,
// and expires a room nobody comes back to. Uses a real (short) timer.
func TestEmptyGraceSurvivesAReconnectShorterThanIt(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.EmptyGrace = 150 * time.Millisecond })
	res := f.mint(t, "ABCDEF")
	p, c := f.join(t, res.Code, "a", Grants{AttachOK: true}, nil)
	c.nextState(t)
	p.Leave()
	// Reconnect well inside the grace.
	time.Sleep(40 * time.Millisecond)
	p2, c2 := f.join(t, res.Code, "a", Grants{AttachOK: true}, nil)
	st := c2.nextState(t)
	if len(st.Attachments) != 1 {
		t.Fatalf("attachment lost across the reconnect: %+v", st)
	}
	time.Sleep(200 * time.Millisecond)
	if !f.reg.Has(res.Code) {
		t.Fatal("room ended while a participant was present")
	}
	p2.Leave()
	waitFor(t, func() bool { return !f.reg.Has(res.Code) }, "room to end after the grace")
	if _, taken := f.reg.attached["ABCDEF"]; taken {
		t.Fatal("ended room still holds its attachment")
	}
}

func TestMintWithoutAJoinIsReclaimed(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.EmptyGrace = 50 * time.Millisecond })
	res := f.mint(t, "ABCDEF")
	waitFor(t, func() bool { return !f.reg.Has(res.Code) }, "unjoined room to end")
}

func TestStaticRoomsNeverEndAndGateAttachBySecret(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.EmptyGrace = 30 * time.Millisecond })
	if err := f.reg.UpsertStatic(StaticRoom{Code: "TuhisRoom", DisplayName: "Tuhis' room", AttachSecret: "key"}); err != nil {
		t.Fatal(err)
	}
	// Unknown code, wrong secret, right secret, no secret (viewer).
	if _, err := f.reg.CheckJoin("nope", "", "", false); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
	if _, err := f.reg.CheckJoin("tuhisroom", "", "wrong", true); !errors.Is(err, ErrForbidden) {
		t.Errorf("wrong secret: %v", err)
	}
	g, err := f.reg.CheckJoin("TUHISROOM", "", "key", true)
	if err != nil || !g.AttachOK || g.Creator {
		t.Errorf("right secret: %+v, %v", g, err)
	}
	g, err = f.reg.CheckJoin("TuhisRoom", "", "", false)
	if err != nil || g.AttachOK {
		t.Errorf("viewer without secret: %+v, %v", g, err)
	}
	// A bogus creator token is refused even on a static room.
	if _, err := f.reg.CheckJoin("TuhisRoom", hex.EncodeToString(make([]byte, 16)), "", false); !errors.Is(err, ErrForbidden) {
		t.Errorf("bogus creator token: %v", err)
	}
	p, c := f.join(t, "TuhisRoom", "v", Grants{}, nil)
	st := c.nextState(t)
	if st.Flags&wire.RoomStateFlagDynamic != 0 || st.Code != "TuhisRoom" || st.DisplayName != "Tuhis' room" {
		t.Errorf("static state = %+v", st)
	}
	// Ending a static room by anyone is refused; it never ends on its own.
	p.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandEndRoom})
	if e := c.nextEvent(t, wire.RoomEventCommandRejected); e.Reason != wire.RoomRejectForbidden {
		t.Fatalf("end static: %+v", e)
	}
	p.Leave()
	time.Sleep(80 * time.Millisecond)
	if !f.reg.Has("tuhisroom") {
		t.Fatal("static room ended after emptying")
	}
	// Removing it from the source ends it fleet-wide with reason operator.
	q, qc := f.join(t, "TuhisRoom", "v", Grants{}, nil)
	qc.nextState(t)
	_ = q
	f.reg.ReplaceStatic(nil)
	if e := qc.nextEvent(t, wire.RoomEventRoomEnding); e.Reason != wire.RoomEndReasonOperator {
		t.Fatalf("operator end: %+v", e)
	}
	waitFor(t, func() bool { _, closed := qc.closeCode(); return closed }, "close after operator end")
	// A dynamic room must not be clobbered by a static definition.
	res := f.mint(t, "ABCDEF")
	if err := f.reg.UpsertStatic(StaticRoom{Code: res.Code}); err == nil {
		t.Fatal("a static definition overwrote a dynamic room")
	}
}

func TestParticipantLimitAndNicknames(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.MaxParticipants = 2 })
	res := f.mint(t, "ABCDEF")
	a, ac := f.join(t, res.Code, "tuhis", Grants{}, nil)
	ac.nextState(t)
	b, bc := f.join(t, res.Code, "tuhis", Grants{}, nil)
	st := bc.nextState(t)
	if st.Participants[1].Nickname != "tuhis (2)" {
		t.Errorf("collision suffix: %+v", st.Participants)
	}
	if _, err := f.reg.CheckJoin(res.Code, "", "", false); !errors.Is(err, ErrFull) {
		t.Errorf("CheckJoin at limit: %v", err)
	}
	if _, err := f.reg.Join(res.Code, wire.RoomHello{Protocol: 1}, Grants{}, nil, newFakeConn()); !errors.Is(err, ErrFull) {
		t.Errorf("Join at limit: %v", err)
	}
	// Empty and over-long nicknames are made safe.
	b.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandSetNickname, Nickname: ""})
	if e := ac.nextEvent(t, wire.RoomEventParticipantUpdated); !strings.HasPrefix(e.Participant.Nickname, "guest-") {
		t.Errorf("empty nick → %q", e.Participant.Nickname)
	}
	long := strings.Repeat("é", 40)
	b.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandSetNickname, Nickname: long})
	if e := ac.nextEvent(t, wire.RoomEventParticipantUpdated); len(e.Participant.Nickname) > wire.MaxRoomNicknameLen || !strings.HasPrefix(long, e.Participant.Nickname) {
		t.Errorf("long nick → %q", e.Participant.Nickname)
	}
	a.Leave()
	if e := bc.nextEvent(t, wire.RoomEventParticipantLeft); e.Participant.ID != a.ID() {
		t.Errorf("left: %+v", e)
	}
	// Unsupported (reserved) command kinds are rejected, never fatal.
	b.HandleCommand(wire.RoomCommand{Kind: 0x40})
	if e := bc.nextEvent(t, wire.RoomEventCommandRejected); e.Reason != wire.RoomRejectUnsupported || e.Command != 0x40 {
		t.Errorf("reserved command: %+v", e)
	}
}

func TestBroadcastLifecycleHooks(t *testing.T) {
	f := newFixture(t, nil)
	res := f.mint(t, "ABCDEF")
	_, c := f.join(t, res.Code, "v", Grants{}, nil)
	c.nextState(t)
	// Publisher away → attachment away; a poll catches the viewer count.
	f.reg.PublisherClosed("abcdef")
	if e := c.nextEvent(t, wire.RoomEventAttachmentUpdated); e.Attachment.Live {
		t.Fatalf("away not reflected: %+v", e)
	}
	f.bc.set("ABCDEF", BroadcastState{Live: true, Viewers: 7})
	f.reg.Refresh()
	if e := c.nextEvent(t, wire.RoomEventAttachmentUpdated); !e.Attachment.Live || e.Attachment.ViewerCount != 7 {
		t.Fatalf("refresh: %+v", e)
	}
	f.reg.Refresh() // no change → no event (the next record must be the expiry)
	f.bc.del("ABCDEF")
	f.reg.BroadcastExpired("ABCDEF")
	if e := c.nextEvent(t, wire.RoomEventAttachmentRemoved); e.Reason != wire.RoomDetachReasonExpired {
		t.Fatalf("expired: %+v", e)
	}
	if _, taken := f.reg.attached["ABCDEF"]; taken {
		t.Fatal("expired broadcast still reserved")
	}
	// Hooks for unknown broadcasts are no-ops.
	f.reg.PublisherClosed("ZZZZZZ")
	f.reg.BroadcastExpired("ZZZZZZ")
}

func TestSeqIsMonotonicAndStatsAreKeyedByHMAC(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.Obfuscate = func(s string) string { return "key-" + s } })
	res := f.mint(t, "ABCDEF")
	_, c := f.join(t, res.Code, "v", Grants{}, nil)
	st := c.nextState(t)
	_, c2 := f.join(t, res.Code, "w", Grants{}, nil)
	c2.nextState(t)
	e := c.nextEvent(t, wire.RoomEventParticipantJoined)
	if e.Seq != st.Seq+1 {
		t.Fatalf("seq: state %d then event %d", st.Seq, e.Seq)
	}
	stats := f.reg.Stats()
	row, ok := stats["key-"+res.Code]
	if !ok {
		t.Fatalf("stats keyed raw: %v", stats)
	}
	if row.Kind != rooms.KindDynamic || row.Participants != 2 || row.Attachments != 1 || row.Role != "home" {
		t.Errorf("row = %+v", row)
	}
	tot := f.reg.TotalStats()
	if tot.Dynamic != 1 || tot.Participants != 2 || tot.Attachments != 1 {
		t.Errorf("totals = %+v", tot)
	}
}

func TestUnresponsiveParticipantIsEvicted(t *testing.T) {
	f := newFixture(t, nil)
	res := f.mint(t, "ABCDEF")
	stuck := &stuckConn{block: make(chan struct{})}
	p, err := f.reg.Join(res.Code, wire.RoomHello{Protocol: 1, Nickname: "stuck"}, Grants{}, nil, stuck)
	if err != nil {
		t.Fatal(err)
	}
	defer close(stuck.block)
	// Flood the outbox past its depth from a participant that never reads.
	for i := 0; i < outboxDepth+8; i++ {
		p.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandSetNickname, Nickname: "n"})
	}
	waitFor(t, func() bool {
		code, closed := stuck.closeCode()
		return closed && code == wire.CloseCodeSubscriberUnresponsive
	}, "4001 eviction")
}

type stuckConn struct {
	mu     sync.Mutex
	block  chan struct{}
	closed bool
	code   uint32
}

func (s *stuckConn) Write(context.Context, []byte) error { <-s.block; return nil }
func (s *stuckConn) Close(code uint32, _ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed, s.code = true, code
	}
}
func (s *stuckConn) closeCode() (uint32, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.code, s.closed
}

func TestAdoptDynamicRebuildsAttachments(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.EmptyGrace = 40 * time.Millisecond })
	f.bc.set("ABCDEF", BroadcastState{Live: true, Viewers: 3})
	cr := &rooms.Room{Spec: rooms.RoomSpec{Kind: rooms.KindDynamic}}
	cr.Name = "5up4xw"
	cr.Status.CreatorTokenFingerprint = "fp"
	cr.Status.Attachments = []rooms.Attachment{{BroadcastID: "ABCDEF", Label: "pc"}, {BroadcastID: "0OIL11"}}
	if !f.reg.AdoptDynamic(cr) {
		t.Fatal("adopt refused")
	}
	if f.reg.AdoptDynamic(cr) {
		t.Fatal("double adopt accepted")
	}
	_, c := f.join(t, "5UP4XW", "v", Grants{}, nil)
	st := c.nextState(t)
	if st.Code != "5UP4XW" || len(st.Attachments) != 1 || st.Attachments[0].Label != "pc" || st.Attachments[0].ViewerCount != 3 {
		t.Fatalf("adopted state = %+v", st)
	}
	if got := f.reg.Attachments("5up4xw"); len(got) != 1 || got[0].BroadcastID != "ABCDEF" {
		t.Fatalf("Attachments = %+v", got)
	}
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
