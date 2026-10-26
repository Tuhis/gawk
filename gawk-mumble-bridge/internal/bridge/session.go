package bridge

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/quic-go/webtransport-go"

	"github.com/Tuhis/gawk/gawk-mumble-bridge/internal/mumble"
	"github.com/Tuhis/gawk/gawk-server/voice"
	"github.com/Tuhis/gawk/gawk-server/wire"
)

// endCause is why a voice session ends. One cause wins (the first to call
// end); every later signal of the same end defers to it (CODE-REVIEW.md:
// one event, one authoritative signal).
type endCause struct {
	// code is the close code the browser gets, sent in-band first; 0 when
	// the browser itself went away.
	code   uint32
	reason string
	// left tells the relay (Left) that the bridge side ended the session.
	left bool
}

// voiceSession is one browser's voice session (D5).
type voiceSession struct {
	s      *Server
	sess   *webtransport.Session
	remote string // the browser's address, for the relay's IP bans; never logged
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	key      string
	active   bool // counted against -max-voice-sessions
	accepted bool // the relay accepted the registration
	link     *link
	mc       *mumble.Client
	release  func()
	wantMute bool // self-mute as the browser last asked (or listen-only)

	strMu sync.Mutex
	str   *webtransport.Stream

	endOnce   sync.Once
	ending    chan struct{}
	endedDone chan struct{}
}

func (vs *voiceSession) roomKey() string {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	return vs.key
}

func (s *Server) handleVoice(w http.ResponseWriter, r *http.Request) {
	sess, err := s.publicWT.Upgrade(w, r)
	if err != nil {
		// Upgrade writes no response of its own: without one the dial
		// would see a 200 and a dead session.
		s.log.Debug("voice upgrade failed", "err", err)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithCancel(sess.Context())
	vs := &voiceSession{s: s, sess: sess, remote: r.RemoteAddr, ctx: ctx, cancel: cancel,
		ending: make(chan struct{}), endedDone: make(chan struct{})}
	if !s.trackSession(vs) {
		s.metrics.refused.WithLabelValues("draining").Inc()
		vs.end(endCause{code: codeDraining, reason: "bridge draining"})
		return
	}
	defer s.untrackSession(vs)
	vs.run()
}

func (s *Server) trackSession(vs *voiceSession) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.draining.Load() {
		return false
	}
	s.sessions[vs] = struct{}{}
	return true
}

func (s *Server) untrackSession(vs *voiceSession) {
	s.mu.Lock()
	delete(s.sessions, vs)
	vs.mu.Lock()
	if vs.active {
		vs.active = false
		s.activeSessions--
	}
	vs.mu.Unlock()
	s.metrics.voiceSessions.Set(float64(s.activeSessions))
	s.mu.Unlock()
}

// admit applies -max-voice-sessions (D23).
func (s *Server) admit(vs *voiceSession) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.activeSessions >= s.opts.MaxVoiceSessions {
		return false
	}
	s.activeSessions++
	vs.mu.Lock()
	vs.active = true
	vs.mu.Unlock()
	s.metrics.voiceSessions.Set(float64(s.activeSessions))
	return true
}

// refuse ends a session before Mumble, counting why.
func (vs *voiceSession) refuse(label string, code uint32, reason string) {
	vs.s.metrics.refused.WithLabelValues(label).Inc()
	vs.end(endCause{code: code, reason: reason})
}

// readRecord reads one room-record-framed message (uint16 length ‖ msg).
func readRecord(r io.Reader) ([]byte, error) {
	var hdr [wire.RoomRecordHeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n, err := wire.ParseRoomRecordLength(hdr[:])
	if err != nil {
		return nil, err
	}
	msg := make([]byte, n)
	if _, err := io.ReadFull(r, msg); err != nil {
		return nil, err
	}
	return msg, nil
}

func (vs *voiceSession) run() {
	s := vs.s
	t := s.timings

	// 1. The hello: one bidirectional stream, its first record.
	hctx, hcancel := context.WithTimeout(vs.ctx, t.HelloTimeout)
	str, err := vs.sess.AcceptStream(hctx)
	hcancel()
	if err != nil {
		if vs.ctx.Err() != nil {
			vs.end(endCause{})
			return
		}
		vs.refuse("hello_timeout", codeUnavailable, "no voice hello")
		return
	}
	vs.strMu.Lock()
	vs.str = str
	vs.strMu.Unlock()
	_ = str.SetReadDeadline(time.Now().Add(t.HelloTimeout))
	rec, err := readRecord(str)
	_ = str.SetReadDeadline(time.Time{})
	if err != nil {
		if vs.ctx.Err() != nil {
			vs.end(endCause{})
			return
		}
		vs.refuse("bad_hello", codeRemoved, "invalid voice hello")
		return
	}
	hello, err := wire.ParseVoiceHello(rec)
	if err != nil {
		vs.refuse("bad_hello", codeRemoved, "invalid voice hello")
		return
	}

	// 2. The ticket, offline (D7): forged or malformed is final, expired is
	// worth a fresh ticket.
	ticket, err := voice.VerifyTicket(s.opts.TicketSecret, hello.Ticket, s.now())
	switch {
	case errors.Is(err, voice.ErrTicketExpired):
		vs.refuse("expired_ticket", codeUnavailable, "voice ticket expired")
		return
	case err != nil:
		vs.refuse("bad_ticket", codeRemoved, "invalid voice ticket")
		return
	}
	vs.mu.Lock()
	vs.key = s.roomKeyOf(ticket.Code)
	vs.mu.Unlock()
	if !s.admit(vs) {
		vs.refuse("bridge_full", codeUnavailable, "voice bridge is full")
		return
	}
	listenOnly := hello.Flags&wire.VoiceHelloFlagListenOnly != 0
	deaf := hello.Flags&wire.VoiceHelloFlagSelfDeaf != 0
	mute := listenOnly || deaf || hello.Flags&wire.VoiceHelloFlagSelfMute != 0
	vs.mu.Lock()
	vs.wantMute = mute
	vs.mu.Unlock()

	// 3. The session link: register at the room's home and wait for its
	// answer (D2, D5).
	lctx, lcancel := context.WithTimeout(vs.ctx, t.LinkTimeout)
	l, err := s.dialSessionLink(lctx, ticket.Code)
	if err != nil {
		lcancel()
		if vs.ctx.Err() != nil {
			vs.end(endCause{})
			return
		}
		s.log.Warn("session link dial failed", "room", vs.key, "err", err)
		vs.refuse("relay_unreachable", codeUnavailable, "room voice link failed")
		return
	}
	vs.mu.Lock()
	vs.link = l
	vs.mu.Unlock()
	answer, err := vs.register(lctx, l, hello.Ticket)
	lcancel()
	if err != nil {
		if vs.ctx.Err() != nil {
			vs.end(endCause{})
			return
		}
		vs.refuse("relay_unanswered", codeUnavailable, "the room did not answer")
		return
	}
	if answer.Type == voice.TypeClose {
		vs.refuse("relay_refused", relayCode(answer.CloseCode), answer.Reason)
		return
	}
	vs.mu.Lock()
	vs.accepted = true
	vs.mu.Unlock()
	go vs.linkLoop(l)
	code := answer.Code
	if code == "" {
		code = ticket.Code
	}

	// 4. This user's own Mumble connection (OD6), in the room's channel.
	c, name, channel, err := vs.connect(ticket.Nickname, mute, deaf, code, answer)
	if err != nil {
		if vs.ctx.Err() != nil {
			vs.end(endCause{})
			return
		}
		reason := failureReason(err)
		s.recordFailure(reason, vs.key)
		s.log.Warn("voice user could not join Mumble", "room", vs.key, "reason", reason, "err", err)
		vs.end(endCause{code: codeUnavailable, reason: failureText[reason], left: true})
		return
	}
	s.log.Info("voice user joined", "room", vs.key, "mumble_session", c.Session())
	self := c.Self()
	if err := l.send(voice.Message{Type: voice.TypeJoined, Session: c.Session(), Name: wire.TruncateVoiceName(name), Flags: roomFlags(self)}); err != nil {
		vs.end(endCause{code: codeDraining, reason: "room voice link lost"})
		return
	}

	go vs.statePump(c, l, name, channel, roomFlags(self))
	go vs.audioOut(c)
	go vs.datagramIn(c, listenOnly)
	go vs.controlIn(c, listenOnly)

	select {
	case <-vs.ctx.Done():
		vs.end(endCause{})
	case <-c.Done():
		vs.end(vs.mumbleEnded(c.Err()))
	case <-vs.ending:
	}
	<-vs.endedDone
}

// register sends Register and waits for Accepted or Close.
func (vs *voiceSession) register(ctx context.Context, l *link, ticket []byte) (voice.Message, error) {
	if err := l.send(voice.Message{Type: voice.TypeRegister, Ticket: ticket, Remote: vs.remote}); err != nil {
		return voice.Message{}, err
	}
	type res struct {
		m   voice.Message
		err error
	}
	ch := make(chan res, 1)
	go func() {
		for {
			m, err := l.read()
			if err != nil || m.Type == voice.TypeAccepted || m.Type == voice.TypeClose {
				ch <- res{m, err}
				return
			}
		}
	}()
	select {
	case r := <-ch:
		return r.m, r.err
	case <-ctx.Done():
		l.close("no answer")
		return voice.Message{}, ctx.Err()
	}
}

// relayCode is the code a relay Close carries, or 4016 when it carries none
// the browser could act on.
func relayCode(code uint32) uint32 {
	if code >= wire.MinGawkCloseCode && code <= wire.MaxGawkCloseCode {
		return code
	}
	return codeUnavailable
}

// connect opens the user's Mumble connection and enters the room's channel.
func (vs *voiceSession) connect(nick string, mute, deaf bool, code string, acc voice.Message) (*mumble.Client, string, uint32, error) {
	s := vs.s
	c, name, release, err := s.connectMumble(vs.ctx, MumbleName(s.opts.Mumble.NameTemplate, nick), mute, deaf, audioQueueLen)
	if err != nil {
		return nil, "", 0, err
	}
	vs.mu.Lock()
	vs.mc, vs.release = c, release
	vs.mu.Unlock()
	select {
	case <-vs.ending:
		// end ran while we were dialling and did not see this connection.
		c.Close()
		release()
		return nil, "", 0, context.Canceled
	default:
	}
	ectx, cancel := context.WithTimeout(vs.ctx, s.timings.ChannelEnter)
	defer cancel()
	var channel uint32
	if acc.Temporary {
		channel, err = enterTemp(ectx, c, s.opts.Mumble.TempParent, s.tempChannel(code, acc.DisplayCode), s.timings.TempChannelWait)
	} else {
		channel, err = enterStatic(ectx, c, acc.Channel)
	}
	if err != nil {
		return nil, "", 0, err
	}
	return c, name, channel, nil
}

// mumbleEnded maps the end of the user's Mumble connection (D17, D18).
func (vs *voiceSession) mumbleEnded(err error) endCause {
	var rem *mumble.RemovedError
	if errors.As(err, &rem) && !rem.Ghost() {
		kind, text := "kick", "removed from Mumble"
		if rem.Ban {
			kind, text = "ban", "banned from Mumble"
		}
		vs.s.metrics.removals.WithLabelValues(kind).Inc()
		vs.s.log.Info("voice user removed by Mumble", "room", vs.key, "kind", kind)
		return endCause{code: codeRemoved, reason: text, left: true}
	}
	reason := classify(err)
	if reason != ReasonReplaced {
		reason = ReasonConnectionLost
	}
	vs.s.recordFailure(reason, vs.key)
	vs.s.log.Warn("voice user's Mumble connection ended", "room", vs.key, "reason", reason, "err", err)
	return endCause{code: codeUnavailable, reason: failureText[reason], left: true}
}

// end ends the session once: the Mumble user, the relay (Left when the
// bridge side ended it), the link, then the browser — in-band notice, settle,
// close. Every caller blocks until it is done.
func (vs *voiceSession) end(c endCause) {
	vs.endOnce.Do(func() {
		close(vs.ending)
		vs.mu.Lock()
		l, mc, release, accepted := vs.link, vs.mc, vs.release, vs.accepted
		vs.mu.Unlock()
		if mc != nil {
			mc.Close()
		}
		if release != nil {
			release()
		}
		var linkDone chan struct{}
		if l != nil {
			linkDone = make(chan struct{})
			go func() {
				defer close(linkDone)
				if c.left && accepted && l.send(voice.Message{Type: voice.TypeLeft, CloseCode: c.code, Reason: c.reason}) == nil {
					l.finish(vs.s.timings.NoticeSettle)
				}
				l.close("voice session ended")
			}()
		}
		defer func() {
			if linkDone != nil {
				<-linkDone
			}
		}()
		if c.code != 0 {
			vs.s.metrics.closed.WithLabelValues(strconv.Itoa(int(c.code))).Inc()
			closeWithNotice(vs.sess, c.code, c.reason, vs.s.timings.NoticeSettle)
		} else {
			_ = vs.sess.CloseWithError(0, "")
		}
		vs.cancel()
		close(vs.endedDone)
	})
	<-vs.endedDone
}

// linkLoop watches the session link after Accepted: a relay Close ends the
// session with its code (a gawk kill, ban, room end, newest-wins: D17); the
// link dropping without one is a 4002, rejoin (the home moved or restarted).
func (vs *voiceSession) linkLoop(l *link) {
	for {
		m, err := l.read()
		if err != nil {
			vs.end(endCause{code: codeDraining, reason: "room voice link lost"})
			return
		}
		if m.Type == voice.TypeClose {
			vs.end(endCause{code: relayCode(m.CloseCode), reason: m.Reason})
			return
		}
	}
}

// writeRecord frames and writes one record on the browser's stream.
func (vs *voiceSession) writeRecord(msg []byte) error {
	rec, err := wire.AppendRoomRecord(nil, msg)
	if err != nil {
		return err
	}
	vs.strMu.Lock()
	defer vs.strMu.Unlock()
	_ = vs.str.SetWriteDeadline(time.Now().Add(linkWriteTimeout))
	_, err = vs.str.Write(rec)
	return err
}

// statePump sends the browser a VoiceState and the relay a State whenever
// the bridged user's Mumble state changes (D17: a server mute reaches the
// browser here).
func (vs *voiceSession) statePump(c *mumble.Client, l *link, name string, channel uint32, sentRoom uint8) {
	var sentBrowser uint8 = 0xff
	wname := wire.TruncateVoiceName(name)
	for {
		ch := c.Changed()
		self := c.Self()
		connected := c.Err() == nil && self.Channel == channel
		if bf := stateFlags(self, connected); bf != sentBrowser {
			msg, err := wire.AppendVoiceState(nil, wire.VoiceState{Flags: bf, Session: self.Session, Name: wname})
			if err == nil && vs.writeRecord(msg) == nil {
				sentBrowser = bf
			}
		}
		if rf := roomFlags(self); rf != sentRoom && c.Err() == nil {
			if l.send(voice.Message{Type: voice.TypeState, Session: self.Session, Flags: rf}) == nil {
				sentRoom = rf
			}
		}
		select {
		case <-ch:
		case <-vs.ending:
			return
		}
	}
}

// audioOut forwards every Opus packet this user's connection receives to
// the browser as a VoiceFrame datagram tagged with the speaker (D8).
func (vs *voiceSession) audioOut(c *mumble.Client) {
	q := c.Audio()
	m := vs.s.metrics
	var buf []byte
	var dropped uint64
	defer func() {
		if d := q.Dropped() - dropped; d > 0 {
			m.dropped.WithLabelValues(dirToBrowser, "queue_overflow").Add(float64(d))
		}
	}()
	for {
		p, err := q.Pop(vs.ctx)
		if err != nil {
			return
		}
		if d := q.Dropped(); d != dropped {
			m.dropped.WithLabelValues(dirToBrowser, "queue_overflow").Add(float64(d - dropped))
			dropped = d
		}
		buf, err = wire.AppendVoiceFrame(buf[:0], wire.VoiceFrameHeader{End: p.End, Target: p.Target, Speaker: p.Session, Seq: uint32(p.Seq)}, p.Opus)
		if err != nil {
			m.dropped.WithLabelValues(dirToBrowser, "too_large").Inc()
			continue
		}
		if err := vs.sess.SendDatagram(buf); err != nil {
			m.dropped.WithLabelValues(dirToBrowser, "send_failed").Inc()
			continue
		}
		m.forwarded.WithLabelValues(dirToBrowser).Inc()
	}
}

// datagramIn sends the browser's VoiceFrames as this user's voice, unless
// it may not speak (listen-only, muted, suppressed): those are dropped here,
// before murmur would drop them.
func (vs *voiceSession) datagramIn(c *mumble.Client, listenOnly bool) {
	m := vs.s.metrics
	for {
		dg, err := vs.sess.ReceiveDatagram(vs.ctx)
		if err != nil {
			return
		}
		h, payload, err := wire.ParseVoiceFrame(dg)
		if err != nil {
			m.dropped.WithLabelValues(dirToMumble, "malformed").Inc()
			continue
		}
		if listenOnly {
			m.dropped.WithLabelValues(dirToMumble, "listen_only").Inc()
			continue
		}
		vs.mu.Lock()
		muted := vs.wantMute
		vs.mu.Unlock()
		self := c.Self()
		if muted || self.SelfMute || self.Mute || self.Suppress {
			m.dropped.WithLabelValues(dirToMumble, "muted").Inc()
			continue
		}
		ok, err := c.WriteAudio(int64(h.Seq), payload, h.End)
		switch {
		case err != nil:
			return
		case !ok:
			m.dropped.WithLabelValues(dirToMumble, "queue_full").Inc()
		default:
			m.forwarded.WithLabelValues(dirToMumble).Inc()
		}
	}
}

// controlIn applies the browser's VoiceControl records as Mumble self-mute
// and self-deafen. Listen-only stays muted whatever it asks.
func (vs *voiceSession) controlIn(c *mumble.Client, listenOnly bool) {
	vs.strMu.Lock()
	str := vs.str
	vs.strMu.Unlock()
	for {
		msg, err := readRecord(str)
		if err != nil {
			return
		}
		if len(msg) < 2 || msg[1] != wire.TypeVoiceControl {
			continue
		}
		flags, err := wire.ParseVoiceControl(msg)
		if err != nil {
			continue
		}
		deaf := flags&wire.VoiceControlFlagSelfDeaf != 0
		mute := listenOnly || deaf || flags&wire.VoiceControlFlagSelfMute != 0
		vs.mu.Lock()
		vs.wantMute = mute
		vs.mu.Unlock()
		if err := c.SetSelf(mute, deaf); err != nil {
			return
		}
	}
}
