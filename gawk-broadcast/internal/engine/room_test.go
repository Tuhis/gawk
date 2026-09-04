package engine

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// Rooms (R42 RM6). The policies under test are the ones a real relay will
// not produce on demand: the attach proof arriving in either order, a room
// ending mid-session, a transport loss on the control session, a refused
// dial — and, above all, that none of it touches the publisher.

// fakeRoomSession implements RoomSession over a net.Pipe: the client end is
// the control stream the engine sees, the relay end is what the test reads
// and writes. net.Pipe honours deadlines, which the engine's stop path relies
// on exactly as it does for the announce read.
type fakeRoomSession struct {
	client, relay net.Conn
	ctx           context.Context
	cancel        context.CancelFunc

	mu       sync.Mutex
	closeErr error
	closed   bool
	opens    int
}

func newFakeRoomSession() *fakeRoomSession {
	c, r := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	return &fakeRoomSession{client: c, relay: r, ctx: ctx, cancel: cancel}
}

func (f *fakeRoomSession) OpenStreamSync(ctx context.Context) (RoomStream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closeErr != nil {
		return nil, f.closeErr
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	f.opens++
	return f.client, nil
}

func (f *fakeRoomSession) CloseWithError(code webtransport.SessionErrorCode, msg string) error {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	f.cancel()
	_ = f.client.Close()
	return nil
}

func (f *fakeRoomSession) Context() context.Context { return f.ctx }

// closedByRelay models the relay closing the session with a close code.
func (f *fakeRoomSession) closedByRelay(code uint32) {
	f.mu.Lock()
	f.closeErr = &webtransport.SessionError{ErrorCode: webtransport.SessionErrorCode(code), Remote: true}
	f.mu.Unlock()
	f.cancel()
	_ = f.relay.Close()
}

// lost models the connection simply going away: no code.
func (f *fakeRoomSession) lost() {
	f.cancel()
	_ = f.relay.Close()
}

// relayRead reads one framed record on the relay side.
func (f *fakeRoomSession) relayRead(t *testing.T, within time.Duration) []byte {
	t.Helper()
	_ = f.relay.SetReadDeadline(time.Now().Add(within))
	rec, err := readRoomRecord(f.relay)
	if err != nil {
		t.Fatalf("relay read: %v", err)
	}
	return rec
}

// relayWrite frames and writes one message on the relay side.
func (f *fakeRoomSession) relayWrite(t *testing.T, msg []byte) {
	t.Helper()
	rec, err := wire.AppendRoomRecord(nil, msg)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.relay.SetWriteDeadline(time.Now().Add(2 * time.Second))
	if _, err := f.relay.Write(rec); err != nil {
		t.Fatalf("relay write: %v", err)
	}
}

// expectHello reads and validates the RoomHello every connection starts with.
func (f *fakeRoomSession) expectHello(t *testing.T) wire.RoomHello {
	t.Helper()
	rec := f.relayRead(t, 3*time.Second)
	h, err := wire.ParseRoomHello(rec)
	if err != nil {
		t.Fatalf("first record is not a RoomHello: %v (%x)", err, rec)
	}
	if h.ClientKind != wire.RoomClientNative {
		t.Errorf("hello client kind = %d, want native (%d)", h.ClientKind, wire.RoomClientNative)
	}
	return h
}

// expectCommand reads the next record and asserts it is a RoomCommand of kind.
func (f *fakeRoomSession) expectCommand(t *testing.T, kind uint8, within time.Duration) wire.RoomCommand {
	t.Helper()
	rec := f.relayRead(t, within)
	c, err := wire.ParseRoomCommand(rec)
	if err != nil {
		t.Fatalf("record is not a RoomCommand: %v (%x)", err, rec)
	}
	if c.Kind != kind {
		t.Fatalf("command kind = 0x%02x, want 0x%02x", c.Kind, kind)
	}
	return c
}

// expectSilence asserts no record arrives within d.
func (f *fakeRoomSession) expectSilence(t *testing.T, d time.Duration, what string) {
	t.Helper()
	_ = f.relay.SetReadDeadline(time.Now().Add(d))
	var b [1]byte
	n, err := f.relay.Read(b[:])
	if err == nil || n > 0 {
		t.Fatalf("%s: unexpected record on the control stream", what)
	}
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("%s: read ended with %v, want a timeout", what, err)
	}
}

// roomDialer scripts room dials and records their URLs.
type roomDialer struct {
	mu      sync.Mutex
	results []struct {
		sess   *fakeRoomSession
		status int
		err    error
	}
	urls  []string
	calls int
	// dialed is signalled per call so a test can wait for "the next dial".
	dialed chan string
}

func newRoomDialer() *roomDialer {
	return &roomDialer{dialed: make(chan string, 8)}
}

func (d *roomDialer) accept(sess *fakeRoomSession) *roomDialer {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.results = append(d.results, struct {
		sess   *fakeRoomSession
		status int
		err    error
	}{sess, http.StatusOK, nil})
	return d
}

func (d *roomDialer) refuse(status int) *roomDialer {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.results = append(d.results, struct {
		sess   *fakeRoomSession
		status int
		err    error
	}{nil, status, errors.New("fake: refused")})
	return d
}

func (d *roomDialer) fn() RoomDialFunc {
	return func(ctx context.Context, rawURL, origin string, insecure bool) (RoomSession, int, error) {
		d.mu.Lock()
		d.urls = append(d.urls, rawURL)
		i := d.calls
		d.calls++
		known := i < len(d.results)
		var r struct {
			sess   *fakeRoomSession
			status int
			err    error
		}
		if known {
			r = d.results[i]
		}
		d.mu.Unlock()
		d.dialed <- rawURL
		if !known {
			// Past the script: park until the client gives up, so an
			// unexpected reconnect shows up as a dial the test can see
			// rather than a spin.
			<-ctx.Done()
			return nil, 0, ctx.Err()
		}
		if r.err != nil {
			return nil, r.status, r.err
		}
		return r.sess, r.status, nil
	}
}

func (d *roomDialer) dialCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

func (d *roomDialer) dialedURLs() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.urls...)
}

func roomTestOpts(sess RelaySession, media *fakeMedia, rd *roomDialer) Options {
	o := testOpts(sess, media, &FakeClock{})
	o.RoomDial = rd.fn()
	return o
}

var (
	testRawToken = bytes.Repeat([]byte{0xcd}, wire.ResumeTokenSize)
	testHexToken = hex.EncodeToString(testRawToken)
)

func announceMsg(t *testing.T, id string) []byte {
	t.Helper()
	msg, err := wire.AppendBroadcastAnnounce(nil, id)
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func tokenMsg(t *testing.T) []byte {
	t.Helper()
	msg, err := wire.AppendResumeToken(nil, testRawToken)
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func stateMsg(t *testing.T, st wire.RoomState) []byte {
	t.Helper()
	msg, err := wire.AppendRoomState(nil, st)
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func eventMsg(t *testing.T, ev wire.RoomEvent) []byte {
	t.Helper()
	msg, err := wire.AppendRoomEvent(nil, ev)
	if err != nil {
		t.Fatal(err)
	}
	return msg
}

func TestRoomURLBuilders(t *testing.T) {
	// An empty attach secret is OMITTED, not sent empty: the relay verifies
	// the param whenever it is present, so an empty one would be a wrong
	// secret on every keyed static room.
	u, err := RoomURL("https://relay.example:4433", "TuhisRoom", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if u != "https://relay.example:4433/room/TuhisRoom" {
		t.Errorf("join URL = %q", u)
	}
	u, err = RoomURL("https://relay.example:4433", "ab2cd3", "k", "ff00")
	if err != nil {
		t.Fatal(err)
	}
	p, _ := url.Parse(u)
	if p.Path != "/room/ab2cd3" || p.Query().Get("attach") != "k" || p.Query().Get("creator") != "ff00" {
		t.Errorf("join URL with grants = %q", u)
	}
	if _, err := RoomURL("https://relay.example", "", "", ""); err == nil {
		t.Error("empty code accepted")
	}

	u, err = RoomNewURL("https://relay.example", "K7M2QP", testHexToken, "Desk", "invite")
	if err != nil {
		t.Fatal(err)
	}
	p, _ = url.Parse(u)
	q := p.Query()
	if p.Path != "/room/new" || q.Get("broadcast") != "K7M2QP" || q.Get("resume") != testHexToken ||
		q.Get("label") != "Desk" || q.Get("create") != "invite" {
		t.Errorf("mint URL = %q", u)
	}
	if _, err := RoomNewURL("https://relay.example", "K7M2QP", "", "", ""); err == nil {
		t.Error("a mint without the resume token was accepted: the relay would 403 it")
	}
}

// The attach latch. The relay sends the broadcast ID and the resume token on
// separate uni streams in no guaranteed order; an Attach carries both, so it
// must wait for whichever lands second — here the token lands first and the
// snapshot before either.
func TestRoomAttachWaitsForBothIDAndToken(t *testing.T) {
	sess := newFakeSession()
	media := newFakeMedia()
	room := newFakeRoomSession()
	rd := newRoomDialer().accept(room)

	states := make(chan wire.RoomState, 4)
	s := New(Config{RelayURL: "https://relay.example", Room: "TuhisRoom", RoomLabel: "Desk", Nickname: "tuhis"},
		Callbacks{OnRoomState: func(st wire.RoomState) { states <- st }},
		roomTestOpts(sess, media, rd))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	// A join dials at once (no credentials needed to be in the room).
	select {
	case u := <-rd.dialed:
		if !strings.HasSuffix(u, "/room/TuhisRoom") {
			t.Errorf("dialed %q, want …/room/TuhisRoom", u)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the room was never dialed")
	}
	if h := room.expectHello(t); h.Nickname != "tuhis" {
		t.Errorf("hello nickname = %q, want the configured one", h.Nickname)
	}
	room.relayWrite(t, stateMsg(t, wire.RoomState{Seq: 7, YourID: 3, Code: "TuhisRoom", Flags: wire.RoomStateFlagAttachOK}))
	select {
	case st := <-states:
		if st.Code != "TuhisRoom" || st.Seq != 7 {
			t.Errorf("state = %+v", st)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnRoomState never fired")
	}

	// Token first, then the ID: nothing may go out in between.
	sess.incoming <- announceStream(tokenMsg(t))
	room.expectSilence(t, 150*time.Millisecond, "after the token alone")
	sess.incoming <- announceStream(announceMsg(t, "K7M2QP"))

	cmd := room.expectCommand(t, wire.RoomCommandAttach, 3*time.Second)
	if cmd.BroadcastID != "K7M2QP" || !bytes.Equal(cmd.ResumeToken, testRawToken) || cmd.Label != "Desk" {
		t.Errorf("attach = %+v, want K7M2QP / raw token / Desk", cmd)
	}
	// Exactly one: a second credential arrival must not re-attach.
	room.expectSilence(t, 150*time.Millisecond, "after the attach")
}

// A mint cannot dial before both halves of the proof exist (the relay
// verifies them pre-upgrade), reports the creator token once, and then
// claims the attachment the mint created with one idempotent Attach — a
// minted attachment has no owner participant on the relay, and ownership is
// what makes the roster flag the broadcaster as streaming and lets it detach
// as the publisher.
func TestRoomMintWaitsForCredentialsAndReportsCreatorToken(t *testing.T) {
	sess := newFakeSession()
	media := newFakeMedia()
	room := newFakeRoomSession()
	rd := newRoomDialer().accept(room)

	created := make(chan [2]string, 1)
	s := New(Config{RelayURL: "https://relay.example", RoomNew: true, RoomLabel: "Desk", RoomCreateSecret: "invite"},
		Callbacks{OnRoomCreated: func(code, tok string) { created <- [2]string{code, tok} }},
		roomTestOpts(sess, media, rd))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	sess.incoming <- announceStream(announceMsg(t, "K7M2QP"))
	select {
	case u := <-rd.dialed:
		t.Fatalf("mint dialed %q with only the ID known", u)
	case <-time.After(150 * time.Millisecond):
	}
	sess.incoming <- announceStream(tokenMsg(t))
	select {
	case u := <-rd.dialed:
		p, _ := url.Parse(u)
		q := p.Query()
		if p.Path != "/room/new" || q.Get("broadcast") != "K7M2QP" || q.Get("resume") != testHexToken ||
			q.Get("label") != "Desk" || q.Get("create") != "invite" {
			t.Errorf("mint URL = %q", u)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the mint never dialed once both credentials were known")
	}
	room.expectHello(t)

	creator := bytes.Repeat([]byte{0x11}, wire.RoomCreatorTokenSize)
	room.relayWrite(t, stateMsg(t, wire.RoomState{
		Seq: 1, YourID: 1, Code: "AB2CD3",
		Flags:        wire.RoomStateFlagDynamic | wire.RoomStateFlagCreator | wire.RoomStateFlagAttachOK,
		CreatorToken: creator,
		Attachments:  []wire.RoomAttachment{{BroadcastID: "K7M2QP", Label: "Desk", Live: true}},
	}))
	select {
	case got := <-created:
		if got[0] != "AB2CD3" || got[1] != hex.EncodeToString(creator) {
			t.Errorf("OnRoomCreated = %v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("OnRoomCreated never fired")
	}
	if c := room.expectCommand(t, wire.RoomCommandAttach, 2*time.Second); c.BroadcastID != "K7M2QP" || c.Label != "Desk" {
		t.Errorf("ownership attach after the mint = %+v", c)
	}
	room.expectSilence(t, 150*time.Millisecond, "after the ownership attach")
	if code, ok := s.InRoom(); !ok || code != "AB2CD3" {
		t.Errorf("InRoom() = %q, %v", code, ok)
	}
}

// A publish resume re-attaches: the relay's hub saw a new publisher session
// and the attachment's ownership follows it.
func TestRoomReattachesAfterPublishResume(t *testing.T) {
	first, second := newFakeSession(), newFakeSession()
	media := newFakeMedia()
	room := newFakeRoomSession()
	rd := newRoomDialer().accept(room)
	d := &recordingDialer{results: []dialResult{{first, http.StatusOK, nil}, {second, http.StatusOK, nil}}}
	opts := resumeOpts(d, media)
	opts.RoomDial = rd.fn()

	resumed := make(chan struct{}, 4)
	s := New(Config{RelayURL: "https://relay.example", BroadcastID: "K7M2QP", ResumeToken: testHexToken, Room: "TuhisRoom"},
		Callbacks{OnResumed: func() { resumed <- struct{}{} }},
		opts)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	<-rd.dialed
	room.expectHello(t)
	room.relayWrite(t, stateMsg(t, wire.RoomState{Seq: 1, YourID: 2, Code: "TuhisRoom", Flags: wire.RoomStateFlagAttachOK}))
	if c := room.expectCommand(t, wire.RoomCommandAttach, 3*time.Second); c.BroadcastID != "K7M2QP" {
		t.Errorf("first attach = %+v", c)
	}

	first.cancel() // the publisher transport died; the room session did not
	waitSignal(t, resumed, 3*time.Second, "auto-resume")
	if c := room.expectCommand(t, wire.RoomCommandAttach, 3*time.Second); !bytes.Equal(c.ResumeToken, testRawToken) {
		t.Errorf("re-attach after resume = %+v", c)
	}
	if rd.dialCount() != 1 {
		t.Errorf("room dialed %d times; a publish resume must not reconnect the room", rd.dialCount())
	}
}

// Close code 4007 is terminal for the room ONLY: no reconnect, OnRoomEnded
// with the RoomEnding reason — and the broadcast is untouched.
func TestRoomEndedIsTerminalForTheRoomOnly(t *testing.T) {
	sess := newFakeSession()
	media := newFakeMedia()
	room := newFakeRoomSession()
	rd := newRoomDialer().accept(room)

	ended := make(chan uint8, 1)
	broadcastEnded := make(chan struct{}, 1)
	s := New(Config{RelayURL: "https://relay.example", Room: "TuhisRoom"},
		Callbacks{
			OnRoomEnded: func(r uint8) { ended <- r },
			OnEnded:     func() { broadcastEnded <- struct{}{} },
		},
		roomTestOpts(sess, media, rd))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	<-rd.dialed
	room.expectHello(t)
	room.relayWrite(t, stateMsg(t, wire.RoomState{Seq: 1, YourID: 2, Code: "TuhisRoom"}))
	room.relayWrite(t, eventMsg(t, wire.RoomEvent{Seq: 2, Kind: wire.RoomEventRoomEnding, Reason: wire.RoomEndReasonCreator}))
	room.closedByRelay(wire.CloseCodeRoomEnded)

	select {
	case r := <-ended:
		if r != wire.RoomEndReasonCreator {
			t.Errorf("OnRoomEnded reason = %d, want creator (%d)", r, wire.RoomEndReasonCreator)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OnRoomEnded never fired")
	}
	select {
	case u := <-rd.dialed:
		t.Fatalf("the room was re-dialed (%q) after 4007", u)
	case <-time.After(400 * time.Millisecond):
	}
	if len(broadcastEnded) != 0 || media.wasStopped() || sess.isClosed() {
		t.Error("the room ending touched the broadcast")
	}
	if _, ok := s.InRoom(); ok {
		t.Error("InRoom() still true after the room ended")
	}
}

// An abrupt loss (no close code) reconnects with the resume backoff shape,
// says hello again, and re-attaches on the new snapshot.
func TestRoomReconnectsAfterAbruptLossAndReattaches(t *testing.T) {
	sess := newFakeSession()
	media := newFakeMedia()
	room1, room2 := newFakeRoomSession(), newFakeRoomSession()
	rd := newRoomDialer().accept(room1).accept(room2)
	sess.incoming <- announceStream(announceMsg(t, "K7M2QP"))
	sess.incoming <- announceStream(tokenMsg(t))

	s := New(Config{RelayURL: "https://relay.example", Room: "TuhisRoom"}, Callbacks{}, roomTestOpts(sess, media, rd))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	<-rd.dialed
	room1.expectHello(t)
	room1.relayWrite(t, stateMsg(t, wire.RoomState{Seq: 1, YourID: 2, Code: "TuhisRoom"}))
	room1.expectCommand(t, wire.RoomCommandAttach, 3*time.Second)

	room1.lost()
	select {
	case u := <-rd.dialed:
		if !strings.HasSuffix(u, "/room/TuhisRoom") {
			t.Errorf("reconnect dialed %q", u)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no reconnect after an abrupt loss")
	}
	room2.expectHello(t)
	room2.relayWrite(t, stateMsg(t, wire.RoomState{Seq: 9, YourID: 5, Code: "TuhisRoom"}))
	room2.expectCommand(t, wire.RoomCommandAttach, 3*time.Second)
}

// A dial the relay refuses for good (403: wrong attach secret) is one typed
// error and no retry; the broadcast is unaffected.
func TestRoomDialRefusedIsATypedErrorWithoutRetry(t *testing.T) {
	sess := newFakeSession()
	media := newFakeMedia()
	rd := newRoomDialer().refuse(http.StatusForbidden)

	errs := make(chan error, 2)
	s := New(Config{RelayURL: "https://relay.example", Room: "TuhisRoom", RoomAttachSecret: "wrong"},
		Callbacks{OnRoomError: func(err error) { errs <- err }},
		roomTestOpts(sess, media, rd))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	select {
	case err := <-errs:
		re, ok := AsRoomError(err)
		if !ok {
			t.Fatalf("error %T (%v), want *RoomError", err, err)
		}
		if re.Status != http.StatusForbidden || re.Op != "join" {
			t.Errorf("RoomError = %+v", re)
		}
		if !strings.Contains(re.Message(), "attach secret") {
			t.Errorf("Message() = %q, want it to name the attach secret", re.Message())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("OnRoomError never fired")
	}
	if u := rd.dialedURLs(); len(u) != 1 || !strings.Contains(u[0], "attach=wrong") {
		t.Errorf("dials = %v, want exactly one carrying the attach secret", u)
	}
	if media.wasStopped() || sess.isClosed() {
		t.Error("a refused room dial touched the broadcast")
	}
}

// A sequence gap means a missed delta: the client asks for a fresh snapshot.
// A CommandRejected repeats the current seq and is not a gap.
func TestRoomEventGapTriggersResync(t *testing.T) {
	sess := newFakeSession()
	media := newFakeMedia()
	room := newFakeRoomSession()
	rd := newRoomDialer().accept(room)

	errs := make(chan error, 2)
	events := make(chan wire.RoomEvent, 8)
	s := New(Config{RelayURL: "https://relay.example", Room: "TuhisRoom"},
		Callbacks{OnRoomError: func(err error) { errs <- err }, OnRoomEvent: func(ev wire.RoomEvent) { events <- ev }},
		roomTestOpts(sess, media, rd))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	<-rd.dialed
	room.expectHello(t)
	room.relayWrite(t, stateMsg(t, wire.RoomState{Seq: 4, YourID: 2, Code: "TuhisRoom"}))
	room.relayWrite(t, eventMsg(t, wire.RoomEvent{Seq: 4, Kind: wire.RoomEventCommandRejected,
		Command: wire.RoomCommandAttach, Reason: wire.RoomRejectForbidden, Message: "attach key required"}))
	select {
	case err := <-errs:
		var rj *RoomRejectError
		if !errors.As(err, &rj) || rj.Reason != wire.RoomRejectForbidden {
			t.Errorf("rejection surfaced as %T %v", err, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CommandRejected did not reach OnRoomError")
	}
	room.expectSilence(t, 100*time.Millisecond, "after a rejection at the current seq")

	room.relayWrite(t, eventMsg(t, wire.RoomEvent{Seq: 7, Kind: wire.RoomEventParticipantLeft, Participant: wire.RoomParticipant{ID: 9}}))
	room.expectCommand(t, wire.RoomCommandResync, 2*time.Second)
	if len(events) < 2 {
		t.Errorf("OnRoomEvent fired %d times, want the rejection and the left event", len(events))
	}
}

// LeaveRoom sends a Detach, waits for the relay's confirmation, then closes
// — the command and the session close travel on different streams and would
// otherwise race. Stop, by contrast, never detaches (the attachment outlives
// the participant on purpose).
func TestLeaveRoomDetachesAndStopDoesNot(t *testing.T) {
	sess := newFakeSession()
	media := newFakeMedia()
	room := newFakeRoomSession()
	rd := newRoomDialer().accept(room)
	sess.incoming <- announceStream(announceMsg(t, "K7M2QP"))
	sess.incoming <- announceStream(tokenMsg(t))

	s := New(Config{RelayURL: "https://relay.example", Room: "TuhisRoom"}, Callbacks{}, roomTestOpts(sess, media, rd))
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	<-rd.dialed
	room.expectHello(t)
	room.relayWrite(t, stateMsg(t, wire.RoomState{Seq: 1, YourID: 2, Code: "TuhisRoom"}))
	room.expectCommand(t, wire.RoomCommandAttach, 3*time.Second)

	left := make(chan struct{})
	go func() {
		defer close(left)
		s.LeaveRoom()
	}()
	c := room.expectCommand(t, wire.RoomCommandDetach, 3*time.Second)
	if c.BroadcastID != "K7M2QP" {
		t.Errorf("detach = %+v", c)
	}
	select {
	case <-left:
		t.Fatal("LeaveRoom returned before the relay confirmed the detach")
	case <-time.After(100 * time.Millisecond):
	}
	room.relayWrite(t, eventMsg(t, wire.RoomEvent{Seq: 2, Kind: wire.RoomEventAttachmentRemoved,
		Attachment: wire.RoomAttachment{BroadcastID: "K7M2QP"}, Reason: wire.RoomDetachReasonPublisher}))
	waitSignal(t, left, 3*time.Second, "LeaveRoom returning after the ack")
	if _, ok := s.InRoom(); ok {
		t.Error("InRoom() after LeaveRoom")
	}
	if media.wasStopped() || sess.isClosed() {
		t.Error("LeaveRoom touched the broadcast")
	}

	// Join again through the live API, then Stop: no Detach this time.
	room2 := newFakeRoomSession()
	rd.accept(room2)
	if err := s.JoinRoom("TuhisRoom", ""); err != nil {
		t.Fatal(err)
	}
	<-rd.dialed
	room2.expectHello(t)
	room2.relayWrite(t, stateMsg(t, wire.RoomState{Seq: 1, YourID: 3, Code: "TuhisRoom"}))
	room2.expectCommand(t, wire.RoomCommandAttach, 3*time.Second)
	s.Stop()
	_ = room2.relay.SetReadDeadline(time.Now().Add(time.Second))
	if rec, err := readRoomRecord(room2.relay); err == nil {
		t.Errorf("Stop sent a record (%x); it must leave the attachment for the grace", rec)
	}
	if !room2.closed {
		t.Error("Stop did not close the room session")
	}
}
