package transport

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/internal/roomsrv"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// Room control sessions (R42, docs/44 §4.2, §4.6, RM2).
//
// CONNECT /room/new mints a dynamic room from a live broadcast; CONNECT
// /room/{code} joins one. Both upgrade to a WebTransport session on which the
// client opens ONE bidirectional stream and sends a RoomHello; the relay
// answers a RoomState and then streams RoomEvent deltas while reading
// RoomCommands. Media never touches this session — a participant's tiles
// are ordinary /subscribe sessions.
//
// The routes are registered only with -rooms on (New), so a relay without
// rooms is byte-identical to pre-R42 (docs/44 D17): nothing here is reachable.

const (
	// roomHelloTimeout bounds how long a joined session may take to open its
	// control stream and send RoomHello before the relay gives up on it.
	roomHelloTimeout = 10 * time.Second
	// roomWriteTimeout bounds one control record write to a participant;
	// past it the participant is treated as gone.
	roomWriteTimeout = 5 * time.Second
	// roomCloseBadRequest is the application close code for a session that
	// broke the control protocol (no hello, malformed record).
	roomCloseBadRequest = 400
)

// SetRooms installs the room registry (R42). Called once at startup from
// main, before Run; a nil registry — or never calling this — is the -rooms
// off shape, in which the room routes are not registered at all. Stored
// atomically for the same reason SetModeration is.
func (s *Server) SetRooms(reg *roomsrv.Registry) {
	if reg != nil {
		reg.SetTokens(s.resume)
	}
	s.rooms.Store(reg)
}

func (s *Server) roomRegistry() *roomsrv.Registry { return s.rooms.Load() }

// roomStatsSource lets the H3 /statusz route (built in New, before SetRooms
// runs) read the registry installed later. A nil registry yields nil, which
// omits the rooms section.
type roomStatsSource struct{ s *Server }

func (r roomStatsSource) Stats() map[string]roomsrv.RoomStats {
	reg := r.s.roomRegistry()
	if reg == nil {
		return nil
	}
	return reg.Stats()
}

// roomRejectBanned is rejectBanned for the room route: same 451, same log
// discipline (no raw code, no ban reason at Warn).
func (s *Server) roomRejectBanned(w http.ResponseWriter, r *http.Request, route string) bool {
	peer := remoteIP(r.RemoteAddr)
	rec, banned := s.banSet().BannedIP(peer, s.clock())
	if !banned {
		return false
	}
	s.metrics.Connection(route, metrics.OutcomeBanned)
	s.log.Warn("room rejected: banned", "route", route,
		"remote", r.RemoteAddr, "origin", r.Header.Get("Origin"), "target_type", string(rec.Target.Type))
	s.log.Debug("room ban detail", "remote", r.RemoteAddr, "target", rec.Target.Value, "ban_reason", rec.Reason)
	w.WriteHeader(http.StatusUnavailableForLegalReasons)
	return true
}

// handleRoomNew mints a dynamic room (docs/44 §4.4 step 1). Query params:
// broadcast (the live broadcast to attach), resume (its resume token, hex),
// label (tile label), create (the -room-create-secret), name (nickname
// fallback). Every gate answers pre-upgrade: 403 wrong create secret or
// resume token, 404 unknown broadcast, 409 broadcast already in a room, 429
// -max-rooms, 503 store unavailable.
func (s *Server) handleRoomNew(w http.ResponseWriter, r *http.Request) {
	const route = "room"
	if s.rejectedDraining(w, route) {
		return
	}
	if s.rateLimited(r) {
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	if s.roomRejectBanned(w, r, route) {
		return
	}
	reg := s.roomRegistry()
	if reg == nil {
		s.metrics.Connection(route, metrics.OutcomeNotFound)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	token, _ := hex.DecodeString(q.Get("resume"))
	res, err := reg.Mint(r.Context(), roomsrv.MintRequest{
		BroadcastID:  q.Get("broadcast"),
		ResumeToken:  token,
		Label:        q.Get("label"),
		CreateSecret: q.Get("create"),
	})
	if err != nil {
		status, outcome := roomMintStatus(err)
		s.metrics.Connection(route, outcome)
		// The broadcast ID is joinable; the failure is logged by key only.
		s.log.Warn("room mint rejected", "remote", r.RemoteAddr, "origin", r.Header.Get("Origin"),
			"broadcast_key", s.broadcastKey(q.Get("broadcast")), "status", status, "err", err)
		w.WriteHeader(status)
		return
	}
	sess, err := s.wt.Upgrade(w, r)
	if err != nil {
		// The room exists with nobody in it; end it now rather than letting
		// the broadcast sit reserved for the empty grace.
		reg.EndRoom(res.Code, wire.RoomEndReasonEmpty)
		s.metrics.Connection(route, metrics.OutcomeUpgradeFailed)
		s.log.Warn("room mint upgrade failed", "err", err)
		w.WriteHeader(http.StatusForbidden) // no implicit 200 (finding 12)
		return
	}
	s.serveRoomSession(r, sess, reg, res.Code, roomsrv.Grants{Creator: true, AttachOK: true}, res.CreatorToken)
}

func roomMintStatus(err error) (int, string) {
	switch {
	case errors.Is(err, roomsrv.ErrForbidden):
		return http.StatusForbidden, metrics.OutcomeUnauthorized
	case errors.Is(err, roomsrv.ErrNotFound):
		return http.StatusNotFound, metrics.OutcomeNotFound
	case errors.Is(err, roomsrv.ErrAlreadyAttached):
		return http.StatusConflict, metrics.OutcomeConflict
	case errors.Is(err, roomsrv.ErrMaxRooms):
		return http.StatusTooManyRequests, metrics.OutcomeLimitRejected
	case errors.Is(err, roomsrv.ErrUnavailable):
		return http.StatusServiceUnavailable, metrics.OutcomeError
	}
	return http.StatusInternalServerError, metrics.OutcomeError
}

// handleRoom joins a room (docs/44 §4.2). Query params: creator (creator
// token, hex), attach (attach secret), name (nickname fallback). 404 for an
// unknown code, 403 for a wrong creator token or attach secret, 429 at the
// participant limit, 451 for a banned IP.
func (s *Server) handleRoom(w http.ResponseWriter, r *http.Request) {
	const route = "room"
	if s.rejectedDraining(w, route) {
		return
	}
	if s.rateLimited(r) {
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	if s.roomRejectBanned(w, r, route) {
		return
	}
	reg := s.roomRegistry()
	code := r.PathValue("code")
	if reg == nil || code == "" {
		s.metrics.Connection(route, metrics.OutcomeNotFound)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	grants, err := reg.CheckJoin(code, q.Get("creator"), q.Get("attach"), q.Has("attach"))
	if err != nil {
		var status int
		var outcome string
		switch {
		case errors.Is(err, roomsrv.ErrNotFound):
			status, outcome = http.StatusNotFound, metrics.OutcomeNotFound
		case errors.Is(err, roomsrv.ErrForbidden):
			status, outcome = http.StatusForbidden, metrics.OutcomeUnauthorized
		case errors.Is(err, roomsrv.ErrFull):
			status, outcome = http.StatusTooManyRequests, metrics.OutcomeLimitRejected
		default:
			status, outcome = http.StatusInternalServerError, metrics.OutcomeError
		}
		s.metrics.Connection(route, outcome)
		s.log.Warn("room join rejected pre-upgrade", "remote", r.RemoteAddr, "origin", r.Header.Get("Origin"),
			"room_key", s.broadcastKey(code), "status", status, "err", err)
		w.WriteHeader(status)
		return
	}
	sess, err := s.wt.Upgrade(w, r)
	if err != nil {
		s.metrics.Connection(route, metrics.OutcomeUpgradeFailed)
		s.log.Warn("room upgrade failed", "err", err)
		w.WriteHeader(http.StatusForbidden)
		return
	}
	s.serveRoomSession(r, sess, reg, code, grants, nil)
}

// serveRoomSession runs one control session to completion: accept the
// client's bidirectional stream, read RoomHello, join, then pump commands
// until the session or the room ends.
func (s *Server) serveRoomSession(r *http.Request, sess *webtransport.Session, reg *roomsrv.Registry, code string, grants roomsrv.Grants, creatorToken []byte) {
	const route = "room"
	defer s.trackSession(sess)()
	s.metrics.Connection(route, metrics.OutcomeAccepted)
	log := s.log.With("remote", sess.RemoteAddr(), "route", route, "room_key", s.broadcastKey(code))

	ctx := r.Context()
	helloCtx, cancel := context.WithTimeout(ctx, roomHelloTimeout)
	stream, err := sess.AcceptStream(helloCtx)
	if err != nil {
		cancel()
		log.Warn("room session opened no control stream", "err", err)
		sess.CloseWithError(roomCloseBadRequest, "no control stream")
		return
	}
	conn := &roomConn{sess: sess, stream: stream}
	rec, err := conn.read(helloCtx)
	cancel()
	if err != nil {
		log.Warn("room hello not received", "err", err)
		sess.CloseWithError(roomCloseBadRequest, "expected RoomHello")
		return
	}
	hello, err := wire.ParseRoomHello(rec)
	if err != nil {
		log.Warn("room hello malformed", "err", err)
		sess.CloseWithError(roomCloseBadRequest, "malformed RoomHello")
		return
	}
	if hello.Nickname == "" {
		hello.Nickname = r.URL.Query().Get("name")
	}
	p, err := reg.Join(code, hello, grants, creatorToken, conn)
	if err != nil {
		switch {
		case errors.Is(err, roomsrv.ErrFull):
			sess.CloseWithError(webtransport.SessionErrorCode(http.StatusTooManyRequests), "room full")
		default:
			sess.CloseWithError(webtransport.SessionErrorCode(http.StatusNotFound), "room ended")
		}
		log.Warn("room join rejected post-upgrade", "err", err)
		return
	}
	defer p.Leave()
	log.Info("room session started", "participant", p.ID(), "client_kind", hello.ClientKind,
		"creator", grants.Creator, "attach_ok", grants.AttachOK)

	for {
		rec, err := conn.read(ctx)
		if err != nil {
			log.Info("room session ended", "participant", p.ID(), "reason", sessionEndReason(ctx, err))
			return
		}
		cmd, err := wire.ParseRoomCommand(rec)
		if err != nil && !errors.Is(err, wire.ErrUnknownRoomKind) {
			log.Warn("room command malformed", "participant", p.ID(), "err", err)
			sess.CloseWithError(roomCloseBadRequest, "malformed RoomCommand")
			return
		}
		p.HandleCommand(cmd)
	}
}

// roomConn adapts one bidirectional stream + its session to roomsrv.Conn.
type roomConn struct {
	sess   *webtransport.Session
	stream *webtransport.Stream
	wmu    sync.Mutex
	closed atomic.Bool
}

func (c *roomConn) Write(ctx context.Context, record []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	deadline := time.Now().Add(roomWriteTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = c.stream.SetWriteDeadline(deadline)
	_, err := c.stream.Write(record)
	return err
}

func (c *roomConn) Close(code uint32, reason string) {
	if c.closed.CompareAndSwap(false, true) {
		_ = c.sess.CloseWithError(webtransport.SessionErrorCode(code), reason)
	}
}

// read returns the next framed message (Version ‖ Type ‖ payload) or an
// error. The length prefix is validated before any allocation.
func (c *roomConn) read(ctx context.Context) ([]byte, error) {
	if d, ok := ctx.Deadline(); ok {
		_ = c.stream.SetReadDeadline(d)
	} else {
		_ = c.stream.SetReadDeadline(time.Time{})
	}
	var hdr [wire.RoomRecordHeaderSize]byte
	if _, err := io.ReadFull(c.stream, hdr[:]); err != nil {
		return nil, err
	}
	n, err := wire.ParseRoomRecordLength(hdr[:])
	if err != nil {
		return nil, err
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.stream, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
