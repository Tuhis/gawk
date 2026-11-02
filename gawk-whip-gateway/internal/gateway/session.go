package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"

	"github.com/Tuhis/gawk/gawk-server/publisher"

	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/media"
	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/status"
	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/streamkey"
)

// iceConnectTimeout bounds how long an answered session may wait for OBS's
// first connectivity check before it counts as lost. Provisional until WG0.
const iceConnectTimeout = 30 * time.Second

type endKind int

const (
	endDeleted   endKind = iota + 1 // WHIP DELETE: StopBroadcast (4014), ended, no lastError
	endLost                         // ICE/consent failure: Stop (0, grace), lost
	endViolation                    // the gateway ends it (D7): StopBroadcast, reason, backoff
	endEngine                       // the engine ended itself (relay close code, resume failure)
	endReplaced                     // a newer session for the key on this pod: Stop, record nothing
	endShutdown                     // the pod is stopping: Stop, record nothing
)

type endCause struct {
	kind   endKind
	reason string
	gopMs  int
	err    error
}

func (c endCause) metricReason() string {
	switch c.kind {
	case endDeleted:
		return "deleted"
	case endReplaced:
		return "superseded"
	case endShutdown:
		return "shutdown"
	}
	if c.reason == "" {
		return "ended"
	}
	return c.reason
}

type inputKind int

const (
	inVideo inputKind = iota
	inAudio
	inVideoSR
	inAudioSR
)

type input struct {
	kind inputKind
	pkt  *rtp.Packet
	sr   *rtcp.SenderReport
	at   time.Time
}

// session is one OBS → relay publish: a PeerConnection, a media pipeline
// and a publisher engine.
type session struct {
	srv *Server
	id  string
	key streamkey.Key
	log *slog.Logger

	pc   *webrtc.PeerConnection
	eng  Engine
	src  *source
	pipe *media.Pipeline // owned by loop
	in   chan input
	done chan struct{}

	mu        sync.Mutex
	live      bool
	ending    bool
	videoSSRC uint32
	videoSeen bool
	viewers   *uint32
	snap      status.Snapshot
	engineErr error
	meter     *media.Meter
}

func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func newSession(srv *Server, key streamkey.Key, o offer) *session {
	ss := &session{
		srv:   srv,
		id:    newSessionID(),
		key:   key,
		pipe:  media.NewPipeline(media.PipelineConfig{Reorder: srv.opts.Reorder, GOP: srv.opts.GOP}),
		in:    make(chan input, 1024),
		done:  make(chan struct{}),
		meter: media.NewMeter(3),
		snap:  status.Snapshot{Codec: "H.264"},
	}
	ss.log = srv.log.With("session", ss.id[:8], "key", hex.EncodeToString(key.Hash[:4]))
	ss.src = newSource(o.audio != nil)
	if o.sps != nil {
		ss.pipe.SetParameterSets(o.sps, o.pps)
	}
	return ss
}

// engineConfig is the publish the gateway makes for the key (D2, D4): a
// reclaim of the ID with its resume token, through the relay's publish
// proxy so its per-IP gates see the OBS user.
func (srv *Server) engineConfig(key streamkey.Key, ip netip.Addr, hasAudio bool) publisher.Config {
	hdr := http.Header{}
	if srv.opts.Relay.PublishProxyToken != "" {
		hdr.Set("Authorization", "Bearer "+srv.opts.Relay.PublishProxyToken)
	}
	hdr.Set("Gawk-Publisher-Ip", ip.String())
	return publisher.Config{
		RelayURL:      srv.opts.Relay.URL,
		BroadcastID:   key.ID,
		ResumeToken:   key.Token,
		PublishSecret: srv.opts.Relay.PublishSecret,
		Origin:        srv.opts.Relay.Origin,
		Insecure:      srv.opts.Relay.Insecure,
		Header:        hdr,
		Media:         publisher.MediaConfig{DisableAudio: !hasAudio},
	}
}

func (ss *session) callbacks() publisher.Callbacks {
	return publisher.Callbacks{
		OnViewerCount: func(n uint32) {
			ss.mu.Lock()
			ss.viewers = &n
			ss.mu.Unlock()
		},
		OnResumed: func() {
			// D7: the relay pod's cache is empty after a resume; ask for an
			// IDR (stock OBS ignores it, other senders honour it).
			ss.sendPLI("resume")
		},
		OnError: func(err error) {
			ss.mu.Lock()
			ss.engineErr = err
			ss.mu.Unlock()
		},
		OnEnded: func() {
			ss.mu.Lock()
			err := ss.engineErr
			ss.mu.Unlock()
			// Off the engine's goroutine: end waits for the engine to stop,
			// and this callback runs inside it.
			go ss.end(endCause{kind: endEngine, err: err})
		},
	}
}

// classifyEngineEnd maps why the engine ended itself onto D8/D9.
//
// The publisher package reports a relay close code only as the error text
// of its closeCodeError (resume.go); there is no typed error to match. The
// three strings below are pinned against the real engine by
// TestEngineCloseCodesClassify, which fails if publisher rewords them. A
// typed close-code error in publisher would replace this; see the module
// README's gaps.
func classifyEngineEnd(err error) (reason string, superseded bool) {
	if err == nil {
		return "", false
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "another broadcaster took over this code"): // 4004
		return "", true
	case strings.Contains(msg, "terminated by the server operator"): // 4006
		return status.ReasonKilled, false
	case strings.Contains(msg, "the relay ended this broadcast"): // 4000
		return "", false
	}
	if se, ok := publisher.AsStartError(err); ok {
		if r := relayRefusal(se.Status); r.reason != "" {
			return r.reason, false
		}
	}
	return status.ReasonRelayBusy, false
}

// onTrack starts reading one remote track.
func (ss *session) onTrack(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
	isVideo := track.Kind() == webrtc.RTPCodecTypeVideo
	if isVideo {
		ss.mu.Lock()
		first := !ss.videoSeen
		ss.videoSeen, ss.videoSSRC = true, uint32(track.SSRC())
		ss.mu.Unlock()
		if first {
			ss.sendPLI("start") // D7: the session start's event PLI
		}
	}
	go func() {
		for {
			pkts, _, err := receiver.ReadRTCP()
			if err != nil {
				return
			}
			for _, p := range pkts {
				if sr, ok := p.(*rtcp.SenderReport); ok {
					k := inAudioSR
					if isVideo {
						k = inVideoSR
					}
					if !ss.push(input{kind: k, sr: sr}) {
						return
					}
				}
			}
		}
	}()
	go func() {
		k := inAudio
		if isVideo {
			k = inVideo
		}
		for {
			pkt, _, err := track.ReadRTP()
			if err != nil {
				return
			}
			if !ss.push(input{kind: k, pkt: pkt, at: time.Now()}) {
				return
			}
		}
	}()
}

func (ss *session) push(in input) bool {
	select {
	case ss.in <- in:
		return true
	case <-ss.done:
		return false
	}
}

func (ss *session) onConnectionState(st webrtc.PeerConnectionState) {
	switch st {
	case webrtc.PeerConnectionStateConnected:
		ss.goLive()
	case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
		go ss.end(endCause{kind: endLost, reason: status.ReasonLost})
	}
}

// goLive marks the session live: D9's "a session that goes live clears the
// key's lastError".
func (ss *session) goLive() {
	ss.mu.Lock()
	if ss.live || ss.ending {
		ss.mu.Unlock()
		return
	}
	ss.live = true
	snap := ss.snap
	ss.mu.Unlock()
	ss.log.Info("session live")
	ss.srv.updateRecordFor(ss, func(r *status.Record) {
		r.State = status.StateLive
		r.LastError = nil
		r.LastErrorAt = time.Time{}
		r.Session = &snap
	})
}

func (ss *session) sendPLI(trigger string) {
	ss.mu.Lock()
	ssrc, ok, ending := ss.videoSSRC, ss.videoSeen, ss.ending
	ss.mu.Unlock()
	if !ok || ending {
		return
	}
	ss.srv.m.plis.WithLabelValues(trigger).Inc()
	if h := ss.srv.opts.Hooks.PLI; h != nil {
		h(ss.id, trigger)
	}
	if err := ss.pc.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: ssrc}}); err != nil {
		ss.log.Debug("PLI send failed", "err", err)
	}
}

// loop owns the pipeline: packets in, AUs out to the engine, NACKs back to
// OBS, deadlines for D6's window, a stats sample each second.
func (ss *session) loop() {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	stats := time.NewTicker(time.Second)
	defer stats.Stop()
	connect := time.NewTimer(iceConnectTimeout)
	defer connect.Stop()
	armed := time.Time{}
	for {
		var timerC <-chan time.Time
		if dl, ok := ss.pipe.Deadline(); ok {
			if !dl.Equal(armed) {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(time.Until(dl))
				armed = dl
			}
			timerC = timer.C
		}
		select {
		case <-ss.done:
			return
		case in := <-ss.in:
			ss.handle(in)
		case <-timerC:
			armed = time.Time{}
			ss.apply(ss.pipe.Tick(time.Now(), ss.src.nowUs()))
		case <-stats.C:
			ss.sample()
		case <-connect.C:
			ss.mu.Lock()
			live := ss.live
			ss.mu.Unlock()
			if !live {
				ss.log.Info("OBS never connected")
				go ss.end(endCause{kind: endLost, reason: status.ReasonLost})
			}
		}
	}
}

func (ss *session) handle(in input) {
	switch in.kind {
	case inVideo:
		p := in.pkt
		out := ss.pipe.Video(media.Packet{Seq: p.SequenceNumber, Timestamp: p.Timestamp, Marker: p.Marker, Payload: p.Payload}, in.at, ss.src.nowUs())
		ss.apply(out)
		if ss.pipe.GOP().Verdict() == media.GOPEnd {
			ss.violate(status.ReasonGOPTooLong, int(ss.pipe.GOP().Median().Milliseconds()))
		}
	case inAudio:
		p := in.pkt
		if pkt, ok := ss.pipe.Audio(media.AudioPacket{Seq: p.SequenceNumber, Timestamp: p.Timestamp, Payload: p.Payload}, ss.src.nowUs()); ok {
			ss.src.pushAudio(pkt)
		}
	case inVideoSR:
		ss.pipe.Timeline().VideoSenderReport(in.sr.RTPTime, in.sr.NTPTime)
	case inAudioSR:
		ss.pipe.Timeline().AudioSenderReport(in.sr.RTPTime, in.sr.NTPTime)
	}
}

// apply hands one pipeline output on.
func (ss *session) apply(out media.Output) {
	m := ss.srv.m
	if len(out.NACK) > 0 {
		ss.mu.Lock()
		ssrc := ss.videoSSRC
		ss.mu.Unlock()
		m.nacks.Add(float64(len(out.NACK)))
		if err := ss.pc.WriteRTCP([]rtcp.Packet{&rtcp.TransportLayerNack{
			MediaSSRC: ssrc, Nacks: rtcp.NackPairsFromSequenceNumbers(out.NACK),
		}}); err != nil {
			ss.log.Debug("NACK send failed", "err", err)
		}
	}
	if out.WindowDrops > 0 {
		m.framesDropped.WithLabelValues(dropReorderWindow).Add(float64(out.WindowDrops))
	}
	if out.IDRWaitDrops > 0 {
		m.framesDropped.WithLabelValues(dropAwaitingIDR).Add(float64(out.IDRWaitDrops))
	}
	backpressured := false
	for _, au := range out.AUs {
		if backpressured && !au.Keyframe {
			m.framesDropped.WithLabelValues(dropBackpressure).Inc()
			continue
		}
		if !ss.src.pushFrame(au) {
			// The engine fell behind: drop, and the deltas after it, until
			// the next IDR, exactly like a loss.
			backpressured = true
			ss.pipe.Lost()
			m.framesDropped.WithLabelValues(dropBackpressure).Inc()
			continue
		}
		backpressured = false
		m.frames.Inc()
	}
	if v := out.Violation; v != nil {
		reason := status.ReasonInvalidH264
		if v.Kind == media.ViolationBFrames {
			reason = status.ReasonBFrames
		}
		ss.log.Info("refusing the stream", "reason", reason, "err", v.Err)
		ss.violate(reason, 0)
	}
}

func (ss *session) violate(reason string, gopMs int) {
	ss.mu.Lock()
	if ss.ending {
		ss.mu.Unlock()
		return
	}
	ss.mu.Unlock()
	go ss.end(endCause{kind: endViolation, reason: reason, gopMs: gopMs})
}

// sample refreshes the status snapshot (once a second).
func (ss *session) sample() {
	r := ss.meter.Sample(time.Now(), ss.pipe.Counters())
	g := ss.pipe.GOP()
	med := g.Median()
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if sps := ss.pipe.SPS(); sps != nil {
		ss.snap.Width, ss.snap.Height = sps.Width, sps.Height
	}
	if r.OK {
		ss.snap.FPS = float64(int(r.FPS*10+0.5)) / 10
		ss.snap.BitrateKbps = r.BitrateKbps
		loss := float64(int(r.LossPct*100+0.5)) / 100
		ss.snap.LossPct = &loss
	}
	ss.snap.GOPMs = int(med.Milliseconds())
	ss.snap.Warnings = nil
	if g.Verdict() == media.GOPWarn {
		ss.snap.Warnings = []status.Warning{{Reason: status.WarnGOPLong, GOPMs: ss.snap.GOPMs}}
	}
	ss.snap.Viewers = ss.viewers
	if ss.live && med > 0 {
		ss.srv.m.gop.Observe(med.Seconds())
	}
	if ss.live && ss.pipe.Reorder().RTT().Samples() > 0 {
		ss.srv.m.rtt.Observe(ss.pipe.Reorder().RTT().RTT().Seconds())
	}
}

// snapshot is the live status values.
func (ss *session) snapshot() status.Snapshot {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	s := ss.snap
	s.Viewers = ss.viewers
	s.Warnings = append([]status.Warning(nil), ss.snap.Warnings...)
	return s
}

func (ss *session) isLive() bool {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.live && !ss.ending
}

// end ends the session once, for one cause (CODE-REVIEW.md: one event, one
// authoritative signal — whichever of DELETE, ICE failure, a violation or
// the engine's own end arrives first decides, and the rest are no-ops).
func (ss *session) end(c endCause) {
	ss.mu.Lock()
	if ss.ending {
		ss.mu.Unlock()
		return
	}
	ss.ending = true
	snap := ss.snap
	snap.Viewers = nil
	ss.mu.Unlock()
	close(ss.done)

	switch c.kind {
	case endDeleted, endViolation:
		// The deliberate end: 4014, the broadcast ends now (R78).
		_ = ss.eng.StopBroadcast()
	default:
		// 0: the broadcast waits in the relay's grace; OBS's reconnect (or
		// the newer session) reclaims it.
		_ = ss.eng.Stop()
	}
	_ = ss.pc.Close()
	ss.src.stop()

	if c.kind == endEngine {
		reason, superseded := classifyEngineEnd(c.err)
		c.reason = reason
		if superseded {
			c.reason = ""
			ss.log.Info("superseded by a newer publisher for the key")
		}
	}
	ss.log.Info("session ended", "cause", c.metricReason())
	ss.srv.m.ended.WithLabelValues(c.metricReason()).Inc()

	if c.kind != endReplaced && c.kind != endShutdown {
		ss.srv.updateRecordFor(ss, func(r *status.Record) {
			r.State = status.StateEnded
			r.Session = &snap
			now := ss.srv.now()
			if c.reason == "" {
				r.LastError = nil
				r.LastErrorAt = time.Time{}
			} else {
				r.LastError = &status.ErrorInfo{Reason: c.reason, GOPMs: c.gopMs}
				r.LastErrorAt = now
			}
			if status.StartsBackoff(c.reason) {
				d := ss.srv.opts.Backoff.StartBackoff(r, now)
				ss.log.Info("backoff started", "reason", c.reason, "for", d)
			}
		})
	}
	ss.srv.removeSession(ss)
}

// updateRecordFor updates the key's record on behalf of ss, unless a newer
// session for the key has taken it over on this pod.
func (srv *Server) updateRecordFor(ss *session, fn func(*status.Record)) {
	srv.mu.Lock()
	cur := srv.byKey[ss.key.Hash]
	srv.mu.Unlock()
	if cur != nil && cur != ss {
		return
	}
	srv.table.Update(ss.key.Hash, srv.now(), fn)
}

func (srv *Server) removeSession(ss *session) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.sessions[ss.id] == ss {
		delete(srv.sessions, ss.id)
	}
	if srv.byKey[ss.key.Hash] == ss {
		delete(srv.byKey, ss.key.Hash)
	}
	srv.m.sessions.Set(float64(len(srv.sessions)))
}

// source is the engine's MediaSource: the session loop pushes rebuilt AUs
// and Opus packets into it.
type source struct {
	frames   chan publisher.AccessUnit
	audio    chan publisher.AudioPacket
	hasAudio bool
	stopped  chan struct{}
	once     sync.Once

	clockMu sync.Mutex
	clock   publisher.Clock
}

func newSource(hasAudio bool) *source {
	s := &source{
		frames:   make(chan publisher.AccessUnit, 64),
		hasAudio: hasAudio,
		stopped:  make(chan struct{}),
		clock:    publisher.NewClock(),
	}
	if hasAudio {
		s.audio = make(chan publisher.AudioPacket, 128)
	}
	return s
}

// factory is the engine's MediaSourceFactory: the source adopts the
// engine's clock, so frame stamps and TimeSync read one timeline (the
// engine's Decision 6).
func (s *source) factory(_ publisher.MediaConfig, clock publisher.Clock, _ *slog.Logger) (publisher.MediaSource, error) {
	s.clockMu.Lock()
	s.clock = clock
	s.clockMu.Unlock()
	return s, nil
}

func (s *source) nowUs() uint64 {
	s.clockMu.Lock()
	defer s.clockMu.Unlock()
	return s.clock.NowUs()
}

func (s *source) Start(context.Context) (<-chan publisher.AccessUnit, error) { return s.frames, nil }

// Stop is the engine's teardown. The channels are never closed (the
// session loop may still be pushing); the engine's pumps leave on its
// context.
func (s *source) Stop() error { s.stop(); return nil }

func (s *source) stop()               { s.once.Do(func() { close(s.stopped) }) }
func (s *source) Encoder() string     { return "whip" }
func (s *source) CapturePath() string { return "whip" }
func (s *source) Err() error          { return nil }

func (s *source) Audio() <-chan publisher.AudioPacket {
	if !s.hasAudio {
		return nil
	}
	return s.audio
}

// AudioFormat is the engine's fixed lane (48 kHz stereo Opus, 20 ms), which
// is what OBS's Opus encoder sends. A sender that sends mono or another
// frame size fails the engine's first-packet TOC check and the broadcast
// publishes video only (publisher/send.go). Provisional until WG0.
func (s *source) AudioFormat() (publisher.AudioFormat, bool) {
	return publisher.DefaultAudioFormat("whip"), s.hasAudio
}

func (s *source) pushFrame(au publisher.AccessUnit) bool {
	select {
	case <-s.stopped:
		return false
	default:
	}
	select {
	case s.frames <- au:
		return true
	default:
		return false
	}
}

func (s *source) pushAudio(p publisher.AudioPacket) {
	if !s.hasAudio {
		return
	}
	select {
	case s.audio <- p:
	default: // the engine fell behind; audio is subordinate
	}
}
