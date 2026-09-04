package engine

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// Rooms (R42, docs/44 §4.8 "Native broadcasters", RM6).
//
// A room control session is a second WebTransport session beside the
// publisher's, carrying ONE bidirectional stream of length-prefixed records
// (docs/44 §4.6). Media never rides it: the broadcast keeps publishing on its
// own session exactly as before, and the room session only tells the relay
// "this broadcast belongs in that room" — an Attach command carrying the
// broadcast ID and the raw resume token as proof (D9).
//
// Two facts shape the code below:
//
//   - The attach proof arrives in unspecified order. The relay announces the
//     broadcast ID and hands over the resume token on separate uni streams,
//     and webtransport-go does not accept in open order (readServerMessages).
//     So an Attach may only be sent once BOTH have landed — the attach latch.
//   - The broadcast outlives any one publisher session (auto-resume, R17) and
//     the room session outlives any one QUIC connection too. Every publish
//     resume and every room reconnect therefore re-attaches: the relay treats
//     it as idempotent (label/ownership refresh), and a reconnected
//     broadcaster must re-attach before it may detach.
//
// The room session's failures never touch the publisher: close code 4007 is
// terminal for the room only (terminalForPublisher never learns it), and a
// room that cannot be joined is an OnRoomError, not a session-ending one.

// roomCloseCodeEnded is wire.CloseCodeRoomEnded, restated as the local name
// the close-code switch reads. Terminal for the room session only.
const roomCloseCodeEnded = wire.CloseCodeRoomEnded

// roomWriteTimeout bounds one control record write; past it the relay is
// treated as gone and the read loop's error path takes over.
const roomWriteTimeout = 5 * time.Second

// roomDetachAckTimeout bounds how long LeaveRoom waits for the relay to
// confirm a Detach before closing the session. The command travels on the
// control stream and the session close on the CONNECT stream; closing
// immediately would race them.
const roomDetachAckTimeout = 2 * time.Second

// DefaultNickname is the roster name a native broadcaster joins with when
// the profile sets none.
const DefaultNickname = "gawk-broadcast"

// RoomSession is the slice of a WebTransport session a room control session
// uses — a seam in the RelaySession mould. The policies worth testing are
// again the failure ones (a relay that never sends RoomState, a 4007 mid
// session, a token that arrives after the state), and a real relay will not
// produce those on demand.
type RoomSession interface {
	// OpenStreamSync opens the one bidirectional control stream. On a closed
	// session it hands back the close error before touching the connection,
	// which is how roomCloseCode reads the code (sessionCloseCode's trick).
	OpenStreamSync(ctx context.Context) (RoomStream, error)
	CloseWithError(code webtransport.SessionErrorCode, msg string) error
	// Context is cancelled when the session ends, for any reason.
	Context() context.Context
}

// RoomStream is the control stream: records in both directions.
type RoomStream interface {
	io.Reader
	io.Writer
	io.Closer
	SetReadDeadline(t time.Time) error
	SetWriteDeadline(t time.Time) error
}

// RoomDialFunc dials a room control session. Same shape as DialFunc, and for
// the same reason: the HTTP status is the whole point (403 wrong attach
// secret, 404 unknown code, 409 already in a room, 429 full).
type RoomDialFunc func(ctx context.Context, rawURL, origin string, insecure bool) (sess RoomSession, status int, err error)

// wtRoomSession adapts *webtransport.Session to RoomSession.
type wtRoomSession struct{ s *webtransport.Session }

func (w wtRoomSession) OpenStreamSync(ctx context.Context) (RoomStream, error) {
	str, err := w.s.OpenStreamSync(ctx)
	if err != nil {
		return nil, err
	}
	return str, nil
}

func (w wtRoomSession) CloseWithError(code webtransport.SessionErrorCode, msg string) error {
	return w.s.CloseWithError(code, msg)
}

func (w wtRoomSession) Context() context.Context { return w.s.Context() }

// dialRoom is the production RoomDialFunc: the publisher's dial, one
// adapter over.
func dialRoom(ctx context.Context, rawURL, origin string, insecure bool) (RoomSession, int, error) {
	sess, status, err := dialWebTransport(ctx, rawURL, origin, insecure)
	if err != nil {
		return nil, status, err
	}
	return wtRoomSession{s: sess}, status, nil
}

// RoomURL builds the CONNECT URL that joins a room (docs/44 §4.2): the code
// or slug as the path, the attach secret and creator token as query params.
//
// The attach param is only set when non-empty: the relay verifies it
// whenever it is *present*, so an empty one would turn "no secret given"
// into "wrong secret" (403) on every static room that has one.
func RoomURL(relayURL, code, attachSecret, creatorToken string) (string, error) {
	if code == "" {
		return "", errors.New("engine: empty room code")
	}
	base, err := url.Parse(relayURL)
	if err != nil {
		return "", err
	}
	u, err := base.Parse("/room/" + url.PathEscape(code))
	if err != nil {
		return "", err
	}
	q := u.Query()
	if attachSecret != "" {
		q.Set("attach", attachSecret)
	}
	if creatorToken != "" {
		q.Set("creator", creatorToken)
	}
	if len(q) > 0 {
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

// RoomNewURL builds the CONNECT URL that mints a dynamic room from a live
// broadcast (docs/44 §4.4 step 1): the broadcast ID and its resume token
// (hex, the relay's query-param encoding) are the proof of ownership; the
// label names the tile; the create secret is the fleet's -room-create-secret
// when it has one.
func RoomNewURL(relayURL, broadcastID, resumeTokenHex, label, createSecret string) (string, error) {
	if broadcastID == "" || resumeTokenHex == "" {
		return "", errors.New("engine: a room is minted from a live broadcast: ID and resume token are required")
	}
	base, err := url.Parse(relayURL)
	if err != nil {
		return "", err
	}
	u, err := base.Parse("/room/new")
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("broadcast", broadcastID)
	q.Set("resume", resumeTokenHex)
	if label != "" {
		q.Set("label", label)
	}
	if createSecret != "" {
		q.Set("create", createSecret)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// RoomError is a room control session failure the shells can act on: the
// dial the relay refused (Status set), or a protocol failure (Status 0).
// Distinct from StartError because it is not session-fatal — the broadcast
// keeps publishing whatever happens to the room.
type RoomError struct {
	// Op is "join" or "new".
	Op     string
	Status int
	Err    error
}

func (e *RoomError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("room %s failed (HTTP %d): %v", e.Op, e.Status, e.Err)
	}
	return fmt.Sprintf("room %s failed: %v", e.Op, e.Err)
}

func (e *RoomError) Unwrap() error { return e.Err }

// Message renders the failure as a sentence. The statuses are the ones the
// relay's handleRoom / handleRoomNew actually return (docs/44 §4.2, §4.4);
// keep this in step with gawk-server/internal/transport/room.go.
func (e *RoomError) Message() string {
	switch e.Status {
	case http.StatusForbidden:
		if e.Op == "new" {
			return "The relay refused to create a room: the create secret or the broadcast's resume token was rejected."
		}
		return "The room refused the attach secret. Check it matches the room's."
	case http.StatusNotFound:
		if e.Op == "new" {
			return "The relay does not know this broadcast, so no room could be created from it."
		}
		return "No room with that code exists on the relay (or it has ended)."
	case http.StatusConflict:
		return "This broadcast is already attached to a room. Detach it first."
	case http.StatusTooManyRequests:
		if e.Op == "new" {
			return "The relay has reached its room limit. Try again later."
		}
		return "The room is full."
	case http.StatusUnavailableForLegalReasons:
		return "Your address is banned by the relay operator."
	case http.StatusServiceUnavailable:
		return "The relay cannot create rooms right now (its store is unavailable). Try again in a moment."
	}
	if e.Op == "new" {
		return fmt.Sprintf("Could not create a room: %v", e.Err)
	}
	return fmt.Sprintf("Could not join the room: %v", e.Err)
}

// RoomRejectError is a CommandRejected from the relay: the room session is
// still up, but one command was refused.
type RoomRejectError struct {
	Command uint8
	Reason  uint8
	Detail  string
}

func (e *RoomRejectError) Error() string {
	return fmt.Sprintf("room command 0x%02x rejected (reason %d): %s", e.Command, e.Reason, e.Detail)
}

// Message renders the rejection as a sentence, one per wire reason.
func (e *RoomRejectError) Message() string {
	switch e.Reason {
	case wire.RoomRejectLimit:
		return "The room has no free broadcast slot."
	case wire.RoomRejectBadProof:
		return "The relay did not accept this broadcast's resume token as proof of ownership."
	case wire.RoomRejectNotFound:
		return "The relay could not find this broadcast to attach it."
	case wire.RoomRejectForbidden:
		return "Attaching to this room needs its attach secret."
	case wire.RoomRejectAlreadyAttached:
		return "This broadcast is already attached to another room."
	case wire.RoomRejectUnsupported:
		return "The relay does not support that room command."
	case wire.RoomRejectUnavailable:
		return "The relay cannot change the room right now. Try again in a moment."
	}
	return "The relay rejected the room command: " + e.Detail
}

// AsRoomError extracts a *RoomError from err, if there is one.
func AsRoomError(err error) (*RoomError, bool) {
	var re *RoomError
	ok := errors.As(err, &re)
	return re, ok
}

// --- the client --------------------------------------------------------------

type roomMode int

const (
	roomModeJoin roomMode = iota
	roomModeNew
)

// roomClient is one room membership: it dials, keeps the control stream up
// across reconnects, and owns the attach latch.
type roomClient struct {
	s   *Session
	log *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	// wake is poked whenever the attach credentials may have completed, so a
	// mint waiting on them does not poll.
	wake chan struct{}
	// detached is closed when the relay confirms our Detach (LeaveRoom waits
	// on it before closing the session).
	detached     chan struct{}
	detachedOnce sync.Once

	mu           sync.Mutex
	mode         roomMode
	code         string // the configured code (join) or the minted display code
	attachSecret string
	createSecret string
	label        string
	creatorToken string // hex; set by a mint, presented on reconnect
	stream       RoomStream
	sess         RoomSession
	// gen counts connections and publish resumes; attachedGen is the gen an
	// Attach was last sent at (or the mint snapshot, which attached us).
	gen         uint64
	attachedGen uint64
	haveState   bool
	lastSeq     uint32
	endReason   uint8
	wmu         sync.Mutex
}

func (s *Session) newRoomClient(parent context.Context, mode roomMode, code, attachSecret, label, createSecret string) *roomClient {
	ctx, cancel := context.WithCancel(parent)
	return &roomClient{
		s:            s,
		log:          s.log,
		ctx:          ctx,
		cancel:       cancel,
		done:         make(chan struct{}),
		wake:         make(chan struct{}, 1),
		detached:     make(chan struct{}),
		mode:         mode,
		code:         code,
		attachSecret: attachSecret,
		createSecret: createSecret,
		label:        label,
	}
}

// startConfiguredRoom opens the room the Config asked for, if any. Called
// from startLive, after the publisher session is up.
func (s *Session) startConfiguredRoom(ctx context.Context) {
	switch {
	case s.cfg.RoomNew:
		s.startRoom(ctx, roomModeNew, "", "", s.cfg.RoomLabel, s.cfg.RoomCreateSecret)
	case s.cfg.Room != "":
		s.startRoom(ctx, roomModeJoin, s.cfg.Room, s.cfg.RoomAttachSecret, s.cfg.RoomLabel, "")
	}
}

// startRoom replaces the current room client (if any) with a new one.
// Under s.mu: a stopped session must not spawn goroutines, and the room
// goroutine joins s.wg so Stop waits for it — the Add is safe because a live
// session always has its supervisor in the group.
func (s *Session) startRoom(ctx context.Context, mode roomMode, code, attachSecret, label, createSecret string) error {
	s.mu.Lock()
	if s.stopped || s.cancel == nil {
		s.mu.Unlock()
		return errors.New("engine: session is not live")
	}
	old := s.room
	rc := s.newRoomClient(ctx, mode, code, attachSecret, label, createSecret)
	s.room = rc
	s.wg.Add(1)
	s.mu.Unlock()
	if old != nil {
		old.stop()
	}
	go func() {
		defer s.wg.Done()
		rc.run()
	}()
	return nil
}

// JoinRoom attaches the live broadcast to a room by code or slug. A room the
// session is already in is left first (a broadcast belongs to at most one
// room, D1). attachSecret is a static room's key; empty for dynamic rooms.
func (s *Session) JoinRoom(code, attachSecret string) error {
	if code == "" {
		return errors.New("engine: empty room code")
	}
	s.LeaveRoom()
	s.mu.Lock()
	ctx, label := s.roomCtx, s.cfg.RoomLabel
	s.mu.Unlock()
	if ctx == nil {
		return errors.New("engine: session is not live")
	}
	return s.startRoom(ctx, roomModeJoin, code, attachSecret, label, "")
}

// NewRoom mints a dynamic room from the live broadcast (once its ID and
// resume token are known) and joins it as creator. The code and creator
// token arrive through OnRoomCreated.
func (s *Session) NewRoom(label, createSecret string) error {
	s.LeaveRoom()
	s.mu.Lock()
	ctx := s.roomCtx
	if label == "" {
		label = s.cfg.RoomLabel
	}
	s.mu.Unlock()
	if ctx == nil {
		return errors.New("engine: session is not live")
	}
	return s.startRoom(ctx, roomModeNew, "", "", label, createSecret)
}

// LeaveRoom detaches the broadcast from its room (when attached) and closes
// the room session. Idempotent; a no-op when not in a room. The broadcast
// itself keeps publishing.
func (s *Session) LeaveRoom() {
	s.mu.Lock()
	rc := s.room
	s.room = nil
	s.mu.Unlock()
	if rc == nil {
		return
	}
	rc.detach()
	rc.stop()
}

// InRoom reports whether a room session is currently held (joined or
// reconnecting), and its code when known.
func (s *Session) InRoom() (code string, ok bool) {
	s.mu.Lock()
	rc := s.room
	s.mu.Unlock()
	if rc == nil {
		return "", false
	}
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.code, true
}

// roomCredentialsChanged is the attach latch's input: called whenever the
// broadcast ID or the resume token lands.
func (s *Session) roomCredentialsChanged() {
	s.mu.Lock()
	rc := s.room
	s.mu.Unlock()
	if rc != nil {
		rc.credentialsChanged()
	}
}

// roomResumed re-attaches after a publish resume: the relay's hub saw a new
// publisher session, and the attachment's ownership follows it.
func (s *Session) roomResumed() {
	s.mu.Lock()
	rc := s.room
	s.mu.Unlock()
	if rc != nil {
		rc.reattach()
	}
}

// stopRoom is teardown's half: cancel, never wait (the goroutine is in
// s.wg, which Stop waits on).
func (s *Session) stopRoom() {
	s.mu.Lock()
	rc := s.room
	s.room = nil
	s.mu.Unlock()
	if rc != nil {
		rc.stop()
	}
}

// credentials returns the attach proof as far as it has arrived.
func (s *Session) credentials() (id, tokenHex string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.BroadcastID, s.cfg.ResumeToken
}

func (rc *roomClient) credentialsChanged() {
	select {
	case rc.wake <- struct{}{}:
	default:
	}
	rc.maybeAttach()
}

func (rc *roomClient) reattach() {
	rc.mu.Lock()
	rc.gen++
	rc.mu.Unlock()
	rc.maybeAttach()
}

// stop ends the client: cancels its context (which unblocks the read loop
// through the deadline watch) and closes the session.
func (rc *roomClient) stop() {
	rc.cancel()
	rc.mu.Lock()
	sess := rc.sess
	rc.mu.Unlock()
	if sess != nil {
		_ = sess.CloseWithError(webtransport.SessionErrorCode(0), "")
	}
}

// run is the client's life: dial, serve, reconnect — until the room ends,
// the relay refuses for good, or the client is stopped.
func (rc *roomClient) run() {
	defer close(rc.done)
	// A client that ends on its own (room ended, refused for good) is no
	// longer the session's room; one replaced by JoinRoom/LeaveRoom already
	// isn't, so only clear our own reference.
	defer func() {
		rc.s.mu.Lock()
		if rc.s.room == rc {
			rc.s.room = nil
		}
		rc.s.mu.Unlock()
	}()
	delay := resumeInitialDelay
	var deadline time.Time
	for attempt := 1; ; attempt++ {
		if rc.ctx.Err() != nil {
			return
		}
		if !rc.waitCredentials() {
			return
		}
		rawURL, op, err := rc.dialURL()
		if err != nil {
			rc.fail(&RoomError{Op: op, Err: err})
			return
		}
		origin, insecure := rc.s.dialParams()
		sess, status, err := rc.s.roomDial(rc.ctx, rawURL, origin, insecure)
		if err != nil {
			if rc.ctx.Err() != nil {
				return
			}
			rerr := &RoomError{Op: op, Status: status, Err: err}
			rc.log.Info("room dial failed", "op", op, "attempt", attempt, "status", status, "err", err)
			if deadline.IsZero() {
				deadline = time.Now().Add(resumeWindow)
			}
			if roomDialTerminal(status) || time.Now().After(deadline) {
				rc.fail(rerr)
				return
			}
			if !sleepCtx(rc.ctx, delay) {
				return
			}
			if delay *= 2; delay > resumeMaxDelay {
				delay = resumeMaxDelay
			}
			continue
		}
		delay, deadline, attempt = resumeInitialDelay, time.Time{}, 0

		if rc.serve(sess) {
			return
		}
		if rc.ctx.Err() != nil {
			return
		}
		rc.log.Info("room session lost; reconnecting")
		if !sleepCtx(rc.ctx, resumeInitialDelay) {
			return
		}
	}
}

// waitCredentials blocks a mint until the broadcast ID and resume token are
// both known; a join dials at once and attaches when they arrive.
func (rc *roomClient) waitCredentials() bool {
	for {
		rc.mu.Lock()
		mode := rc.mode
		rc.mu.Unlock()
		if mode != roomModeNew {
			return true
		}
		if id, tok := rc.s.credentials(); id != "" && tok != "" {
			return true
		}
		select {
		case <-rc.ctx.Done():
			return false
		case <-rc.wake:
		}
	}
}

// dialURL picks the URL for this connection: /room/new for a mint that has
// not happened yet, /room/{code} (with the creator token, if we hold one)
// otherwise.
func (rc *roomClient) dialURL() (rawURL, op string, err error) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	relayURL := rc.s.relayURL()
	if rc.mode == roomModeNew {
		id, tok := rc.s.credentials()
		u, err := RoomNewURL(relayURL, id, tok, rc.label, rc.createSecret)
		return u, "new", err
	}
	u, err := RoomURL(relayURL, rc.code, rc.attachSecret, rc.creatorToken)
	return u, "join", err
}

// roomDialTerminal reports whether a room dial's HTTP status means retrying
// can only fail again — the same reasoning as resumeTerminal, for the room
// routes' own statuses.
func roomDialTerminal(status int) bool {
	switch status {
	case http.StatusForbidden, // wrong attach secret / creator token / create secret
		http.StatusNotFound,                   // unknown code, or unknown broadcast
		http.StatusConflict,                   // broadcast already in a room
		http.StatusUnavailableForLegalReasons: // banned
		return true
	}
	return false
}

// serve runs one connection to completion. It reports whether the client
// is done for good (room ended, protocol failure, stopped) as opposed to
// merely disconnected.
func (rc *roomClient) serve(sess RoomSession) (terminal bool) {
	str, err := sess.OpenStreamSync(rc.ctx)
	if err != nil {
		rc.log.Warn("room control stream not opened", "err", err)
		_ = sess.CloseWithError(webtransport.SessionErrorCode(0), "")
		return rc.ctx.Err() != nil
	}
	rc.mu.Lock()
	rc.stream, rc.sess = str, sess
	rc.gen++
	rc.haveState = false
	rc.mu.Unlock()
	defer func() {
		rc.mu.Lock()
		rc.stream, rc.sess = nil, nil
		rc.mu.Unlock()
		_ = sess.CloseWithError(webtransport.SessionErrorCode(0), "")
	}()

	hello, err := wire.AppendRoomHello(nil, wire.RoomHello{
		Protocol:   wire.RoomProtocolVersion,
		ClientKind: wire.RoomClientNative,
		Nickname:   rc.s.nickname(),
	})
	if err != nil {
		rc.fail(&RoomError{Op: "join", Err: fmt.Errorf("bad room hello: %w", err)})
		return true
	}
	if err := rc.send(hello); err != nil {
		rc.log.Warn("room hello not sent", "err", err)
		return rc.ctx.Err() != nil
	}

	// A pending Read does not watch ctx: stopping must actively unblock it,
	// exactly as readServerMessage does.
	stopWatch := context.AfterFunc(rc.ctx, func() {
		_ = str.SetReadDeadline(time.Now().Add(-time.Second))
	})
	defer stopWatch()

	for {
		rec, err := readRoomRecord(str)
		if err != nil {
			if rc.ctx.Err() != nil {
				return true
			}
			return rc.ended(sess, err)
		}
		if terminal := rc.handle(rec); terminal {
			return true
		}
	}
}

// ended classifies a dead connection by its close code.
func (rc *roomClient) ended(sess RoomSession, readErr error) (terminal bool) {
	code, ok := roomCloseCode(sess)
	rc.mu.Lock()
	reason := rc.endReason
	rc.mu.Unlock()
	switch {
	case ok && code == roomCloseCodeEnded:
		// Terminal for the room only: the broadcast keeps publishing and the
		// publisher's supervisor never sees this code.
		rc.log.Info("room ended", "reason", reason)
		rc.s.cb.roomEnded(reason)
		return true
	case ok && code == http.StatusNotFound:
		// Post-upgrade join failure: the room ended between the dial and the
		// hello. Same outcome as 4007 without a RoomEnding event.
		rc.log.Info("room ended before the join completed")
		rc.s.cb.roomEnded(reason)
		return true
	case ok && code == http.StatusBadRequest:
		// The relay says we broke the protocol. Reconnecting would only
		// repeat it; surface it as the bug it is.
		rc.fail(&RoomError{Op: "join", Err: fmt.Errorf("the relay closed the room session as malformed (code %d)", code)})
		return true
	}
	rc.log.Info("room session ended", "close_code", code, "has_code", ok, "err", readErr)
	return false
}

// handle dispatches one record. It reports whether the client is done.
func (rc *roomClient) handle(rec []byte) (terminal bool) {
	switch rec[1] {
	case wire.TypeRoomState:
		st, err := wire.ParseRoomState(rec)
		if err != nil {
			rc.log.Warn("room state parse failed", "err", err)
			return false
		}
		rc.onState(cloneRoomState(st))
	case wire.TypeRoomEvent:
		ev, err := wire.ParseRoomEvent(rec)
		if err != nil && !errors.Is(err, wire.ErrUnknownRoomKind) {
			rc.log.Warn("room event parse failed", "err", err)
			return false
		}
		rc.onEvent(ev, err != nil)
	default:
		rc.log.Debug("room record ignored: unknown type", "type", rec[1])
	}
	return false
}

func (rc *roomClient) onState(st wire.RoomState) {
	rc.mu.Lock()
	rc.lastSeq = st.Seq
	rc.haveState = true
	rc.code = st.Code
	created := false
	if rc.mode == roomModeNew {
		// The mint's first snapshot: it carries the creator token exactly
		// once (docs/44 §4.4), and the broadcast is already attached. From
		// here on this is a join by code, with the creator grant presented
		// on every reconnect.
		//
		// attachedGen is deliberately NOT marked: a minted attachment has no
		// owner participant on the relay (measured against RM2: the minter
		// carries no STREAMING flag and its detach goes the creator route),
		// so the idempotent Attach that follows claims ownership — the
		// relay documents it as a label/ownership refresh.
		rc.mode = roomModeJoin
		if len(st.CreatorToken) == wire.RoomCreatorTokenSize {
			rc.creatorToken = hex.EncodeToString(st.CreatorToken)
			created = true
		}
	}
	token := rc.creatorToken
	rc.mu.Unlock()
	// The code is joinable: the key is the handle the logs get (D16).
	rc.log.Info("room state", "room_key", hex.EncodeToString(st.Key), "seq", st.Seq,
		"attachments", len(st.Attachments), "participants", len(st.Participants))
	if created {
		rc.s.cb.roomCreated(st.Code, token)
	}
	rc.s.cb.roomState(st)
	rc.maybeAttach()
}

func (rc *roomClient) onEvent(ev wire.RoomEvent, unknown bool) {
	rc.mu.Lock()
	gap := ev.Seq > rc.lastSeq+1
	if ev.Seq > rc.lastSeq {
		rc.lastSeq = ev.Seq
	}
	if ev.Kind == wire.RoomEventRoomEnding {
		rc.endReason = ev.Reason
	}
	rc.mu.Unlock()
	if gap {
		// A missed delta: the snapshot that follows replaces our state and
		// resets the sequence (docs/44 §4.6).
		rc.log.Info("room event gap; resyncing", "seq", ev.Seq)
		if msg, err := wire.AppendRoomCommand(nil, wire.RoomCommand{Kind: wire.RoomCommandResync}); err == nil {
			if err := rc.send(msg); err != nil {
				rc.log.Debug("room resync not sent", "err", err)
			}
		}
	}
	if unknown {
		return
	}
	switch ev.Kind {
	case wire.RoomEventCommandRejected:
		rerr := &RoomRejectError{Command: ev.Command, Reason: ev.Reason, Detail: ev.Message}
		rc.log.Warn("room command rejected", "command", ev.Command, "reason", ev.Reason, "detail", ev.Message)
		rc.s.cb.roomError(rerr)
	case wire.RoomEventAttachmentRemoved:
		if id, _ := rc.s.credentials(); id != "" && ev.Attachment.BroadcastID == id {
			rc.detachedOnce.Do(func() { close(rc.detached) })
		}
	}
	rc.s.cb.roomEvent(ev)
}

// maybeAttach is the latch: an Attach goes out once per generation, and
// only when the stream is up, the snapshot has arrived, and BOTH the
// broadcast ID and the resume token are known.
func (rc *roomClient) maybeAttach() {
	rc.mu.Lock()
	if rc.stream == nil || !rc.haveState || rc.attachedGen == rc.gen {
		rc.mu.Unlock()
		return
	}
	id, tokHex := rc.s.credentials()
	if id == "" || tokHex == "" {
		rc.mu.Unlock()
		return
	}
	raw, err := hex.DecodeString(tokHex)
	if err != nil || len(raw) != wire.ResumeTokenSize {
		rc.mu.Unlock()
		rc.log.Warn("resume token unusable as attach proof", "err", err, "len", len(raw))
		return
	}
	rc.attachedGen = rc.gen
	label := rc.label
	rc.mu.Unlock()

	msg, err := wire.AppendRoomCommand(nil, wire.RoomCommand{
		Kind:        wire.RoomCommandAttach,
		BroadcastID: id,
		ResumeToken: raw,
		Label:       label,
	})
	if err != nil {
		rc.s.cb.roomError(&RoomError{Op: "join", Err: fmt.Errorf("bad attach command: %w", err)})
		return
	}
	if err := rc.send(msg); err != nil {
		rc.log.Debug("room attach not sent", "err", err)
		rc.mu.Lock()
		rc.attachedGen = 0 // let the next generation try again
		rc.mu.Unlock()
	}
}

// detach sends a Detach for the broadcast and waits (briefly) for the relay
// to confirm it, so the session close that follows cannot overtake it.
func (rc *roomClient) detach() {
	id, _ := rc.s.credentials()
	rc.mu.Lock()
	attached := rc.attachedGen != 0 && rc.stream != nil
	rc.mu.Unlock()
	if id == "" || !attached {
		return
	}
	msg, err := wire.AppendRoomCommand(nil, wire.RoomCommand{Kind: wire.RoomCommandDetach, BroadcastID: id})
	if err != nil {
		return
	}
	if err := rc.send(msg); err != nil {
		rc.log.Debug("room detach not sent", "err", err)
		return
	}
	select {
	case <-rc.detached:
	case <-rc.ctx.Done():
	case <-time.After(roomDetachAckTimeout):
		rc.log.Warn("room detach not confirmed in time")
	}
}

// send frames and writes one message on the control stream.
func (rc *roomClient) send(msg []byte) error {
	rc.mu.Lock()
	str := rc.stream
	rc.mu.Unlock()
	if str == nil {
		return errors.New("engine: room session not connected")
	}
	rec, err := wire.AppendRoomRecord(nil, msg)
	if err != nil {
		return err
	}
	rc.wmu.Lock()
	defer rc.wmu.Unlock()
	_ = str.SetWriteDeadline(time.Now().Add(roomWriteTimeout))
	_, err = str.Write(rec)
	return err
}

// fail reports a terminal room failure. The broadcast is unaffected.
func (rc *roomClient) fail(err error) {
	rc.log.Warn("room session failed", "err", err)
	rc.s.cb.roomError(err)
}

// readRoomRecord reads one framed record (Version ‖ Type ‖ payload). The
// length is validated before any allocation.
func readRoomRecord(r io.Reader) ([]byte, error) {
	var hdr [wire.RoomRecordHeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n, err := wire.ParseRoomRecordLength(hdr[:])
	if err != nil {
		return nil, err
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// roomCloseCode reads the close code a dead room session carries, if any —
// sessionCloseCode's mechanism on the bidirectional opener: a closed session
// hands the close error back before touching the connection. Only called
// once the read loop has failed, i.e. the session is done.
func roomCloseCode(sess RoomSession) (uint32, bool) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := sess.OpenStreamSync(ctx)
	var se *webtransport.SessionError
	if errors.As(err, &se) {
		return uint32(se.ErrorCode), true
	}
	return 0, false
}

// cloneRoomState detaches a parsed state from the record it aliases, so a
// shell may keep it past the next read.
func cloneRoomState(st wire.RoomState) wire.RoomState {
	if st.CreatorToken != nil {
		st.CreatorToken = append([]byte(nil), st.CreatorToken...)
	}
	if st.Key != nil {
		st.Key = append([]byte(nil), st.Key...)
	}
	st.Attachments = append([]wire.RoomAttachment(nil), st.Attachments...)
	st.Participants = append([]wire.RoomParticipant(nil), st.Participants...)
	return st
}

func (s *Session) nickname() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cfg.Nickname != "" {
		return s.cfg.Nickname
	}
	return DefaultNickname
}

func (s *Session) relayURL() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.RelayURL
}

func (s *Session) dialParams() (origin string, insecure bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	origin = s.cfg.Origin
	if origin == "" {
		origin = DefaultOrigin
	}
	return origin, s.cfg.Insecure
}
