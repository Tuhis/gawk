package roomsrv

// R79 MV3 (docs/80 §9): the home pod's voice state, against fakes — voice-
// live rooms, the ticket, the session-link registration and its refusals,
// newest wins, the merged roster and who receives it, and every end of a
// voice session (D17, §11.5, C4). The transport tests run the same paths
// over real links.

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/events"
	"github.com/Tuhis/gawk/gawk-server/internal/eventbus"
	"github.com/Tuhis/gawk/gawk-server/rooms"
	"github.com/Tuhis/gawk/gawk-server/voice"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

var testVoiceSecret = []byte("0123456789abcdef0123")

const testVoiceURL = "https://voice.example/voice"

func voiceOpts(o *Options) {
	o.Voice = VoiceOptions{Enabled: true, PublicURL: testVoiceURL, TicketSecret: testVoiceSecret, MaxRooms: 2, MaxUsersPerRoom: 3}
}

// fakeLink is a session link: it records the close the registry decided.
type fakeLink struct {
	mu     sync.Mutex
	closed bool
	code   uint32
	reason string
}

func (l *fakeLink) Close(code uint32, reason string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.closed, l.code, l.reason = true, code, reason
	}
}

func (l *fakeLink) closeCode() (uint32, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.code, l.closed
}

func (f *fixture) joinVoice(t *testing.T, code, nick string, want bool) (*Participant, *fakeConn) {
	t.Helper()
	c := newFakeConn()
	hello := wire.RoomHello{Protocol: 1, Nickname: nick}
	if want {
		hello.WantCaps = wire.RoomCapVoice
	}
	p, err := f.reg.Join(code, hello, Grants{AttachOK: true}, nil, c)
	if err != nil {
		t.Fatalf("Join(%s): %v", code, err)
	}
	return p, c
}

// voiceRoomFixture is a static voice-live room with its observer reporting
// two native users.
func voiceRoomFixture(t *testing.T, mutate func(*Options)) *fixture {
	t.Helper()
	f := newFixture(t, func(o *Options) {
		voiceOpts(o)
		if mutate != nil {
			mutate(o)
		}
	})
	if err := f.reg.UpsertStatic(StaticRoom{Code: "friends", Mumble: &rooms.MumbleIntegration{Channel: "Friends/Gaming"}}); err != nil {
		t.Fatal(err)
	}
	vr := f.reg.VoiceRooms()
	if len(vr) != 1 || vr[0].Code != "friends" || vr[0].Channel != "Friends/Gaming" {
		t.Fatalf("VoiceRooms = %+v", vr)
	}
	f.reg.ApplyVoiceRoster("friends", vr[0].ID, voice.Message{Type: voice.TypeRoster, Available: true,
		Users: []voice.User{{Session: 7, Name: "native"}, {Session: 9, Name: "Alice [web]", Flags: wire.RoomVoiceUserSelfMute}}})
	return f
}

func (f *fixture) ticket(t *testing.T, p *Participant, c *fakeConn) []byte {
	t.Helper()
	p.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandVoiceJoin})
	e := nextEv(t, c, wire.RoomEventVoiceTicket)
	if e.VoiceURL != testVoiceURL {
		t.Fatalf("ticket url = %q", e.VoiceURL)
	}
	return append([]byte(nil), e.VoiceTicket...)
}

func TestVoiceTicketVerifiesWithSharedSecret(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	f := voiceRoomFixture(t, func(o *Options) { o.Now = func() time.Time { return now } })
	p, c := f.joinVoice(t, "friends", "alice", true)
	st := c.nextState(t)
	if st.Caps != wire.RoomCapVoice {
		t.Fatalf("caps = 0x%02x, want voice", st.Caps)
	}
	raw := f.ticket(t, p, c)
	tk, err := voice.VerifyTicket(testVoiceSecret, raw, now)
	if err != nil {
		t.Fatalf("VerifyTicket: %v", err)
	}
	epoch, _ := f.reg.VoiceEpoch("friends")
	if tk.Code != "friends" || tk.Participant != p.ID() || tk.Nickname != "alice" || tk.Epoch != epoch || !tk.Expiry.Equal(now.Add(voice.TicketTTL)) {
		t.Fatalf("ticket = %+v (epoch %d)", tk, epoch)
	}
	if _, err := voice.VerifyTicket([]byte("another-secret-entirely"), raw, now); err == nil {
		t.Fatal("ticket verified under another secret")
	}
	// Out of sequence: the next sequenced event follows the snapshot's seq
	// directly, the ticket took none.
	f.joinVoice(t, "friends", "bob", false)
	if e := nextEv(t, c, wire.RoomEventParticipantJoined); e.Seq != st.Seq+1 {
		t.Fatalf("joined seq = %d after state seq %d: the voice events advanced it", e.Seq, st.Seq)
	}
	// Two tickets never share a nonce.
	raw2 := f.ticket(t, p, c)
	tk2, _ := voice.VerifyTicket(testVoiceSecret, raw2, now)
	if tk2.Nonce == tk.Nonce {
		t.Fatal("two tickets with one nonce")
	}
}

func TestVoiceRosterMergesObserverAndSessionsAndGoesOnlyToVoiceClients(t *testing.T) {
	f := voiceRoomFixture(t, nil)
	p, c := f.joinVoice(t, "friends", "alice", true)
	st := c.nextState(t)
	e := nextEv(t, c, wire.RoomEventVoiceRosterUpdated)
	if e.Seq != st.Seq || e.VoiceFlags != wire.RoomVoiceRosterFlagAvailable || len(e.VoiceRoster) != 2 {
		t.Fatalf("roster after state = %+v (state seq %d)", e, st.Seq)
	}
	for _, v := range e.VoiceRoster {
		if v.Kind != wire.RoomVoiceKindMumble {
			t.Fatalf("entry before any registration = %+v", v)
		}
	}
	_, other := f.joinVoice(t, "friends", "viewer", false)
	other.nextState(t)

	vs, acc, err := f.reg.RegisterVoice("friends", f.ticket(t, p, c), netip.MustParseAddr("198.51.100.4"), &fakeLink{})
	if err != nil {
		t.Fatal(err)
	}
	if acc.Channel != "Friends/Gaming" || acc.Temporary || acc.Code != "friends" {
		t.Fatalf("accepted = %+v", acc)
	}
	vs.Joined(9, "Alice [web]")
	e = nextEv(t, c, wire.RoomEventVoiceRosterUpdated)
	if len(e.VoiceRoster) != 2 || e.VoiceRoster[1].Kind != wire.RoomVoiceKindGawk || e.VoiceRoster[1].ParticipantID != p.ID() ||
		e.VoiceRoster[1].Flags != wire.RoomVoiceUserSelfMute || e.VoiceRoster[0].Kind != wire.RoomVoiceKindMumble {
		t.Fatalf("merged roster = %+v", e.VoiceRoster)
	}
	vs.State(9, wire.RoomVoiceUserMute)
	if e = nextEv(t, c, wire.RoomEventVoiceRosterUpdated); e.VoiceRoster[1].Flags != wire.RoomVoiceUserMute {
		t.Fatalf("state not applied: %+v", e.VoiceRoster)
	}
	// A report that changes nothing visible pushes nothing; a resync gets
	// the roster after its snapshot regardless.
	vs.State(9, wire.RoomVoiceUserMute)
	p.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandResync})
	c.nextState(t)
	if e := c.next(t).(wire.RoomEvent); e.Kind != wire.RoomEventVoiceRosterUpdated {
		t.Fatalf("after resync: %+v", e)
	}
	// The observer losing the channel empties the roster and says so.
	id := f.reg.VoiceRooms()[0].ID
	f.reg.VoiceObserverDown("friends", id, "link lost")
	if e = nextEv(t, c, wire.RoomEventVoiceRosterUpdated); e.VoiceFlags != 0 || len(e.VoiceRoster) != 0 {
		t.Fatalf("roster while down = %+v", e)
	}
	if row := f.reg.Stats()["friends"]; !row.VoiceLive || row.VoiceSessions != 1 || row.VoiceObserver != "down" {
		t.Fatalf("stats row = %+v", row)
	}
	// A stale observer's report is dropped.
	f.reg.ApplyVoiceRoster("friends", id+100, voice.Message{Available: true})
	if row := f.reg.Stats()["friends"]; row.VoiceObserver != "down" {
		t.Fatalf("a stale observer's report applied: %+v", row)
	}
	f.reg.ApplyVoiceRoster("friends", id, voice.Message{Available: false, Reason: "cannot enter"})
	if row := f.reg.Stats()["friends"]; row.VoiceObserver != "unavailable" {
		t.Fatalf("stats row = %+v", row)
	}
	// The participant that never asked for voice saw none of it.
	other.mu.Lock()
	defer other.mu.Unlock()
	for _, rec := range other.records {
		if e, err := wire.ParseRoomEvent(rec[2:]); err == nil && (e.Kind == wire.RoomEventVoiceRosterUpdated || e.Kind == wire.RoomEventVoiceTicket) {
			t.Fatalf("a non-voice participant got voice event %+v", e)
		}
	}
}

func TestVoiceRosterIsCappedAndNamesTruncated(t *testing.T) {
	f := voiceRoomFixture(t, nil)
	users := make([]voice.User, wire.MaxVoiceRosterEntries+5)
	for i := range users {
		users[i] = voice.User{Session: uint32(i + 1), Name: string(make([]byte, 0)) + "a-very-long-mumble-name-that-goes-on-and-on-for-more-than-sixty-four-bytes", Flags: 0xff}
	}
	f.reg.ApplyVoiceRoster("friends", f.reg.VoiceRooms()[0].ID, voice.Message{Available: true, Users: users})
	_, c := f.joinVoice(t, "friends", "alice", true)
	c.nextState(t)
	e := nextEv(t, c, wire.RoomEventVoiceRosterUpdated)
	if len(e.VoiceRoster) != wire.MaxVoiceRosterEntries || len(e.VoiceRoster[0].Name) > wire.MaxVoiceNameLen || e.VoiceRoster[0].Flags != voiceUserFlagMask {
		t.Fatalf("roster = %d entries, first %+v", len(e.VoiceRoster), e.VoiceRoster[0])
	}
}

func TestSecondVoiceRegistrationClosesTheFirst(t *testing.T) {
	f := voiceRoomFixture(t, nil)
	p, c := f.joinVoice(t, "friends", "alice", true)
	first, second := &fakeLink{}, &fakeLink{}
	vs1, _, err := f.reg.RegisterVoice("friends", f.ticket(t, p, c), netip.Addr{}, first)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.reg.RegisterVoice("friends", f.ticket(t, p, c), netip.Addr{}, second); err != nil {
		t.Fatal(err)
	}
	if code, closed := first.closeCode(); !closed || code != wire.CloseCodeVoiceRemoved {
		t.Fatalf("first link = %d, %v; want 4015", code, closed)
	}
	if _, closed := second.closeCode(); closed {
		t.Fatal("the newer session was closed")
	}
	// The replaced session's late reports change nothing.
	vs1.Joined(7, "x")
	vs1.Left()
	if row := f.reg.Stats()["friends"]; row.VoiceSessions != 1 {
		t.Fatalf("sessions = %d, want the newer one", row.VoiceSessions)
	}
}

func TestReplayedVoiceTicketNonceIsRefused(t *testing.T) {
	f := voiceRoomFixture(t, nil)
	p, c := f.joinVoice(t, "friends", "alice", true)
	raw := f.ticket(t, p, c)
	link := &fakeLink{}
	if _, _, err := f.reg.RegisterVoice("friends", raw, netip.Addr{}, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.reg.RegisterVoice("friends", raw, netip.Addr{}, &fakeLink{}); err != ErrVoiceReplay {
		t.Fatalf("replay err = %v, want ErrVoiceReplay", err)
	}
	if _, closed := link.closeCode(); closed {
		t.Fatal("a replayed ticket closed the legitimate session (newest wins must not run)")
	}
}

func TestVoiceNoncesArePrunedAtExpiry(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	f := voiceRoomFixture(t, func(o *Options) { o.Now = clock })
	p, c := f.joinVoice(t, "friends", "alice", true)
	if _, _, err := f.reg.RegisterVoice("friends", f.ticket(t, p, c), netip.Addr{}, &fakeLink{}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	now = now.Add(voice.TicketTTL + time.Second)
	mu.Unlock()
	if _, _, err := f.reg.RegisterVoice("friends", f.ticket(t, p, c), netip.Addr{}, &fakeLink{}); err != nil {
		t.Fatal(err)
	}
	f.reg.mu.Lock()
	n := len(f.reg.voiceNonces)
	f.reg.mu.Unlock()
	if n != 1 {
		t.Fatalf("nonces kept = %d, want only the live ticket's", n)
	}
}

func TestVoiceTicketFromAnotherEpochIsRefused(t *testing.T) {
	var mu sync.Mutex
	gen := uint64(4)
	held := true
	f := voiceRoomFixture(t, func(o *Options) {
		o.VoiceEpoch = func(string) (uint64, bool) { mu.Lock(); defer mu.Unlock(); return gen, held }
	})
	p, c := f.joinVoice(t, "friends", "alice", true)
	raw := f.ticket(t, p, c)
	mu.Lock()
	gen = 5 // the room moved home and back
	mu.Unlock()
	if _, _, err := f.reg.RegisterVoice("friends", raw, netip.Addr{}, &fakeLink{}); err != ErrVoiceEpoch {
		t.Fatalf("err = %v, want ErrVoiceEpoch", err)
	}
	if _, _, err := f.reg.RegisterVoice("friends", f.ticket(t, p, c), netip.Addr{}, &fakeLink{}); err != nil {
		t.Fatalf("a fresh ticket from the current epoch: %v", err)
	}
	// Without the lease there is no epoch to mint under.
	mu.Lock()
	held = false
	mu.Unlock()
	p.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandVoiceJoin})
	if e := nextEv(t, c, wire.RoomEventCommandRejected); e.Reason != wire.RoomRejectUnavailable {
		t.Fatalf("reject = %+v", e)
	}
	// A single pod's epoch is random per process.
	a, b := newFixture(t, voiceOpts), newFixture(t, voiceOpts)
	ea, _ := a.reg.VoiceEpoch("x")
	eb, _ := b.reg.VoiceEpoch("x")
	if ea == eb {
		t.Fatal("two registries share an epoch")
	}
}

func TestRegisterVoiceRefusals(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	f := voiceRoomFixture(t, func(o *Options) { o.Now = clock })
	if err := f.reg.UpsertStatic(StaticRoom{Code: "other", Mumble: &rooms.MumbleIntegration{Channel: "Other"}}); err != nil {
		t.Fatal(err)
	}
	p, c := f.joinVoice(t, "friends", "alice", true)
	raw := f.ticket(t, p, c)

	if _, _, err := f.reg.RegisterVoice("friends", []byte("garbage"), netip.Addr{}, &fakeLink{}); !isErr(err, ErrVoiceBadTicket) {
		t.Errorf("garbage: %v", err)
	}
	if _, _, err := f.reg.RegisterVoice("other", raw, netip.Addr{}, &fakeLink{}); err != ErrVoiceWrongRoom {
		t.Errorf("wrong room: %v", err)
	}
	if _, _, err := f.reg.RegisterVoice("not a code!", raw, netip.Addr{}, &fakeLink{}); err != ErrVoiceWrongRoom {
		t.Errorf("bad code: %v", err)
	}
	mu.Lock()
	now = now.Add(voice.TicketTTL)
	mu.Unlock()
	if _, _, err := f.reg.RegisterVoice("friends", raw, netip.Addr{}, &fakeLink{}); err != ErrVoiceTicketExpired {
		t.Errorf("expired: %v", err)
	}
	// The participant left: its room session is gone.
	raw = f.ticket(t, p, c)
	p.Leave()
	if _, _, err := f.reg.RegisterVoice("friends", raw, netip.Addr{}, &fakeLink{}); err != ErrVoiceNoParticipant {
		t.Errorf("gone participant: %v", err)
	}
	// The cap: three sessions, a fourth participant is refused.
	var tickets [][]byte
	for _, n := range []string{"a", "b", "c", "d"} {
		q, qc := f.joinVoice(t, "friends", n, true)
		tickets = append(tickets, f.ticket(t, q, qc))
	}
	for _, tk := range tickets[:3] {
		if _, _, err := f.reg.RegisterVoice("friends", tk, netip.Addr{}, &fakeLink{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := f.reg.RegisterVoice("friends", tickets[3], netip.Addr{}, &fakeLink{}); err != ErrVoiceFull {
		t.Errorf("over the cap: %v", err)
	}
	// ...and VoiceJoin says so before a ticket is minted.
	q, qc := f.joinVoice(t, "friends", "e", true)
	q.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandVoiceJoin})
	if e := nextEv(t, qc, wire.RoomEventCommandRejected); e.Reason != wire.RoomRejectLimit || e.Command != wire.RoomCommandVoiceJoin {
		t.Errorf("join over the cap = %+v", e)
	}
	// Voice off for the room: not live.
	qraw := tickets[3]
	if err := f.reg.UpsertStatic(StaticRoom{Code: "friends"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.reg.RegisterVoice("friends", qraw, netip.Addr{}, &fakeLink{}); err != ErrVoiceNotLive {
		t.Errorf("voice off: %v", err)
	}
	f.reg.EndRoom("friends", wire.RoomEndReasonOperator)
	if _, _, err := f.reg.RegisterVoice("friends", qraw, netip.Addr{}, &fakeLink{}); err != ErrVoiceNotLive {
		t.Errorf("room gone: %v", err)
	}
}

func isErr(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

func TestVoiceJoinRejections(t *testing.T) {
	// A room with no mapping on a voice relay: Unsupported.
	f := newFixture(t, voiceOpts)
	if err := f.reg.UpsertStatic(StaticRoom{Code: "plain"}); err != nil {
		t.Fatal(err)
	}
	p, c := f.joinVoice(t, "plain", "a", true)
	if st := c.nextState(t); st.Caps != 0 {
		t.Fatalf("caps on a room without voice = 0x%02x", st.Caps)
	}
	p.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandVoiceJoin})
	if e := nextEv(t, c, wire.RoomEventCommandRejected); e.Reason != wire.RoomRejectUnsupported {
		t.Fatalf("no voice = %+v", e)
	}
	// A voice room whose observer is not in the channel: Unavailable.
	if err := f.reg.UpsertStatic(StaticRoom{Code: "friends", Mumble: &rooms.MumbleIntegration{}}); err != nil {
		t.Fatal(err)
	}
	q, qc := f.joinVoice(t, "friends", "b", true)
	q.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandVoiceJoin})
	if e := nextEv(t, qc, wire.RoomEventCommandRejected); e.Reason != wire.RoomRejectUnavailable {
		t.Fatalf("observer down = %+v", e)
	}
	// An invalid mapping is no voice at all, with a warning.
	if err := f.reg.UpsertStatic(StaticRoom{Code: "broken", Mumble: &rooms.MumbleIntegration{Channel: "x", Temporary: true}}); err != nil {
		t.Fatal(err)
	}
	for _, v := range f.reg.VoiceRooms() {
		if v.Code == "broken" {
			t.Fatal("an invalid mapping made the room voice-live")
		}
	}
}

// The off-path: voice not configured. A mapped room has no capability, no
// voice event reaches a client that asks for voice, VoiceJoin is the
// pre-R79 Unsupported, and no observer link is asked for.
func TestVoiceOffPathChangesNothing(t *testing.T) {
	var changed int
	f := newFixture(t, func(o *Options) { o.OnVoiceChanged = func() { changed++ } })
	if err := f.reg.UpsertStatic(StaticRoom{Code: "friends", Mumble: &rooms.MumbleIntegration{Channel: "Friends"}}); err != nil {
		t.Fatal(err)
	}
	if got := f.reg.VoiceRooms(); len(got) != 0 {
		t.Fatalf("VoiceRooms = %+v", got)
	}
	p, c := f.joinVoice(t, "friends", "a", true)
	if st := c.nextState(t); st.Caps != 0 {
		t.Fatalf("caps = 0x%02x", st.Caps)
	}
	p.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandVoiceJoin})
	if e := c.next(t).(wire.RoomEvent); e.Kind != wire.RoomEventCommandRejected || e.Reason != wire.RoomRejectUnsupported {
		t.Fatalf("VoiceJoin = %+v, want only an Unsupported reject", e)
	}
	if _, _, err := f.reg.RegisterVoice("friends", []byte("x"), netip.Addr{}, &fakeLink{}); err != ErrVoiceNotLive {
		t.Fatalf("RegisterVoice = %v", err)
	}
	res, err := f.reg.Mint(context.Background(), MintRequest{BroadcastID: "ABCDEF", ResumeToken: f.tokens.MintResume("ABCDEF"), Voice: true})
	if err == nil {
		t.Fatalf("mint without a live broadcast = %+v", res)
	}
	f.bc.set("ABCDEF", BroadcastState{Live: true})
	if res, err = f.reg.Mint(context.Background(), MintRequest{BroadcastID: "ABCDEF", ResumeToken: f.tokens.MintResume("ABCDEF"), Voice: true}); err != nil || res.Voice {
		t.Fatalf("mint ?voice=1 = %+v, %v; want a room without voice", res, err)
	}
	if row := f.reg.Stats()["friends"]; row.VoiceLive || row.VoiceObserver != "" {
		t.Fatalf("stats row = %+v", row)
	}
	if changed != 0 {
		t.Fatalf("OnVoiceChanged ran %d times", changed)
	}
}

func TestVoiceSessionEnds(t *testing.T) {
	f := voiceRoomFixture(t, nil)
	reg := func(nick string) (*Participant, *fakeConn, *fakeLink) {
		p, c := f.joinVoice(t, "friends", nick, true)
		l := &fakeLink{}
		if _, _, err := f.reg.RegisterVoice("friends", f.ticket(t, p, c), netip.MustParseAddr("203.0.113.9"), l); err != nil {
			t.Fatal(err)
		}
		return p, c, l
	}
	// The room control session ending: 4002 (C4).
	p, _, l := reg("leaver")
	p.Leave()
	if code, closed := l.closeCode(); !closed || code != wire.CloseCodeServerDraining {
		t.Fatalf("leave: %d %v, want 4002", code, closed)
	}
	// An IP ban covering the voice client: 4015.
	_, _, l = reg("banned")
	if n := f.reg.CloseVoiceIn(func(a netip.Addr) bool { return a == netip.MustParseAddr("203.0.113.9") }); n != 1 {
		t.Fatalf("CloseVoiceIn closed %d", n)
	}
	if code, _ := l.closeCode(); code != wire.CloseCodeVoiceRemoved {
		t.Fatalf("ban: %d", code)
	}
	// A kill of the broadcast the participant attached: 4015.
	p, c, l := reg("streamer")
	f.bc.set("QWERTY", BroadcastState{Live: true})
	p.HandleCommand(wire.RoomCommand{Kind: wire.RoomCommandAttach, BroadcastID: "QWERTY", ResumeToken: f.tokens.MintResume("QWERTY")})
	nextEv(t, c, wire.RoomEventAttachmentAdded)
	if n := f.reg.CloseVoiceOfBroadcast("UNKNWN"); n != 0 {
		t.Fatalf("an unrelated broadcast closed %d", n)
	}
	if n := f.reg.CloseVoiceOfBroadcast("QWERTY"); n != 1 {
		t.Fatalf("CloseVoiceOfBroadcast closed %d", n)
	}
	if code, _ := l.closeCode(); code != wire.CloseCodeVoiceRemoved {
		t.Fatalf("kill: %d", code)
	}
	// The channel remapped: 4016, rejoin lands in the new channel.
	_, c, l = reg("remapped")
	if err := f.reg.UpsertStatic(StaticRoom{Code: "friends", Mumble: &rooms.MumbleIntegration{Channel: "Elsewhere"}}); err != nil {
		t.Fatal(err)
	}
	if code, _ := l.closeCode(); code != wire.CloseCodeVoiceUnavailable {
		t.Fatalf("remap: %d", code)
	}
	if vr := f.reg.VoiceRooms(); len(vr) != 1 || vr[0].Channel != "Elsewhere" {
		t.Fatalf("VoiceRooms after remap = %+v", vr)
	}
	// Voice turned off: 4015, and every participant learns from a fresh
	// RoomState without the capability.
	f.reg.ApplyVoiceRoster("friends", f.reg.VoiceRooms()[0].ID, voice.Message{Available: true})
	_, c, l = reg("off")
	if err := f.reg.UpsertStatic(StaticRoom{Code: "friends"}); err != nil {
		t.Fatal(err)
	}
	if code, _ := l.closeCode(); code != wire.CloseCodeVoiceRemoved {
		t.Fatalf("voice off: %d", code)
	}
	for {
		if st, ok := c.next(t).(wire.RoomState); ok {
			if st.Caps != 0 {
				t.Fatalf("caps after voice off = 0x%02x", st.Caps)
			}
			break
		}
	}
	// ...and back on: a fresh RoomState with it.
	if err := f.reg.UpsertStatic(StaticRoom{Code: "friends", Mumble: &rooms.MumbleIntegration{Channel: "Friends"}}); err != nil {
		t.Fatal(err)
	}
	for {
		if st, ok := c.next(t).(wire.RoomState); ok {
			if st.Caps != wire.RoomCapVoice {
				t.Fatalf("caps after voice on = 0x%02x", st.Caps)
			}
			break
		}
	}
	// The room ending: 4015.
	f.reg.ApplyVoiceRoster("friends", f.reg.VoiceRooms()[0].ID, voice.Message{Available: true})
	_, _, l = reg("end")
	f.reg.EndRoom("friends", wire.RoomEndReasonOperator)
	if code, _ := l.closeCode(); code != wire.CloseCodeVoiceRemoved {
		t.Fatalf("room end: %d", code)
	}
	if len(f.reg.VoiceRooms()) != 0 {
		t.Fatal("an ended room is still voice-live")
	}
}

func TestVoiceRoomMovingHomeClosesVoiceNonTerminally(t *testing.T) {
	f := voiceRoomFixture(t, nil)
	p, c := f.joinVoice(t, "friends", "a", true)
	l := &fakeLink{}
	if _, _, err := f.reg.RegisterVoice("friends", f.ticket(t, p, c), netip.Addr{}, l); err != nil {
		t.Fatal(err)
	}
	f.reg.ReleaseHome("friends")
	f.reg.EndRoom("friends", wire.RoomEndReasonOperator)
	if code, _ := l.closeCode(); code != wire.CloseCodeServerDraining {
		t.Fatalf("re-home: %d, want 4002", code)
	}
}

func TestVoiceLiveStaticRoomIsHomedButNotLive(t *testing.T) {
	var (
		mu  sync.Mutex
		evs []string
	)
	f := newFixture(t, func(o *Options) {
		voiceOpts(o)
		o.OnEvent = func(ev eventbus.Event) { mu.Lock(); evs = append(evs, ev.Type); mu.Unlock() }
	})
	if err := f.reg.UpsertStatic(StaticRoom{Code: "friends", Mumble: &rooms.MumbleIntegration{Channel: "Friends"}, VoiceOnly: true}); err != nil {
		t.Fatal(err)
	}
	if len(f.reg.VoiceRooms()) != 1 {
		t.Fatal("not voice-live")
	}
	if got := f.reg.AdminRooms(); len(got) != 0 {
		t.Fatalf("AdminRooms lists a room homed only for voice: %+v", got)
	}
	mu.Lock()
	if len(evs) != 0 {
		t.Fatalf("voice-live published %v", evs)
	}
	mu.Unlock()
	f.joinVoice(t, "friends", "a", false)
	if got := f.reg.AdminRooms(); len(got) != 1 {
		t.Fatalf("AdminRooms after a join = %+v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, e := range evs {
		if e == events.TypeRoomOpened {
			t.Fatal("room.opened from voice")
		}
	}
	if tot := f.reg.TotalStats(); tot.VoiceLive != 1 || tot.Static != 1 {
		t.Fatalf("totals = %+v", tot)
	}
}

func TestMintWithVoiceAndTheVoiceRoomCap(t *testing.T) {
	var reserved []*rooms.Room
	var changed int
	f := newFixture(t, func(o *Options) {
		voiceOpts(o)
		o.OnVoiceChanged = func() { changed++ }
		o.Reserve = func(_ context.Context, r *rooms.Room) error { reserved = append(reserved, r.DeepCopy()); return nil }
	})
	mint := func(id string, wantVoice bool) MintResult {
		t.Helper()
		f.bc.set(id, BroadcastState{Live: true})
		res, err := f.reg.Mint(context.Background(), MintRequest{BroadcastID: id, ResumeToken: f.tokens.MintResume(id), Voice: wantVoice})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	a := mint("AAAAAA", true)
	if !a.Voice || reserved[0].Spec.Integrations.Mumble == nil || !reserved[0].Spec.Integrations.Mumble.Temporary {
		t.Fatalf("voice mint = %+v, CR mumble %+v", a, reserved[0].Spec.Integrations.Mumble)
	}
	if again := mint("AAAAAA", true); !again.Repeated || !again.Voice {
		t.Fatalf("repeat = %+v", again)
	}
	if b := mint("BBBBBB", false); b.Voice || reserved[len(reserved)-1].Spec.Integrations.Mumble != nil {
		t.Fatalf("plain mint = %+v", b)
	}
	mint("CCCCCC", true)
	// MaxRooms is 2: the third voice mint still mints, without voice.
	d := mint("DDDDDD", true)
	if d.Voice || reserved[len(reserved)-1].Spec.Integrations.Mumble != nil {
		t.Fatalf("over the cap = %+v", d)
	}
	vr := f.reg.VoiceRooms()
	if len(vr) != 2 || !vr[0].Temporary {
		t.Fatalf("VoiceRooms = %+v", vr)
	}
	if changed != 2 {
		t.Fatalf("OnVoiceChanged = %d", changed)
	}
	// The fleet count, when the cluster supplies it, is what binds.
	g := newFixture(t, func(o *Options) { voiceOpts(o); o.VoiceRoomsInUse = func() int { return 2 } })
	g.bc.set("EEEEEE", BroadcastState{Live: true})
	if res, _ := g.reg.Mint(context.Background(), MintRequest{BroadcastID: "EEEEEE", ResumeToken: g.tokens.MintResume("EEEEEE"), Voice: true}); res.Voice {
		t.Fatal("the fleet-wide voice cap did not bind")
	}
}

func TestAdoptDynamicRestoresVoice(t *testing.T) {
	f := newFixture(t, voiceOpts)
	cr := &rooms.Room{Spec: rooms.RoomSpec{Kind: rooms.KindDynamic, Integrations: rooms.Integrations{Mumble: &rooms.MumbleIntegration{Temporary: true}}}}
	cr.Name = "k7xq2m"
	if !f.reg.AdoptDynamic(cr) {
		t.Fatal("not adopted")
	}
	if vr := f.reg.VoiceRooms(); len(vr) != 1 || !vr[0].Temporary || vr[0].Code != "k7xq2m" {
		t.Fatalf("VoiceRooms = %+v", vr)
	}
}

func TestReplaceStaticCarriesTheMumbleMapping(t *testing.T) {
	f := newFixture(t, voiceOpts)
	f.reg.ReplaceStatic([]rooms.FileRoom{{Code: "friends", Mumble: &rooms.MumbleIntegration{Channel: "Friends"}}, {Code: "plain"}})
	if vr := f.reg.VoiceRooms(); len(vr) != 1 || vr[0].Code != "friends" {
		t.Fatalf("VoiceRooms = %+v", vr)
	}
	f.reg.ReplaceStatic([]rooms.FileRoom{{Code: "friends"}})
	if vr := f.reg.VoiceRooms(); len(vr) != 0 {
		t.Fatalf("VoiceRooms after the file dropped the mapping = %+v", vr)
	}
}

// nextEv is nextEvent that also skips snapshots.
func nextEv(t *testing.T, c *fakeConn, kind uint8) wire.RoomEvent {
	t.Helper()
	for {
		if e, ok := c.next(t).(wire.RoomEvent); ok && e.Kind == kind {
			return e
		}
	}
}
