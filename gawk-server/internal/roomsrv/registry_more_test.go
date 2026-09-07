package roomsrv

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/rooms"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// Lookup is the transport's pre-upgrade view of a room (any spelling of
// the code) and Nickname the roster name a session actually got, suffix
// included (docs/44 D10).
func TestLookupAndNicknameReflectTheRoom(t *testing.T) {
	f := newFixture(t, nil)
	if _, ok := f.reg.Lookup("nope"); ok {
		t.Fatal("Lookup found an unknown room")
	}
	if _, ok := f.reg.Lookup("!!"); ok {
		t.Fatal("Lookup accepted an invalid code")
	}
	if err := f.reg.UpsertStatic(StaticRoom{Code: "TuhisRoom", AttachSecret: "key"}); err != nil {
		t.Fatal(err)
	}
	info, ok := f.reg.Lookup(" tuhisROOM ")
	if !ok || info.Code != "tuhisroom" || info.Kind != rooms.KindStatic || !info.HasSecret || info.Participants != 0 || info.Attachments != 0 {
		t.Fatalf("Lookup = %+v, %v", info, ok)
	}
	a, ac := f.join(t, "TuhisRoom", "tuhis", Grants{}, nil)
	ac.nextState(t)
	b, bc := f.join(t, "TuhisRoom", "tuhis", Grants{}, nil)
	bc.nextState(t)
	if a.Nickname() != "tuhis" || b.Nickname() != "tuhis (2)" {
		t.Fatalf("nicknames = %q, %q", a.Nickname(), b.Nickname())
	}
	b.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandSetNickname, Nickname: "other"})
	ac.nextEvent(t, wire.RoomEventParticipantUpdated)
	if b.Nickname() != "other" {
		t.Fatalf("after rename: %q", b.Nickname())
	}
	if info, _ := f.reg.Lookup("tuhisroom"); info.Participants != 2 {
		t.Fatalf("participants = %d, want 2", info.Participants)
	}
	f.bc.set("ABCDEF", BroadcastState{Live: true})
	res := f.mint(t, "ABCDEF")
	if info, ok := f.reg.Lookup(strings.ToUpper(res.Code)); !ok || info.Kind != rooms.KindDynamic || info.HasSecret || info.Attachments != 1 {
		t.Fatalf("dynamic Lookup = %+v, %v", info, ok)
	}
}

// The transport owns the resume-token key, so credentials arrive after
// construction: until SetTokens, every proof fails closed (mint and
// creator token alike); after it, the same requests pass.
func TestSetTokensInstallsCredentialsAfterConstruction(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.Tokens = nil })
	f.bc.set("ABCDEF", BroadcastState{Live: true})
	req := MintRequest{BroadcastID: "ABCDEF", ResumeToken: f.tokens.MintResume("ABCDEF")}
	if _, err := f.reg.Mint(context.Background(), req); !errors.Is(err, ErrForbidden) {
		t.Fatalf("mint without tokens: %v, want forbidden", err)
	}
	if err := f.reg.UpsertStatic(StaticRoom{Code: "TuhisRoom"}); err != nil {
		t.Fatal(err)
	}
	tok := hex.EncodeToString(f.tokens.MintCreator("tuhisroom"))
	if _, err := f.reg.CheckJoin("TuhisRoom", tok, "", false); !errors.Is(err, ErrForbidden) {
		t.Fatalf("creator token without tokens: %v, want forbidden", err)
	}
	f.reg.SetTokens(f.tokens)
	res, err := f.reg.Mint(context.Background(), req)
	if err != nil {
		t.Fatalf("mint after SetTokens: %v", err)
	}
	if g, err := f.reg.CheckJoin(res.Code, hex.EncodeToString(res.CreatorToken), "", false); err != nil || !g.Creator {
		t.Fatalf("creator join after SetTokens: %+v, %v", g, err)
	}
}

// The creator token crosses as a query string, so both hex cases must
// verify and anything that is not 32 hex digits is a forbidden token, not
// a panic or a pass (unhex / decodeHex).
func TestCreatorTokenHexDecoding(t *testing.T) {
	f := newFixture(t, nil)
	res := f.mint(t, "ABCDEF")
	lower := hex.EncodeToString(res.CreatorToken)
	cases := []struct {
		name string
		tok  string
		ok   bool
	}{
		{name: "lowercase", tok: lower, ok: true},
		{name: "uppercase", tok: strings.ToUpper(lower), ok: true},
		{name: "non-hex of the right length", tok: strings.Repeat("zz", wire.RoomCreatorTokenSize)},
		{name: "too short", tok: lower[:30]},
		{name: "too long", tok: lower + "00"},
		{name: "wrong room's token", tok: hex.EncodeToString(f.tokens.MintCreator("other1"))},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g, err := f.reg.CheckJoin(res.Code, tc.tok, "", false)
			if tc.ok && (err != nil || !g.Creator) {
				t.Fatalf("CheckJoin = %+v, %v; want creator", g, err)
			}
			if !tc.ok && !errors.Is(err, ErrForbidden) {
				t.Fatalf("CheckJoin = %+v, %v; want forbidden", g, err)
			}
		})
	}
}

// RunRefresh is the poll that catches viewer-count changes the hooks do
// not carry; it must push a delta on its interval and stop with its
// context.
func TestRunRefreshPushesDeltasUntilCancelled(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.RefreshInterval = 10 * time.Millisecond })
	res := f.mint(t, "ABCDEF")
	_, c := f.join(t, res.Code, "v", Grants{}, nil)
	c.nextState(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.reg.RunRefresh(ctx); close(done) }()
	f.bc.set("ABCDEF", BroadcastState{Live: true, Viewers: 5})
	if e := c.nextEvent(t, wire.RoomEventAttachmentUpdated); e.Attachment.ViewerCount != 5 || !e.Attachment.Live {
		t.Fatalf("refresh delta = %+v", e)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RunRefresh did not return after cancel")
	}
}

// blockedConn stalls on its first write (after signalling it started) and
// records every close code it receives.
type blockedConn struct {
	mu      sync.Mutex
	started chan struct{}
	once    sync.Once
	block   chan struct{}
	codes   []uint32
}

func (b *blockedConn) Write(context.Context, []byte) error {
	b.once.Do(func() { close(b.started) })
	<-b.block
	return nil
}

func (b *blockedConn) Close(code uint32, _ string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.codes = append(b.codes, code)
}

func (b *blockedConn) closeCodes() []uint32 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]uint32(nil), b.codes...)
}

// closeAfterDrain's other branch: a participant whose outbox is exactly
// full when the room ends cannot take the drain sentinel, and its writer is
// stuck behind the queue — so the close must not wait on the writer. It
// closes with 4007 straight away and the participant is gone, with no
// overflow eviction (4001) in between.
func TestCloseAfterDrainClosesImmediatelyWhenTheOutboxIsFull(t *testing.T) {
	f := newFixture(t, nil)
	res := f.mint(t, "ABCDEF")
	conn := &blockedConn{started: make(chan struct{}), block: make(chan struct{})}
	defer close(conn.block)
	p, err := f.reg.Join(res.Code, wire.RoomHello{Protocol: 1, Nickname: "stuck"}, Grants{}, nil, conn)
	if err != nil {
		t.Fatal(err)
	}
	// The writer has dequeued the initial RoomState and is stuck writing
	// it; the outbox is empty. Fill it to one short of its depth, so the
	// RoomEnding event is the record that fills it.
	<-conn.started
	for i := 0; i < outboxDepth-1; i++ {
		p.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandSetNickname, Nickname: "n"})
	}
	if n := len(p.outbox); n != outboxDepth-1 {
		t.Fatalf("outbox holds %d, want %d", n, outboxDepth-1)
	}
	f.reg.EndRoom(res.Code, wire.RoomEndReasonCreator)
	waitFor(t, func() bool { return len(conn.closeCodes()) > 0 }, "the close")
	if codes := conn.closeCodes(); len(codes) != 1 || codes[0] != wire.CloseCodeRoomEnded {
		t.Fatalf("close codes = %v, want exactly [4007]", codes)
	}
	select {
	case <-p.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("participant not marked gone after the immediate close")
	}
}

// attachmentsLog records every OnAttachmentsChanged delivery.
type attachmentsLog struct {
	mu    sync.Mutex
	calls []struct {
		code string
		ids  []string
	}
}

func (l *attachmentsLog) hook(code string, list []rooms.Attachment) {
	l.mu.Lock()
	defer l.mu.Unlock()
	ids := make([]string, 0, len(list))
	for _, a := range list {
		ids = append(ids, a.BroadcastID+"/"+a.Label)
	}
	l.calls = append(l.calls, struct {
		code string
		ids  []string
	}{code, ids})
}

func (l *attachmentsLog) last(t *testing.T, wantCode string, wantIDs ...string) {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.calls) == 0 {
		t.Fatal("no OnAttachmentsChanged delivery")
	}
	got := l.calls[len(l.calls)-1]
	if got.code != wantCode || strings.Join(got.ids, ",") != strings.Join(wantIDs, ",") {
		t.Fatalf("last delivery = %s %v, want %s %v", got.code, got.ids, wantCode, wantIDs)
	}
}

func (l *attachmentsLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.calls)
}

// The cluster seam (RM3): every change to a room's attachment list is
// handed to the store as the whole current list, in attach order, with
// the labels — mint, attach, idempotent re-attach, detach and expiry —
// and a room that has ended hands it nothing (the CR is being deleted).
func TestNotifyAttachmentsHandsTheStoreTheCurrentList(t *testing.T) {
	var log attachmentsLog
	f := newFixture(t, func(o *Options) { o.OnAttachmentsChanged = log.hook })
	res := f.mint(t, "ABCDEF")
	log.last(t, res.Code, "ABCDEF/pc")

	p, c := f.join(t, res.Code, "b", Grants{Creator: true, AttachOK: true}, res.CreatorToken)
	c.nextState(t)
	f.bc.set("GHJKMN", BroadcastState{Live: true})
	p.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandAttach, BroadcastID: "GHJKMN", ResumeToken: f.tokens.MintResume("GHJKMN"), Label: "laptop"})
	c.nextEvent(t, wire.RoomEventAttachmentAdded)
	log.last(t, res.Code, "ABCDEF/pc", "GHJKMN/laptop")

	// A re-attach (reconnected broadcaster) refreshes the label and is
	// reported again.
	before := log.count()
	p.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandAttach, BroadcastID: "GHJKMN", ResumeToken: f.tokens.MintResume("GHJKMN"), Label: "laptop2"})
	c.nextEvent(t, wire.RoomEventAttachmentUpdated)
	if log.count() != before+1 {
		t.Fatalf("re-attach delivered %d times, want 1", log.count()-before)
	}
	log.last(t, res.Code, "ABCDEF/pc", "GHJKMN/laptop2")

	p.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandDetach, BroadcastID: "GHJKMN"})
	c.nextEvent(t, wire.RoomEventAttachmentRemoved)
	log.last(t, res.Code, "ABCDEF/pc")

	f.bc.del("ABCDEF")
	f.reg.BroadcastExpired("ABCDEF")
	c.nextEvent(t, wire.RoomEventAttachmentRemoved)
	log.last(t, res.Code)

	// Nothing for an ended room, even when a hook races the end.
	f.reg.mu.Lock()
	rm := f.reg.rooms[res.Code]
	f.reg.mu.Unlock()
	f.reg.EndRoom(res.Code, wire.RoomEndReasonCreator)
	n := log.count()
	f.reg.notifyAttachments(rm)
	if log.count() != n {
		t.Fatal("an ended room delivered an attachment list")
	}
}

// Static definitions: a malformed code is refused (and skipped, never
// fatal, by ReplaceStatic), and a re-definition updates the room in place
// — the next join sees the new name and the secret requirement follows
// the definition.
func TestUpsertStaticRejectsBadCodesAndUpdatesInPlace(t *testing.T) {
	f := newFixture(t, nil)
	if err := f.reg.UpsertStatic(StaticRoom{Code: "-bad-"}); !errors.Is(err, rooms.ErrInvalidCode) {
		t.Fatalf("bad code: %v", err)
	}
	if err := f.reg.UpsertStatic(StaticRoom{Code: "TuhisRoom", DisplayName: "Old", AttachSecret: "key"}); err != nil {
		t.Fatal(err)
	}
	if err := f.reg.UpsertStatic(StaticRoom{Code: "tuhisroom", DisplayCode: "TUHISroom", DisplayName: "New", MaxBroadcasts: 1}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if info, _ := f.reg.Lookup("tuhisroom"); info.HasSecret {
		t.Fatal("the dropped attach secret is still required")
	}
	if g, err := f.reg.CheckJoin("tuhisroom", "", "", false); err != nil || !g.AttachOK {
		t.Fatalf("join without the dropped secret: %+v, %v", g, err)
	}
	_, c := f.join(t, "TuhisRoom", "v", Grants{}, nil)
	if st := c.nextState(t); st.DisplayName != "New" || st.Code != "TUHISroom" {
		t.Fatalf("state after update = %+v", st)
	}
	if n := f.reg.TotalStats().Static; n != 1 {
		t.Fatalf("static rooms = %d, want 1 (updated, not duplicated)", n)
	}

	f.reg.ReplaceStatic([]rooms.FileRoom{{Code: "!!!"}, {Code: "TuhisRoom", DisplayName: "Again"}, {Code: "second-room"}})
	if tot := f.reg.TotalStats(); tot.Static != 2 {
		t.Fatalf("after ReplaceStatic: %+v, want the two valid rooms", tot)
	}
	if !f.reg.Has("tuhisroom") || !f.reg.Has("second-room") {
		t.Fatal("ReplaceStatic lost a listed room")
	}
	_, c2 := f.join(t, "TuhisRoom", "w", Grants{}, nil)
	if st := c2.nextState(t); st.DisplayName != "Again" {
		t.Fatalf("state after ReplaceStatic = %+v", st)
	}
}

// Mint's cluster seam: the Reserve hook is the atomic code reservation.
// "Taken" retries with a fresh code, the store being unreachable or full
// passes straight through with no room left behind, and the CR handed to
// the hook carries the room's identity.
func TestMintReserveSeam(t *testing.T) {
	var mu sync.Mutex
	var seen []*rooms.Room
	var answers []error
	f := newFixture(t, func(o *Options) {
		o.Reserve = func(_ context.Context, cr *rooms.Room) error {
			mu.Lock()
			defer mu.Unlock()
			seen = append(seen, cr)
			err := answers[0]
			answers = answers[1:]
			return err
		}
	})
	f.bc.set("ABCDEF", BroadcastState{Live: true})
	req := func(label string) MintRequest {
		return MintRequest{BroadcastID: "ABCDEF", ResumeToken: f.tokens.MintResume("ABCDEF"), Label: label}
	}

	answers = []error{errors.New("already exists"), nil}
	res, err := f.reg.Mint(context.Background(), req("\xff"))
	if err != nil {
		t.Fatalf("mint with one collision: %v", err)
	}
	if len(seen) != 2 || seen[0].Name == seen[1].Name {
		t.Fatalf("reserve calls = %d (%v), want 2 with distinct codes", len(seen), seen)
	}
	cr := seen[1]
	if cr.Name != res.Code || cr.Spec.Kind != rooms.KindDynamic || cr.Status.CreatorTokenFingerprint != rooms.Fingerprint(res.CreatorToken) || cr.Status.CreatedAt == nil {
		t.Fatalf("reserved CR = %+v", cr)
	}
	if got := f.reg.Attachments(res.Code); len(got) != 1 || got[0].Label != "" {
		t.Fatalf("an invalid UTF-8 label was kept: %+v", got)
	}
	f.reg.EndRoom(res.Code, wire.RoomEndReasonCreator)

	for _, want := range []error{ErrUnavailable, ErrMaxRooms} {
		answers = []error{want}
		if _, err := f.reg.Mint(context.Background(), req("pc")); !errors.Is(err, want) {
			t.Fatalf("reserve %v: mint returned %v", want, err)
		}
		if tot := f.reg.TotalStats(); tot.Dynamic != 0 || tot.Attachments != 0 {
			t.Fatalf("a failed reservation left a room behind: %+v", tot)
		}
	}
}
