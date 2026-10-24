package transport

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/broadcastid"
	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/internal/roomcluster"
	"github.com/Tuhis/gawk/gawk-server/internal/roomsrv"
	"github.com/Tuhis/gawk/gawk-server/rooms"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// Room control over a PoP link, and the party wall's vouched attaches
// (R74, docs/76 items 33-36).
//
// A party guest's room control session goes to the PoP. While the PoP is
// linked it sends the join home on a bidirectional stream of its link: the
// PoP writes a poplink RoomOpen record (popclient), then the guest's raw
// room records follow. Home answers on the same stream with a two-byte
// big-endian status (200: joined; otherwise the HTTP status the join route
// would have answered, and the stream ends), then the room's raw records,
// and ends with a close record: a zero length prefix (no room record is
// shorter than 2 bytes, wire.ParseRoomRecordLength) followed by a u32
// close code, a u8 reason length and the reason. The PoP turns that into
// its guest's session close, so a 4007 stays terminal and a lost home
// reads as 4002. Home routes the stream as R42 routes any join: served
// here when this pod homes the room, else piped to the room's home pod's
// /internal/room, else adopted.
//
// A wall's tiles arrive VOUCHED: a wall record on the PoP's authenticated
// link, for an ID that PoP reserved, becomes AttachVouched/DetachVouched on
// the wall's home pod (POST /internal/roomvouch/{code} when that is not
// this pod). Neither carries media.

// roomLinkClose is the close record's marker: a room record length of 0.
const roomLinkCloseMarker = 0

// maxRoomLinkCloseReason caps the close record's reason.
const maxRoomLinkCloseReason = 255

// roomVouchTimeout bounds one vouched attach or detach on the wall's home.
const roomVouchTimeout = 5 * time.Second

// Wall ops (poplink.WallAttach / WallDetach; restated so transport keeps
// the record vocabulary to itself).
const (
	WallOpAttach = "attach"
	WallOpDetach = "detach"
)

// RoomForwarder is a linked PoP's way home for room joins (popclient
// installs it). It must not block for long: the join waits on it.
type RoomForwarder interface {
	// ForwardRoom reports whether a /room/{code} join is served by the home
	// fleet. local says this relay's own registry has the room (a dynamic
	// room minted here, or the local wall).
	ForwardRoom(code string, local bool) bool
	// OpenRoom opens a room stream on the link and writes its RoomOpen
	// record; home's status and records follow on the returned stream.
	OpenRoom(ctx context.Context, path, rawQuery string) (io.ReadWriteCloser, error)
	// WallCommand handles an Attach or Detach of a broadcast the guest
	// proved it publishes here (its resume token checked against this
	// relay's key): Show and Hide on the wall, forwarded vouched. false
	// sends the command home untouched.
	WallCommand(code string, cmd wire.RoomCommand) bool
}

type roomForwarderBox struct{ f RoomForwarder }

// SetRoomForwarder installs the PoP's room forwarder; nil removes it. Safe
// while serving.
func (s *Server) SetRoomForwarder(f RoomForwarder) {
	if f == nil {
		s.roomForwarder.Store(nil)
		return
	}
	s.roomForwarder.Store(&roomForwarderBox{f: f})
}

func (s *Server) roomForwarderOrNil() RoomForwarder {
	if b := s.roomForwarder.Load(); b != nil {
		return b.f
	}
	return nil
}

// RoomRegistry is the installed room registry (nil with -rooms off): a
// PoP's wall keeps its local read-only wall there.
func (s *Server) RoomRegistry() *roomsrv.Registry { return s.roomRegistry() }

// forwardRoomJoin is handleRoom's PoP gate: true means the join went home
// (or was answered trying).
func (s *Server) forwardRoomJoin(w http.ResponseWriter, r *http.Request, reg *roomsrv.Registry, code string) bool {
	fwd := s.roomForwarderOrNil()
	if fwd == nil {
		return false
	}
	norm, err := rooms.NormalizeCode(code)
	if err != nil {
		return false
	}
	local := false
	if reg != nil {
		_, local = reg.Lookup(norm)
	}
	if !fwd.ForwardRoom(norm, local) {
		return false
	}
	s.proxyRoomOverLink(w, r, fwd, norm)
	return true
}

// linkStream gives any link stream the deadlines roomConn and the pumps
// set; a stream without them (none in production) ignores them.
type linkStream struct {
	io.ReadWriteCloser
}

func (l linkStream) SetReadDeadline(t time.Time) error {
	if d, ok := l.ReadWriteCloser.(interface{ SetReadDeadline(time.Time) error }); ok {
		return d.SetReadDeadline(t)
	}
	return nil
}

func (l linkStream) SetWriteDeadline(t time.Time) error {
	if d, ok := l.ReadWriteCloser.(interface{ SetWriteDeadline(time.Time) error }); ok {
		return d.SetWriteDeadline(t)
	}
	return nil
}

func writeRoomLinkStatus(w linkStream, status int) error {
	_ = w.SetWriteDeadline(time.Now().Add(roomWriteTimeout))
	defer w.SetWriteDeadline(time.Time{})
	var b [2]byte
	binary.BigEndian.PutUint16(b[:], uint16(status))
	_, err := w.Write(b[:])
	return err
}

func readRoomLinkStatus(r linkStream, deadline time.Time) (int, error) {
	_ = r.SetReadDeadline(deadline)
	defer r.SetReadDeadline(time.Time{})
	var b [2]byte
	if _, err := io.ReadFull(r, b[:]); err != nil {
		return 0, err
	}
	return int(binary.BigEndian.Uint16(b[:])), nil
}

// roomLinkClosed is the close record as the PoP reads it.
type roomLinkClosed struct {
	code   uint32
	reason string
}

func (c *roomLinkClosed) Error() string {
	return fmt.Sprintf("room closed by home (%d %s)", c.code, c.reason)
}

func appendRoomLinkClose(dst []byte, code uint32, reason string) []byte {
	if len(reason) > maxRoomLinkCloseReason {
		reason = reason[:maxRoomLinkCloseReason]
	}
	dst = binary.BigEndian.AppendUint16(dst, roomLinkCloseMarker)
	dst = binary.BigEndian.AppendUint32(dst, code)
	dst = append(dst, byte(len(reason)))
	return append(dst, reason...)
}

// readRoomFrame reads one framed room record (prefix included). From home
// it may be the close record instead, returned as *roomLinkClosed.
func readRoomFrame(r io.Reader, allowClose bool) ([]byte, error) {
	var hdr [wire.RoomRecordHeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	if allowClose && binary.BigEndian.Uint16(hdr[:]) == roomLinkCloseMarker {
		var head [5]byte
		if _, err := io.ReadFull(r, head[:]); err != nil {
			return nil, err
		}
		reason := make([]byte, head[4])
		if _, err := io.ReadFull(r, reason); err != nil {
			return nil, err
		}
		return nil, &roomLinkClosed{code: binary.BigEndian.Uint32(head[:4]), reason: string(reason)}
	}
	n, err := wire.ParseRoomRecordLength(hdr[:])
	if err != nil {
		return nil, err
	}
	buf := make([]byte, wire.RoomRecordHeaderSize+n)
	copy(buf, hdr[:])
	if _, err := io.ReadFull(r, buf[wire.RoomRecordHeaderSize:]); err != nil {
		return nil, err
	}
	return buf, nil
}

// --- the PoP side -------------------------------------------------------

// proxyRoomOverLink forwards one guest's room control session home over the
// PoP's link. Pre-upgrade, home's status is the guest's: a 404 or 403 at
// home is a 404 or 403 here. Post-upgrade the records are piped, except an
// Attach or Detach of a broadcast the guest proved it publishes here, which
// the forwarder turns into a vouched wall record (Hide/Show). The proof is
// the broadcast's resume token from THIS relay: ?broadcast=&resume= on the
// join, or the token of an Attach the session sent.
func (s *Server) proxyRoomOverLink(w http.ResponseWriter, r *http.Request, fwd RoomForwarder, code string) {
	const route = "room-link"
	log := s.log.With("route", route, "room_key", s.broadcastKey(code))
	q := r.URL.Query()
	proven := map[string]bool{}
	if id, err := broadcastid.Normalize(q.Get("broadcast")); err == nil {
		if tok, err := hex.DecodeString(q.Get("resume")); err == nil && s.resume.VerifyResume(id, tok) {
			proven[id] = true
		}
	}
	// This relay's resume token never travels home: home could not check
	// it, and has no business holding it.
	q.Del("broadcast")
	q.Del("resume")

	dialCtx, cancel := context.WithTimeout(r.Context(), edgeAttachTimeout)
	raw, err := fwd.OpenRoom(dialCtx, "/room/"+code, q.Encode())
	cancel()
	if err != nil {
		s.metrics.Connection(route, metrics.OutcomeError)
		log.Warn("room join could not reach home over the link", "err", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	up := linkStream{raw}
	status, err := readRoomLinkStatus(up, time.Now().Add(edgeAttachTimeout))
	if err != nil || status != http.StatusOK {
		_ = up.Close()
		if err != nil {
			s.metrics.Connection(route, metrics.OutcomeError)
			log.Warn("room join over the link: no answer from home", "err", err)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		s.metrics.Connection(route, metrics.OutcomeUnauthorized)
		log.Info("room join over the link: home refused", "status", status)
		w.WriteHeader(status)
		return
	}
	sess, err := s.wt.Upgrade(w, r)
	if err != nil {
		_ = up.Close()
		s.metrics.Connection(route, metrics.OutcomeUpgradeFailed)
		log.Warn("room link upgrade failed", "err", err)
		w.WriteHeader(http.StatusForbidden)
		return
	}
	defer s.trackSession(sess)()
	s.sendSessionNotices(sess, r, kindRoom)
	s.metrics.Connection(route, metrics.OutcomeAccepted)
	log = log.With("remote", sess.RemoteAddr())

	ctx := r.Context()
	helloCtx, cancel := context.WithTimeout(ctx, roomHelloTimeout)
	client, err := sess.AcceptStream(helloCtx)
	cancel()
	if err != nil {
		log.Warn("room link: guest opened no control stream", "err", err)
		sess.CloseWithError(roomCloseBadRequest, "no control stream")
		_ = up.Close()
		return
	}
	log.Info("room link session started")

	fromGuest := make(chan error, 1)
	go func() {
		for {
			rec, err := readRoomFrame(client, false)
			if err != nil {
				fromGuest <- err
				return
			}
			if s.interceptWallCommand(fwd, code, rec[wire.RoomRecordHeaderSize:], proven) {
				continue
			}
			rec = vouchForHome(rec, proven)
			if _, err := up.Write(rec); err != nil {
				fromGuest <- err
				return
			}
		}
	}()
	fromHome := make(chan error, 1)
	go func() {
		for {
			rec, err := readRoomFrame(up, true)
			if err != nil {
				fromHome <- err
				return
			}
			_ = client.SetWriteDeadline(time.Now().Add(roomWriteTimeout))
			if _, err := client.Write(rec); err != nil {
				fromHome <- err
				return
			}
		}
	}()
	select {
	case err := <-fromGuest:
		log.Info("room link session ended", "reason", sessionEndReason(ctx, err))
		sess.CloseWithError(0, "")
		_ = up.Close()
	case upErr := <-fromHome:
		code, reason := uint32(wire.CloseCodeServerDraining), "room home lost"
		var closed *roomLinkClosed
		if errors.As(upErr, &closed) {
			code, reason = closed.code, closed.reason
		}
		log.Info("room link upstream ended", "close_code", code, "err", upErr)
		sess.CloseWithError(webtransport.SessionErrorCode(code), reason)
		_ = up.Close()
	}
}

// interceptWallCommand reports whether msg was an Attach or Detach the
// forwarder handled vouched. An Attach carrying this relay's valid resume
// token is itself a proof for the rest of the session.
func (s *Server) interceptWallCommand(fwd RoomForwarder, code string, msg []byte, proven map[string]bool) bool {
	if len(msg) < 2 || msg[1] != wire.TypeRoomCommand {
		return false
	}
	cmd, err := wire.ParseRoomCommand(msg)
	if err != nil || (cmd.Kind != wire.RoomCommandAttach && cmd.Kind != wire.RoomCommandDetach) {
		return false
	}
	if cmd.Kind == wire.RoomCommandAttach && s.resume.VerifyResume(cmd.BroadcastID, cmd.ResumeToken) {
		proven[cmd.BroadcastID] = true
	}
	if !proven[cmd.BroadcastID] {
		return false
	}
	return fwd.WallCommand(code, cmd)
}

// vouchForHome rewrites a proven Attach going home (one WallCommand did
// not take: a room at home, not the wall) so this relay's resume token
// does not travel: home could not check it, and has no business holding
// it. The token is zeroed and home takes the link's vouch instead
// (popVouch).
func vouchForHome(rec []byte, proven map[string]bool) []byte {
	msg := rec[wire.RoomRecordHeaderSize:]
	if len(msg) < 2 || msg[1] != wire.TypeRoomCommand {
		return rec
	}
	cmd, err := wire.ParseRoomCommand(msg)
	if err != nil || cmd.Kind != wire.RoomCommandAttach || !proven[cmd.BroadcastID] {
		return rec
	}
	cmd.ResumeToken = make([]byte, wire.ResumeTokenSize)
	m, err := wire.AppendRoomCommand(nil, cmd)
	if err != nil {
		return rec
	}
	out, err := wire.AppendRoomRecord(nil, m)
	if err != nil {
		return rec
	}
	return out
}

// --- the home side ------------------------------------------------------

// ServeRoomStream serves one room join a linked PoP sent over its link (the
// stream after its RoomOpen record). path is the join's /room/{code};
// /room/new never travels (a PoP mints its rooms locally). Statuses are the
// join route's: 404 unknown, 403 a wrong creator token or attach secret,
// 429 full, 503 the room's home unreachable. The PoP's guests skip the rate
// limit and the IP-ban check, as every /pop/* route does: they share the
// venue's address, and the PoP applied its own.
func (s *Server) ServeRoomStream(ctx context.Context, pop, path, rawQuery string, raw io.ReadWriteCloser) {
	const route = "pop-room"
	st := linkStream{raw}
	defer st.Close()
	reg := s.roomRegistry()
	code, ok := strings.CutPrefix(path, "/room/")
	norm, err := rooms.NormalizeCode(code)
	if reg == nil || !ok || code == "new" || err != nil {
		s.metrics.Connection(route, metrics.OutcomeNotFound)
		_ = writeRoomLinkStatus(st, http.StatusNotFound)
		return
	}
	q, _ := url.ParseQuery(rawQuery)
	log := s.log.With("route", route, "pop", pop, "room_key", s.broadcastKey(norm))

	// The cluster gate, as roomHomedElsewhere decides it: held here or no
	// CR serves locally; a live foreign home is piped to; anything else is
	// adopted first.
	if rc := s.roomClusterWiring(); rc != nil {
		if _, held := rc.store.Holding(norm); !held {
			home, found := rc.store.Resolve(norm)
			elsewhere := func(h roomcluster.Home) bool { return h.Live && h.Holder != rc.podName }
			if found && !elsewhere(home) {
				switch err := rc.store.Adopt(ctx, norm); {
				case err == nil:
					found = false // ours now: serve locally
				case errors.Is(err, roomcluster.ErrHeldElsewhere):
					if home, found = rc.store.Resolve(norm); !found || !elsewhere(home) {
						_ = writeRoomLinkStatus(st, http.StatusServiceUnavailable)
						return
					}
				case errors.Is(err, roomcluster.ErrNotFound):
					_ = writeRoomLinkStatus(st, http.StatusNotFound)
					return
				default:
					log.Warn("pop room: adoption failed", "err", err)
					_ = writeRoomLinkStatus(st, http.StatusServiceUnavailable)
					return
				}
			}
			if found {
				s.pipeRoomStreamHome(ctx, st, rc, norm, pop, home, q, log)
				return
			}
		}
	}

	grants, err := reg.CheckJoin(norm, q.Get("creator"), q.Get("attach"), q.Has("attach"))
	if err != nil {
		status, outcome := roomJoinStatus(err)
		s.metrics.Connection(route, outcome)
		log.Info("pop room join rejected", "status", status, "err", err)
		_ = writeRoomLinkStatus(st, status)
		return
	}
	if err := writeRoomLinkStatus(st, http.StatusOK); err != nil {
		return
	}
	s.metrics.Connection(route, metrics.OutcomeAccepted)
	grants.VouchedBy = s.popVouch(pop)
	conn := &roomConn{stream: st, inBand: true, closeFn: func(code uint32, reason string) {
		_ = st.SetWriteDeadline(time.Now().Add(roomWriteTimeout))
		_, _ = st.Write(appendRoomLinkClose(nil, code, reason))
		_ = st.Close()
	}}
	defer s.trackRoomSession(norm, conn)()
	s.runRoomControl(ctx, log, conn, reg, norm, grants, nil, q.Get("name"))
}

// pipeRoomStreamHome is proxyRoom for a link stream: the join goes to the
// room's home pod over /internal/room, and the home's close code comes back
// as the close record.
func (s *Server) pipeRoomStreamHome(ctx context.Context, st linkStream, rc *roomClusterWiring, code, pop string,
	home roomcluster.Home, q url.Values, log *slog.Logger) {
	dialCtx, cancel := context.WithTimeout(ctx, edgeAttachTimeout)
	path := roomProxyPath(code, home.Generation, q) + "&" + popVouchParam + "=" + url.QueryEscape(pop)
	status, up, err := rc.dial(dialCtx, home.Addr, path)
	cancel()
	if err != nil {
		if status == 0 {
			status = http.StatusServiceUnavailable
			log.Warn("pop room: home unreachable", "err", err)
		}
		_ = writeRoomLinkStatus(st, status)
		return
	}
	upstream, err := up.sess.OpenStreamSync(ctx)
	if err != nil {
		up.close(0, "stream open failed")
		_ = writeRoomLinkStatus(st, http.StatusServiceUnavailable)
		return
	}
	if err := writeRoomLinkStatus(st, http.StatusOK); err != nil {
		up.close(0, "pop left")
		return
	}
	defer s.trackProxied(code, home.Kind)()
	fromPop := make(chan error, 1)
	go func() {
		_, err := io.Copy(upstream, st)
		fromPop <- err
	}()
	fromHome := make(chan error, 1)
	go func() {
		_, err := io.Copy(st, upstream)
		fromHome <- err
	}()
	select {
	case <-fromPop:
		up.close(0, "participant left")
	case upErr := <-fromHome:
		code, reason := uint32(wire.CloseCodeServerDraining), "room home lost"
		var se *webtransport.SessionError
		if errors.As(upErr, &se) {
			code, reason = uint32(se.ErrorCode), "room home closed"
		}
		_ = st.SetWriteDeadline(time.Now().Add(roomWriteTimeout))
		_, _ = st.Write(appendRoomLinkClose(nil, code, reason))
		up.close(0, "")
	}
}

// popVouchParam names, on /internal/room, the PoP whose link a piped room
// stream came over (pipeRoomStreamHome), so the room's home pod accepts
// that PoP's vouched attaches as the link-side pod would.
const popVouchParam = "vouch"

// popVouch is a room session's vouched-attach proof for a stream over
// pop's link (docs/76 item 34): the PoP verified the broadcaster's resume
// token against its own key, which home cannot do, so home accepts the
// Attach of a broadcast reserved for that PoP, as it accepts the wall's
// vouched records. A broadcast reserved for nobody, or for another PoP,
// still needs home's own token.
func (s *Server) popVouch(pop string) func(id string) bool {
	return func(id string) bool {
		if coord := s.clusterCoord(); coord != nil {
			o, _, _, ok := coord.Lookup(id)
			return ok && o.Pop == pop
		}
		if h := s.popDemand(); h != nil {
			owner, ok := h.ReservedFor(id)
			return ok && owner == pop
		}
		return false
	}
}

// roomVouchRequest is the POST /internal/roomvouch/{code} body.
type roomVouchRequest struct {
	Op    string `json:"op"`
	Pop   string `json:"pop"`
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
}

// VouchWall applies one wall record for pop on the wall's home: this pod
// when it homes code (or no cluster is wired), the room's home pod over
// /internal/roomvouch otherwise, after an adoption when nobody homes it.
// The caller (popsrv) has authenticated the link and checked that pop
// reserved id. The errors are roomsrv's sentinels.
func (s *Server) VouchWall(ctx context.Context, pop, op, code, id, label string) error {
	reg := s.roomRegistry()
	if reg == nil {
		return roomsrv.ErrDisabled
	}
	norm, err := rooms.NormalizeCode(code)
	if err != nil {
		return roomsrv.ErrNotFound
	}
	req := roomVouchRequest{Op: op, Pop: pop, ID: id, Label: label}
	if rc := s.roomClusterWiring(); rc != nil {
		if _, held := rc.store.Holding(norm); !held {
			if home, found := rc.store.Resolve(norm); found {
				if home.Live && home.Holder != rc.podName {
					return rc.vouch(ctx, home.Addr, norm, home.Generation, req)
				}
				switch err := rc.store.Adopt(ctx, norm); {
				case err == nil:
				case errors.Is(err, roomcluster.ErrHeldElsewhere):
					if home, ok := rc.store.Resolve(norm); ok && home.Live && home.Holder != rc.podName {
						return rc.vouch(ctx, home.Addr, norm, home.Generation, req)
					}
					return roomsrv.ErrUnavailable
				case errors.Is(err, roomcluster.ErrNotFound):
					return roomsrv.ErrNotFound
				default:
					return fmt.Errorf("%w: %v", roomsrv.ErrUnavailable, err)
				}
			}
		}
	}
	return vouchLocal(reg, norm, req)
}

func vouchLocal(reg *roomsrv.Registry, code string, req roomVouchRequest) error {
	switch req.Op {
	case WallOpAttach:
		return reg.AttachVouched(code, req.Pop, req.ID, req.Label)
	case WallOpDetach:
		return reg.DetachVouched(code, req.Pop, req.ID)
	}
	return fmt.Errorf("%w: wall op %q", roomsrv.ErrForbidden, req.Op)
}

// vouchStatus maps vouchLocal's sentinels onto the internal route's
// statuses, and vouchError back.
func vouchStatus(err error) int {
	switch {
	case err == nil:
		return http.StatusNoContent
	case errors.Is(err, roomsrv.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, roomsrv.ErrForbidden):
		return http.StatusForbidden
	case errors.Is(err, roomsrv.ErrAlreadyAttached):
		return http.StatusConflict
	case errors.Is(err, roomsrv.ErrMaxBroadcasts):
		return http.StatusTooManyRequests
	}
	return http.StatusServiceUnavailable
}

func vouchError(status int) error {
	switch status {
	case http.StatusNoContent, http.StatusOK:
		return nil
	case http.StatusNotFound:
		return roomsrv.ErrNotFound
	case http.StatusForbidden:
		return roomsrv.ErrForbidden
	case http.StatusConflict:
		return roomsrv.ErrAlreadyAttached
	case http.StatusTooManyRequests:
		return roomsrv.ErrMaxBroadcasts
	}
	return fmt.Errorf("%w: wall home answered %d", roomsrv.ErrUnavailable, status)
}

// roomVoucher sends one vouched wall op to the room's home pod.
type roomVoucher func(ctx context.Context, addr, code string, gen int64, req roomVouchRequest) error

// newRoomVoucher is the internal room dialer's twin for the vouch POST:
// same server name, roots and PSK, HTTP/3 on the pod's listener.
func newRoomVoucher(serverName, psk string, rootCAs *x509.CertPool) roomVoucher {
	tr := &http3.Transport{
		TLSClientConfig: &tls.Config{ServerName: serverName, RootCAs: rootCAs},
		QUICConfig:      &quic.Config{MaxIdleTimeout: edgeIdleTimeout, KeepAlivePeriod: edgeKeepAlive},
	}
	hc := &http.Client{Transport: tr, Timeout: roomVouchTimeout}
	return func(ctx context.Context, addr, code string, gen int64, req roomVouchRequest) error {
		body, err := json.Marshal(req)
		if err != nil {
			return err
		}
		target := "https://" + addr + "/internal/roomvouch/" + url.PathEscape(code) +
			"?gen=" + strconv.FormatInt(gen, 10) + "&psk=" + url.QueryEscape(psk)
		hr, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(string(body)))
		if err != nil {
			return err
		}
		hr.Header.Set("Content-Type", "application/json")
		rsp, err := hc.Do(hr)
		if err != nil {
			// The URL carries the PSK: never let it reach a log.
			var ue *url.Error
			if errors.As(err, &ue) {
				err = ue.Err
			}
			return fmt.Errorf("%w: wall home %s: %v", roomsrv.ErrUnavailable, addr, err)
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(rsp.Body, 4096))
		_ = rsp.Body.Close()
		return vouchError(rsp.StatusCode)
	}
}

// handleInternalRoomVouch is POST /internal/roomvouch/{code}: a vouched
// wall op from the pod that holds the PoP's link. Gates as
// /internal/room's: 404 without cluster wiring, 401 bad PSK, 404 not the
// home, 409 stale generation. Then the op's own status (vouchStatus).
func (s *Server) handleInternalRoomVouch(w http.ResponseWriter, r *http.Request) {
	const route = "internal-roomvouch"
	rc := s.roomClusterWiring()
	reg := s.roomRegistry()
	if rc == nil || reg == nil {
		s.metrics.Connection(route, metrics.OutcomeNotFound)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	if subtle.ConstantTimeCompare([]byte(q.Get("psk")), []byte(s.cfg.InternalPSK)) != 1 {
		s.metrics.Connection(route, metrics.OutcomeUnauthorized)
		s.log.Warn("internal room vouch unauthorized: bad PSK", "remote", r.RemoteAddr)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	norm, err := rooms.NormalizeCode(r.PathValue("code"))
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	heldGen, held := rc.store.Holding(norm)
	if !held {
		s.metrics.Connection(route, metrics.OutcomeNotFound)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if gen, err := strconv.ParseInt(q.Get("gen"), 10, 64); err != nil || gen != heldGen {
		s.metrics.Connection(route, metrics.OutcomeConflict)
		w.WriteHeader(http.StatusConflict)
		return
	}
	var req roomVouchRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	err = vouchLocal(reg, norm, req)
	s.metrics.Connection(route, metrics.OutcomeAccepted)
	if err != nil {
		s.log.Info("internal room vouch refused", "room_key", s.broadcastKey(norm), "pop", req.Pop,
			"broadcast_key", s.broadcastKey(req.ID), "op", req.Op, "err", err)
	}
	w.WriteHeader(vouchStatus(err))
}
