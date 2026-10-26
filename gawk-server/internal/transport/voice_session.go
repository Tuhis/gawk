package transport

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-server/internal/metrics"
	"github.com/Tuhis/gawk/gawk-server/internal/roomcluster"
	"github.com/Tuhis/gawk/gawk-server/internal/roomsrv"
	"github.com/Tuhis/gawk/gawk-server/rooms"
	"github.com/Tuhis/gawk/gawk-server/voice"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// The session links (R79, docs/80 D2, D5, §11.3, §11.5). The bridge pod
// that accepted a browser's voice session dials CONNECT
// /internal/voice/{code}?psk=<voice PSK> through the relay's Service, opens
// one bidirectional stream and sends Register (the browser's ticket and
// address). The room's home answers Accepted or a Close, then keeps the link
// for as long as the voice session lives: Joined and State keep the roster
// fresh, Left or the link closing ends the session, and a Close from the
// home is how a kick, a ban, a room end or a newer session reaches the
// browser (D17).
//
// A non-home pod in cluster mode proxies the link to the home, as
// /internal/room is proxied: CONNECT /internal/voice/{code}?psk=<cluster
// PSK>&gen=<lease generation>, the stream piped both ways. The home accepts
// either credential, the cluster one only at the current generation (409
// otherwise). Registered only with rooms and voice on.

// voiceRegisterTimeout bounds the wait for the Register after the upgrade.
const voiceRegisterTimeout = 10 * time.Second

// voiceCloseSettle is how long a Close message gets on the stream before the
// session close that follows it would discard it (the closeSettle lesson).
const voiceCloseSettle = 250 * time.Millisecond

// voiceRefusal maps a registration sentinel to its close code (CODE-REVIEW
// "error mapping at boundaries", docs/80 §11.5): retryable ones 4016, a gone
// room session 4002, the rest the terminal 4015.
func voiceRefusal(err error) (uint32, string) {
	switch {
	case errors.Is(err, roomsrv.ErrVoiceTicketExpired):
		return wire.CloseCodeVoiceUnavailable, "voice ticket expired"
	case errors.Is(err, roomsrv.ErrVoiceEpoch):
		return wire.CloseCodeVoiceUnavailable, "the room moved; ask for a new ticket"
	case errors.Is(err, roomsrv.ErrVoiceFull):
		return wire.CloseCodeVoiceUnavailable, "voice is full"
	case errors.Is(err, roomsrv.ErrVoiceNoParticipant):
		return wire.CloseCodeServerDraining, "room session ended"
	case errors.Is(err, roomsrv.ErrVoiceReplay):
		return wire.CloseCodeVoiceRemoved, "voice ticket already used"
	case errors.Is(err, roomsrv.ErrVoiceWrongRoom):
		return wire.CloseCodeVoiceRemoved, "voice ticket is for another room"
	case errors.Is(err, roomsrv.ErrVoiceNotLive):
		return wire.CloseCodeVoiceRemoved, "this room has no voice"
	}
	return wire.CloseCodeVoiceRemoved, "invalid voice ticket"
}

// handleInternalVoice is the session link route. Gates, in order: 404
// without rooms+voice, 401 neither PSK, 404 bad code; then, in cluster mode,
// a cluster-PSK dial must reach the home at its generation (404 / 409),
// while a bridge's dial is proxied to a live home elsewhere or adopts a room
// with none, as a join does.
func (s *Server) handleInternalVoice(w http.ResponseWriter, r *http.Request) {
	const route = "internal-voice"
	if s.rejectedDraining(w, route) {
		return
	}
	reg := s.roomRegistry()
	if reg == nil || !s.cfg.VoiceConfigured() {
		s.metrics.Connection(route, metrics.OutcomeNotFound)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	psk := []byte(q.Get("psk"))
	rc := s.roomClusterWiring()
	bridge := subtle.ConstantTimeCompare(psk, []byte(s.cfg.VoiceBridgePSK)) == 1
	fleet := !bridge && rc != nil && s.cfg.InternalPSK != "" && subtle.ConstantTimeCompare(psk, []byte(s.cfg.InternalPSK)) == 1
	if !bridge && !fleet {
		s.metrics.Connection(route, metrics.OutcomeUnauthorized)
		s.log.Warn("internal voice unauthorized: bad PSK", "remote", r.RemoteAddr)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	norm, err := rooms.NormalizeCode(r.PathValue("code"))
	if err != nil {
		s.metrics.Connection(route, metrics.OutcomeNotFound)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if fleet {
		heldGen, held := rc.store.Holding(norm)
		if !held {
			s.metrics.Connection(route, metrics.OutcomeNotFound)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if gen, err := strconv.ParseInt(q.Get("gen"), 10, 64); err != nil || gen != heldGen {
			s.metrics.Connection(route, metrics.OutcomeConflict)
			s.log.Warn("internal voice rejected: stale generation", "room_key", s.broadcastKey(norm), "got", q.Get("gen"), "held", heldGen)
			w.WriteHeader(http.StatusConflict)
			return
		}
	} else if s.homedElsewhere(w, r, norm, s.proxyVoice) {
		return
	}
	sess, err := s.wt.Upgrade(w, r)
	if err != nil {
		s.metrics.Connection(route, metrics.OutcomeUpgradeFailed)
		s.log.Warn("internal voice upgrade failed", "err", err)
		w.WriteHeader(http.StatusForbidden)
		return
	}
	defer s.trackSession(sess)()
	s.metrics.Connection(route, metrics.OutcomeAccepted)
	s.serveVoiceSession(r.Context(), sess, reg, norm)
}

// voiceSessionLink is the home's side of one session link: roomsrv's
// VoiceLink. Close sends the Close message, then ends the link a moment
// later, without blocking the caller.
type voiceSessionLink struct {
	sess   *webtransport.Session
	stream *webtransport.Stream
	wmu    sync.Mutex
	closed atomic.Bool
	done   chan struct{}
}

func (l *voiceSessionLink) send(m voice.Message) error {
	l.wmu.Lock()
	defer l.wmu.Unlock()
	_ = l.stream.SetWriteDeadline(time.Now().Add(roomWriteTimeout))
	return voice.WriteMessage(l.stream, m)
}

func (l *voiceSessionLink) Close(code uint32, reason string) {
	if !l.closed.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer close(l.done)
		if l.send(voice.Message{Type: voice.TypeClose, CloseCode: code, Reason: reason}) == nil {
			select {
			case <-l.sess.Context().Done():
			case <-time.After(voiceCloseSettle):
			}
		}
		_ = l.sess.CloseWithError(webtransport.SessionErrorCode(code), reason)
	}()
}

// wait blocks until a Close that was started has run (a refusal must not
// return from its handler first: the session would die with no code).
func (l *voiceSessionLink) wait() {
	if l.closed.Load() {
		<-l.done
	}
}

// serveVoiceSession runs one session link at the home to completion.
func (s *Server) serveVoiceSession(ctx context.Context, sess *webtransport.Session, reg *roomsrv.Registry, code string) {
	log := s.log.With("route", "internal-voice", "room_key", s.broadcastKey(code), "remote", sess.RemoteAddr())
	actx, cancel := context.WithTimeout(ctx, voiceRegisterTimeout)
	stream, err := sess.AcceptStream(actx)
	cancel()
	if err != nil {
		log.Warn("voice link opened no stream", "err", err)
		_ = sess.CloseWithError(roomCloseBadRequest, "no link stream")
		return
	}
	link := &voiceSessionLink{sess: sess, stream: stream, done: make(chan struct{})}
	defer link.wait()
	_ = stream.SetReadDeadline(time.Now().Add(voiceRegisterTimeout))
	msg, err := voice.ReadMessage(stream)
	_ = stream.SetReadDeadline(time.Time{})
	if err != nil || msg.Type != voice.TypeRegister {
		log.Warn("voice link: expected Register", "type", msg.Type, "err", err)
		_ = sess.CloseWithError(roomCloseBadRequest, "expected register")
		return
	}
	remote := remoteIP(msg.Remote)
	if rec, banned := s.banSet().BannedIP(remote, s.clock()); banned {
		s.metrics.Connection("voice-register", metrics.OutcomeBanned)
		log.Warn("voice registration refused: banned", "target_type", string(rec.Target.Type))
		link.Close(wire.CloseCodeVoiceRemoved, voiceReasonBannedWire)
		return
	}
	vs, acc, err := reg.RegisterVoice(code, msg.Ticket, remote, link)
	if err != nil {
		cc, reason := voiceRefusal(err)
		s.metrics.Connection("voice-register", metrics.OutcomeUnauthorized)
		log.Info("voice registration refused", "close_code", cc, "err", err)
		link.Close(cc, reason)
		return
	}
	defer vs.Left()
	s.metrics.Connection("voice-register", metrics.OutcomeAccepted)
	if err := link.send(voice.Message{Type: voice.TypeAccepted, Code: acc.Code, DisplayCode: acc.DisplayCode,
		Channel: acc.Channel, Temporary: acc.Temporary}); err != nil {
		log.Warn("voice link: accepted not delivered", "err", err)
		_ = sess.CloseWithError(0, "")
		return
	}
	log.Info("voice session started", "participant", vs.Participant())
	s.pumpVoiceLink(log, stream, vs)
}

// voiceReasonBannedWire is the refusal reason for a banned voice client;
// never the ban's own reason (operator-private).
const voiceReasonBannedWire = "banned"

// pumpVoiceLink reads the bridge's reports until the link ends.
func (s *Server) pumpVoiceLink(log *slog.Logger, stream io.Reader, vs *roomsrv.VoiceSession) {
	for {
		msg, err := voice.ReadMessage(stream)
		if err != nil {
			log.Info("voice session link ended", "participant", vs.Participant(), "err", err)
			return
		}
		switch msg.Type {
		case voice.TypeJoined:
			vs.Joined(msg.Session, msg.Name)
		case voice.TypeState:
			vs.State(msg.Session, msg.Flags)
		case voice.TypeLeft:
			log.Info("voice session left", "participant", vs.Participant(), "close_code", msg.CloseCode)
			return
		}
	}
}

// proxyVoice forwards one session link to the room's home (a non-home pod in
// cluster mode): the home's /internal/voice with the cluster PSK and the
// lease generation, one stream piped both ways, the upstream's close code
// passed through.
func (s *Server) proxyVoice(w http.ResponseWriter, r *http.Request, rc *roomClusterWiring, code string, home roomcluster.Home) {
	const route = "voice-proxy"
	log := s.log.With("route", route, "room_key", s.broadcastKey(code), "home", home.Holder)
	dialCtx, cancel := context.WithTimeout(r.Context(), edgeAttachTimeout)
	status, up, err := rc.dial(dialCtx, home.Addr, voice.SessionPath+code+"?gen="+strconv.FormatInt(home.Generation, 10))
	cancel()
	if err != nil {
		s.metrics.Connection(route, metrics.OutcomeError)
		log.Warn("voice proxy: home unreachable", "status", status, "err", err)
		if status == 0 {
			status = http.StatusServiceUnavailable
		}
		w.WriteHeader(status)
		return
	}
	sess, err := s.wt.Upgrade(w, r)
	if err != nil {
		up.close(0, "bridge upgrade failed")
		s.metrics.Connection(route, metrics.OutcomeUpgradeFailed)
		w.WriteHeader(http.StatusForbidden)
		return
	}
	defer s.trackSession(sess)()
	s.metrics.Connection(route, metrics.OutcomeAccepted)
	ctx := r.Context()
	actx, cancel := context.WithTimeout(ctx, voiceRegisterTimeout)
	client, err := sess.AcceptStream(actx)
	cancel()
	if err != nil {
		_ = sess.CloseWithError(roomCloseBadRequest, "no link stream")
		up.close(0, "bridge left")
		return
	}
	upstream, err := up.sess.OpenStreamSync(ctx)
	if err != nil {
		_ = sess.CloseWithError(webtransport.SessionErrorCode(wire.CloseCodeServerDraining), "room home lost")
		up.close(0, "")
		return
	}
	fromBridge := make(chan error, 1)
	go func() { _, err := io.Copy(upstream, client); fromBridge <- err }()
	fromHome := make(chan error, 1)
	go func() { _, err := io.Copy(client, upstream); fromHome <- err }()
	select {
	case <-fromBridge:
		_ = sess.CloseWithError(0, "")
		up.close(0, "bridge left")
	case upErr := <-fromHome:
		cc, reason := uint32(wire.CloseCodeServerDraining), "room home lost"
		var se *webtransport.SessionError
		if errors.As(upErr, &se) {
			cc, reason = uint32(se.ErrorCode), se.Message
		} else {
			// The home closed the stream cleanly after its Close message:
			// its session close carries the code, a moment later.
			select {
			case <-up.sess.Context().Done():
				if errors.As(context.Cause(up.sess.Context()), &se) {
					cc, reason = uint32(se.ErrorCode), se.Message
				}
			case <-time.After(time.Second):
			}
		}
		// The home's Close message is on its way to the bridge; let it be
		// read before this session's close discards it.
		select {
		case <-fromBridge:
		case <-time.After(voiceCloseSettle):
		}
		_ = sess.CloseWithError(webtransport.SessionErrorCode(cc), reason)
		up.close(0, "")
	}
}
