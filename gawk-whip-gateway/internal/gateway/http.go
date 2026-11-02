package gateway

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"

	"github.com/Tuhis/gawk/gawk-server/publisher"

	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/clientip"
	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/status"
	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/streamkey"
)

// maxOfferBytes bounds a POST body. OBS's offers are ~3 KB.
const maxOfferBytes = 64 << 10

// gatherTimeout bounds waiting for the answer's candidates. ICE-lite on a
// mux gathers synchronously, so this only matters if something is wrong.
const gatherTimeout = 3 * time.Second

// PublicHandler serves the WHIP API (RFC 9725) and D9's status, behind the
// public Ingress.
func (s *Server) PublicHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /whip", s.handlePost)
	mux.HandleFunc("OPTIONS /whip", s.preflight("OPTIONS, POST"))
	mux.HandleFunc("DELETE /whip/session/{id}", s.handleDelete)
	mux.HandleFunc("OPTIONS /whip/session/{id}", s.preflight("OPTIONS, DELETE"))
	mux.HandleFunc("PATCH /whip/session/{id}", func(w http.ResponseWriter, r *http.Request) {
		// No trickle ICE (§3.3: OBS gathers once; ICE-lite answers with
		// every candidate up front). RFC 9725 §4.3.1: 405 for a server that
		// doesn't support it.
		s.cors(w, r, "OPTIONS, DELETE")
		w.Header().Set("Allow", "OPTIONS, DELETE")
		http.Error(w, "trickle ICE is not supported", http.StatusMethodNotAllowed)
	})
	mux.HandleFunc("GET /whip/status", s.handleStatusGet)
	mux.HandleFunc("DELETE /whip/status", s.handleStatusDelete)
	mux.HandleFunc("OPTIONS /whip/status", s.preflight("OPTIONS, GET, DELETE"))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok\n") })
	return mux
}

// cors sets the CORS headers for an allowed Origin (D9: the gawk-app
// origins read /whip/status with the bearer key).
func (s *Server) cors(w http.ResponseWriter, r *http.Request, methods string) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return
	}
	allowed := len(s.opts.AllowedOrigins) == 0
	for _, o := range s.opts.AllowedOrigins {
		if o == "*" || strings.EqualFold(o, origin) {
			allowed = true
			break
		}
	}
	h := w.Header()
	h.Add("Vary", "Origin")
	if !allowed {
		return
	}
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Access-Control-Allow-Methods", methods)
	h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	h.Set("Access-Control-Expose-Headers", "Location, Retry-After")
	h.Set("Access-Control-Max-Age", "600")
}

func (s *Server) preflight(methods string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.cors(w, r, methods)
		w.Header().Set("Allow", methods)
		if strings.Contains(methods, "POST") {
			w.Header().Set("Accept-Post", "application/sdp")
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func retryAfter(w http.ResponseWriter, d time.Duration) {
	if d <= 0 {
		return
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(d.Seconds()))))
}

// httpRefusal is a POST answer other than 201.
type httpRefusal struct {
	status     int
	reason     string // recorded lastError (empty: nothing recorded)
	codec      string
	retryAfter time.Duration
	msg        string
}

// relayRefusal maps the relay's answer to a refused publish dial (D8).
func relayRefusal(st int) httpRefusal {
	switch st {
	case http.StatusUnavailableForLegalReasons:
		return httpRefusal{status: st, reason: status.ReasonBanned, msg: "banned by the relay operator"}
	case http.StatusConflict:
		return httpRefusal{status: st, reason: status.ReasonReserved, msg: "the code is reserved"}
	case http.StatusTooManyRequests:
		return httpRefusal{status: st, reason: status.ReasonRelayBusy, msg: "the relay is busy"}
	case http.StatusUnauthorized, http.StatusForbidden:
		// Never "banned": the relay keeps 451 for that (server.go rejectBanned).
		return httpRefusal{status: st, reason: status.ReasonRejected, msg: "the relay refused the key"}
	}
	// Unreachable (0), a 5xx, a 404 or a 400: nothing OBS can fix, and the
	// fixed reason set has no closer match than "no room right now".
	return httpRefusal{status: http.StatusServiceUnavailable, reason: status.ReasonRelayBusy, msg: "the relay is unavailable"}
}

func (s *Server) refuse(w http.ResponseWriter, r *http.Request, key *streamkey.Key, ref httpRefusal) {
	if ref.reason != "" && key != nil {
		s.recordRefusal(key.Hash, ref.reason, ref.codec)
	}
	if ref.status == http.StatusTooManyRequests && ref.retryAfter == 0 {
		ref.retryAfter = s.opts.RelayBusyRetryAfter
	}
	if ref.status == http.StatusServiceUnavailable && ref.retryAfter == 0 {
		ref.retryAfter = s.opts.FullRetryAfter
	}
	retryAfter(w, ref.retryAfter)
	s.m.post(ref.status)
	http.Error(w, ref.msg, ref.status)
}

func (s *Server) handlePost(w http.ResponseWriter, r *http.Request) {
	s.cors(w, r, "OPTIONS, POST")
	key, err := streamkey.FromAuthorization(r.Header.Get("Authorization"))
	if err != nil {
		s.refuse(w, r, nil, httpRefusal{status: http.StatusUnauthorized, msg: "malformed stream key"})
		return
	}
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/sdp" {
		s.refuse(w, r, nil, httpRefusal{status: http.StatusUnsupportedMediaType, msg: "want application/sdp"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxOfferBytes+1))
	if err != nil || len(body) > maxOfferBytes {
		s.refuse(w, r, nil, httpRefusal{status: http.StatusRequestEntityTooLarge, msg: "offer too large"})
		return
	}
	unlock := s.lockKey(key.Hash)
	defer unlock()
	now := s.now()

	// D9: D7's backoff first, before the cap and the SDP checks, and a 422
	// records nothing new.
	if rec, ok := s.table.Get(key.Hash, now); ok && rec.BackoffRunning(now) {
		s.refuseBackoff(w, rec, now)
		return
	}
	// D4: the client IP, then this gateway's own per-IP limit.
	ip, outcome, err := s.ips.Resolve(r)
	switch {
	case err != nil:
		s.m.xff.WithLabelValues(xffClientIPUnknown).Inc()
		s.refuse(w, r, &key, httpRefusal{status: http.StatusBadRequest, reason: status.ReasonClientIPUnknown, msg: "client address unknown"})
		return
	case outcome == clientip.UntrustedForwarded:
		s.m.xff.WithLabelValues(xffUntrustedPeer).Inc()
		s.warnXFF()
	}
	if ok, wait := s.limiter.Allow(ip, now); !ok {
		s.refuse(w, r, nil, httpRefusal{status: http.StatusTooManyRequests, retryAfter: wait, msg: "too many requests"})
		return
	}
	// D10: a pod holding neither the key's live session nor its backoff
	// asks its peers.
	exempt := s.liveSession(key.Hash) != nil
	peerExempt := false
	if !exempt && s.peers.enabled() {
		best, ok := s.peers.best(r.Context(), key.Hash)
		if ok && !best.snap.Live && best.snap.Backoff && best.snap.Record != nil {
			rec := s.takeover(r.Context(), key.Hash, best)
			s.refuseBackoff(w, rec, s.now())
			return
		}
		peerExempt = ok && best.snap.Live
	}
	// D10's caps.
	n := s.sessionCount()
	switch {
	case exempt:
		// Replaces a session on this pod: the count doesn't grow.
	case peerExempt:
		if n >= s.opts.MaxSessionsHard {
			s.refuse(w, r, &key, httpRefusal{status: http.StatusServiceUnavailable, reason: status.ReasonGatewayFull, msg: "gateway full"})
			return
		}
	default:
		if n >= s.opts.MaxSessions {
			s.refuse(w, r, &key, httpRefusal{status: http.StatusServiceUnavailable, reason: status.ReasonGatewayFull, msg: "gateway full"})
			return
		}
	}
	release := s.reserve()
	defer release()

	// D5.
	off, ref, err := inspectOffer(body)
	if err != nil {
		s.refuse(w, r, nil, httpRefusal{status: http.StatusBadRequest, msg: "unparsable SDP offer"})
		return
	}
	if ref != nil {
		s.refuse(w, r, &key, httpRefusal{status: http.StatusNotAcceptable, reason: ref.reason, codec: ref.codec, msg: "H.264 (one layer) and Opus only"})
		return
	}

	sess, answer, refusal := s.startSession(r.Context(), key, ip, off, string(body))
	if refusal != nil {
		s.refuse(w, r, &key, *refusal)
		return
	}
	s.m.post(http.StatusCreated)
	w.Header().Set("Content-Type", "application/sdp")
	w.Header().Set("Location", "/whip/session/"+sess.id)
	w.WriteHeader(http.StatusCreated)
	_, _ = io.WriteString(w, answer)
}

func (s *Server) refuseBackoff(w http.ResponseWriter, rec status.Record, now time.Time) {
	retryAfter(w, rec.BackoffUntil.Sub(now))
	s.m.post(http.StatusUnprocessableEntity)
	msg := "backing off"
	if rec.LastError != nil {
		msg += ": " + rec.LastError.Reason
	}
	http.Error(w, msg, http.StatusUnprocessableEntity)
}

// startSession builds the PeerConnection, dials the relay (before
// answering: D4's relay dial comes first) and answers.
func (s *Server) startSession(ctx context.Context, key streamkey.Key, ip netip.Addr, off offer, sdpOffer string) (*session, string, *httpRefusal) {
	internalErr := &httpRefusal{status: http.StatusInternalServerError, msg: "internal error"}
	me := &webrtc.MediaEngine{}
	if err := me.RegisterCodec(off.video, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, "", internalErr
	}
	if off.audio != nil {
		if err := me.RegisterCodec(*off.audio, webrtc.RTPCodecTypeAudio); err != nil {
			return nil, "", internalErr
		}
	}
	ir := &interceptor.Registry{}
	// Receiver reports only: NACKs are D6's own (sent the moment a hole is
	// seen, and timed for the RTT), so pion's NACK generator stays out.
	if err := webrtc.ConfigureRTCPReports(ir); err != nil {
		return nil, "", internalErr
	}
	api := webrtc.NewAPI(webrtc.WithMediaEngine(me), webrtc.WithSettingEngine(s.settings), webrtc.WithInterceptorRegistry(ir))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, "", internalErr
	}
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdpOffer}); err != nil {
		_ = pc.Close()
		return nil, "", &httpRefusal{status: http.StatusBadRequest, msg: "offer refused"}
	}

	ss := newSession(s, key, off)
	ss.pc = pc
	cfg := s.engineConfig(key, ip, off.audio != nil)
	if h := s.opts.Hooks.EngineConfig; h != nil {
		h(cfg)
	}
	ss.eng = s.opts.NewEngine(cfg, ss.callbacks(), publisher.Options{
		MediaFactory: ss.src.factory,
		Dial:         s.opts.Dial,
		Log:          slog.New(scrubIDs{ss.log.Handler()}),
	})
	dialCtx, cancel := context.WithTimeout(ctx, s.opts.Relay.DialTimeout)
	err = ss.eng.Start(dialCtx)
	cancel()
	if err != nil {
		_ = pc.Close()
		st := 0
		if se, ok := publisher.AsStartError(err); ok {
			st = se.Status
		}
		ss.log.Info("relay refused the publish", "status", st, "err", redact(err, key, s.opts.Relay))
		ref := relayRefusal(st)
		return nil, "", &ref
	}
	// From here the relay holds our publish: every failure must release it
	// (CODE-REVIEW.md, "Error paths must release what they acquired").
	fail := func() (*session, string, *httpRefusal) {
		_ = ss.eng.Stop()
		_ = pc.Close()
		ss.src.stop()
		return nil, "", internalErr
	}
	pc.OnTrack(ss.onTrack)
	pc.OnConnectionStateChange(ss.onConnectionState)
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return fail()
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(answer); err != nil {
		return fail()
	}
	select {
	case <-gathered:
	case <-time.After(gatherTimeout):
		return fail()
	}
	local := pc.LocalDescription()
	if local == nil {
		return fail()
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fail()
	}
	old := s.byKey[key.Hash]
	s.sessions[ss.id] = ss
	s.byKey[key.Hash] = ss
	s.m.sessions.Set(float64(len(s.sessions)))
	s.mu.Unlock()
	if old != nil {
		// D8's second OBS: the relay has sent the old publisher 4004; tear
		// down its PeerConnection here too.
		go old.end(endCause{kind: endReplaced})
	}
	go ss.loop()
	ss.log.Info("session answered")
	return ss, local.SDP, nil
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	s.cors(w, r, "OPTIONS, DELETE")
	key, err := streamkey.FromAuthorization(r.Header.Get("Authorization"))
	if err != nil {
		http.Error(w, "malformed stream key", http.StatusUnauthorized)
		return
	}
	id := r.PathValue("id")
	if s.deleteLocal(id, key.Hash) {
		w.WriteHeader(http.StatusOK)
		return
	}
	// D10: the session may be on the pod the key hashed to before.
	if s.peers.enabled() {
		if best, ok := s.peers.best(r.Context(), key.Hash); ok && best.snap.Live && best.snap.SessionID == id {
			if s.peers.deleteSession(r.Context(), best.peer, id, key.Hash) {
				w.WriteHeader(http.StatusOK)
				return
			}
		}
	}
	http.Error(w, "no such session", http.StatusNotFound)
}

// deleteLocal ends a local session for a WHIP DELETE: StopBroadcast, so
// the relay closes it with 4014 and the broadcast ends now (D8). It answers
// only after the close is sent.
func (s *Server) deleteLocal(id string, h status.Hash) bool {
	s.mu.Lock()
	ss := s.sessions[id]
	s.mu.Unlock()
	if ss == nil || subtle.ConstantTimeCompare(ss.key.Hash[:], h[:]) != 1 {
		return false
	}
	ss.end(endCause{kind: endDeleted})
	return true
}

func (s *Server) handleStatusGet(w http.ResponseWriter, r *http.Request) {
	s.cors(w, r, "OPTIONS, GET, DELETE")
	w.Header().Set("Cache-Control", "no-store")
	key, err := streamkey.FromAuthorization(r.Header.Get("Authorization"))
	if err != nil {
		http.Error(w, "malformed stream key", http.StatusUnauthorized)
		return
	}
	if peer, ok := s.locate(r.Context(), key.Hash); ok {
		if st, ok := s.peers.status(r.Context(), peer, key.Hash); ok {
			writeJSON(w, st)
			return
		}
	}
	writeJSON(w, s.localStatus(key.Hash))
}

func (s *Server) handleStatusDelete(w http.ResponseWriter, r *http.Request) {
	s.cors(w, r, "OPTIONS, GET, DELETE")
	key, err := streamkey.FromAuthorization(r.Header.Get("Authorization"))
	if err != nil {
		http.Error(w, "malformed stream key", http.StatusUnauthorized)
		return
	}
	if peer, ok := s.locate(r.Context(), key.Hash); ok {
		if s.peers.clearStatus(r.Context(), peer, key.Hash) {
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	s.clearBackoff(key.Hash)
	w.WriteHeader(http.StatusNoContent)
}

// locate decides where a key's status lives (D10): here when this pod holds
// its live session or running backoff; otherwise whichever pod holds the
// winner (live, then backoff, then the newest record). ok=false means here.
func (s *Server) locate(ctx context.Context, h status.Hash) (peer string, ok bool) {
	local := s.localSnapshot(h)
	if local.Live || local.Backoff || !s.peers.enabled() {
		return "", false
	}
	best, found := s.peers.best(ctx, h)
	if !found || !better(best.snap, local) {
		return "", false
	}
	return best.peer, true
}

// localStatus renders this pod's view of the key.
func (s *Server) localStatus(h status.Hash) status.Status {
	now := s.now()
	rec, ok := s.table.Get(h, now)
	if ss := s.liveSession(h); ss != nil && ss.isLive() {
		snap := ss.snapshot()
		rec.State = status.StateLive
		return rec.Status(now, &snap)
	}
	if !ok {
		return (*status.Record)(nil).Status(now, nil)
	}
	if rec.State == status.StateLive {
		// The session behind a live record is gone without having written
		// its end (it can't be, but never report a ghost as live).
		rec.State = status.StateEnded
	}
	return rec.Status(now, nil)
}

// clearBackoff is Retry now (D9): it clears D7's backoff and the lastError
// that started it. The ladder's level stays, so a key that keeps failing
// still climbs it.
func (s *Server) clearBackoff(h status.Hash) {
	now := s.now()
	rec, ok := s.table.Get(h, now)
	if !ok {
		return
	}
	startedBackoff := rec.LastError != nil && status.StartsBackoff(rec.LastError.Reason)
	if !rec.BackoffRunning(now) && !startedBackoff {
		return
	}
	s.table.Update(h, now, func(r *status.Record) {
		r.BackoffUntil = time.Time{}
		if r.LastError != nil && status.StartsBackoff(r.LastError.Reason) {
			r.LastError = nil
			r.LastErrorAt = time.Time{}
		}
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// redact keeps credentials out of a logged error: a dial error can quote
// the publish URL, which carries the resume token and the publish secret.
func redact(err error, key streamkey.Key, r RelayOptions) string {
	msg := err.Error()
	for _, secret := range []string{key.Token, r.PublishSecret, r.PublishProxyToken} {
		if secret != "" {
			msg = strings.ReplaceAll(msg, secret, "[redacted]")
		}
	}
	return msg
}
