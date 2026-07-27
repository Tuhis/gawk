package hub

// R21 DV5 (docs/26 Decision 8): the audio side of the DVR.
//
// Audio gets its OWN ring, and that is a design decision rather than an
// implementation convenience. A GOP exists because a delta frame is
// undecodable without its keyframe; audio has no such dependency — every Opus
// packet is an independent entry point — so there is no natural audio GOP and
// no reason to invent one. The two media also differ in entry unit, eviction
// unit, arrival timing (audio lands earlier: no reassembly, no keyframe wait)
// and bandwidth share (~4%). Binning audio into GOP entries would compromise
// on all four to save a coordinate that timestamps already provide.
//
// What relates the rings is arrival time, and the coupling runs one way only:
// the audio cursor is held to the video cursor (DueFor). Audio is cheap enough
// to catch up almost instantly after a stall, while video is still draining —
// and the viewer holds audio against the video presentation schedule, so audio
// arriving far ahead is overflow-dropped on arrival by its jitter buffer. Left
// unthrottled, the relay would rescue the audio and the viewer would bin it.
//
// Delivery rides the SAME carrier framing as video (docs/24's length-prefixed
// records behind a 0x0A prologue) but on a stream of its own. That is what
// keeps docs/20 field finding 5's harms away: QUIC streams are independent, so
// there is no head-of-line blocking behind video deltas, and with no GOPs
// there is no clumped tail to drop. It also means zero viewer changes — the
// records are audio datagrams, which the viewer's existing datagram path
// already routes by type.

import (
	"sync"
	"time"
)

// AudioSkewBudget is how far ahead of the video cursor the audio cursor may
// run. Comfortably inside the viewer's resilient audio envelope (2000 ms) and
// its 3000 ms alignment hold, so audio released this early is buffered rather
// than dropped — and throttling ~4% of the bitrate costs nothing.
const AudioSkewBudget = 500 * time.Millisecond

type dvrAudioPacket struct {
	seq  int64
	at   time.Time
	data []byte
}

// DVRAudioCursor is one subscriber's position in the audio ring. Immutable,
// like the video cursor.
type DVRAudioCursor struct{ seq int64 }

func (c DVRAudioCursor) Next() DVRAudioCursor { c.seq++; return c }

// DVRAudioRing is a time-bounded window of one broadcast's audio.
type DVRAudioRing struct {
	opts DVROptions

	mu      sync.RWMutex
	packets []dvrAudioPacket
	nextSeq int64
	bytes   int
	wake    chan struct{}
}

func NewDVRAudioRing(opts DVROptions) *DVRAudioRing {
	return &DVRAudioRing{opts: opts, wake: make(chan struct{})}
}

// Wait returns a channel closed by the next append; take it before the final
// read or an append landing in between is missed.
func (r *DVRAudioRing) Wait() <-chan struct{} {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.wake
}

// Append retains one audio datagram (frame or config), copied — the ring
// outlives the caller's buffer by seconds.
func (r *DVRAudioRing) Append(dgram []byte, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.packets = append(r.packets, dvrAudioPacket{
		seq:  r.nextSeq,
		at:   at,
		data: append([]byte(nil), dgram...),
	})
	r.nextSeq++
	r.bytes += len(dgram)
	// Audio is bounded by the same window as video so the two cursors can
	// never be separated by more history than either ring holds.
	for len(r.packets) > 1 && (at.Sub(r.packets[0].at) > r.opts.Window ||
		(r.opts.MaxBytes > 0 && r.bytes > r.opts.MaxBytes)) {
		r.bytes -= len(r.packets[0].data)
		r.packets = r.packets[1:]
	}
	close(r.wake)
	r.wake = make(chan struct{})
}

// Oldest is a cursor at the ring's tail — where a subscriber resyncs to after
// falling off, and where a joiner starts (audio has no keyframe to wait for,
// so the oldest retained packet is a perfectly good entry point).
func (r *DVRAudioRing) Oldest() DVRAudioCursor {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.packets) == 0 {
		return DVRAudioCursor{seq: r.nextSeq}
	}
	return DVRAudioCursor{seq: r.packets[0].seq}
}

// Newest is a cursor past the last packet — where a DVR subscriber that is
// caught up sits.
func (r *DVRAudioRing) Newest() DVRAudioCursor {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return DVRAudioCursor{seq: r.nextSeq}
}

// At resolves a cursor. ok is false at the head of the ring (nothing new yet)
// and for a cursor that fell off the tail — the caller tells them apart with
// FellOffTail.
func (r *DVRAudioRing) At(c DVRAudioCursor) ([]byte, time.Time, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for i := range r.packets {
		if r.packets[i].seq == c.seq {
			return r.packets[i].data, r.packets[i].at, true
		}
	}
	return nil, time.Time{}, false
}

// FellOffTail reports that this cursor's packets have been evicted.
func (r *DVRAudioRing) FellOffTail(c DVRAudioCursor) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if len(r.packets) == 0 {
		return false
	}
	return c.seq < r.packets[0].seq
}

// DueFor reports whether a packet that arrived at packetAt may be released to
// a subscriber whose video cursor is at videoAt. This is the one coupling
// between the two rings, and it runs one way: audio waits for video, never the
// reverse (docs/20 field finding 4 makes video the master clock).
func (r *DVRAudioRing) DueFor(packetAt, videoAt time.Time, budget time.Duration) bool {
	return !packetAt.After(videoAt.Add(budget))
}

// Bytes is the retained payload size, for /statusz.
func (r *DVRAudioRing) Bytes() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.bytes
}

// --- the audio drain -------------------------------------------------------

// drainDVRAudio serves this subscriber's audio from the audio ring on a
// long-lived carrier stream of its own. No rotation: audio has no GOPs, so
// there is no boundary to rotate at, and a resync is simply a timestamp
// discontinuity the viewer's jitter buffer already handles (gap → conceal,
// large jump → re-anchor).
func (s *Subscriber) drainDVRAudio() {
	defer s.retireAudioCarrier(&s.dvrAudioCar)
	var scratch []byte
	for {
		if s.closed.Load() {
			return
		}
		wake := s.dvrAudio.Wait()

		if s.dvrAudio.FellOffTail(s.dvrAudioCur) {
			// Past the ring: rejoin at the tail. The config may be needed
			// before the broadcaster's next 1 Hz re-send, so re-emit the
			// cached one first (the video path gets this for free — config
			// rides every keyframe).
			s.dvrAudioCur = s.dvrAudio.Oldest()
			s.sendCachedAudioConfig(&scratch)
			continue
		}
		pkt, at, ok := s.dvrAudio.At(s.dvrAudioCur)
		if !ok {
			select {
			case <-wake:
			case <-s.dvrStop:
				return
			}
			continue
		}
		// The one coupling between the rings (docs/26 Decision 8b): audio is
		// ~4% of the bitrate and would otherwise sprint away from a video
		// cursor still draining its backlog — and the viewer, holding audio
		// against the video schedule, would overflow-drop exactly what the
		// ring just rescued.
		if !s.dvrAudio.DueFor(at, s.dvrVideoCursorAt(), AudioSkewBudget) {
			select {
			case <-s.dvrStop:
				return
			case <-time.After(dvrRetryFloor):
			}
			continue
		}
		if !s.writeAudioRecord(pkt, &scratch) {
			select {
			case <-s.dvrStop:
				return
			case <-time.After(dvrRetryFloor):
			}
			continue
		}
		s.dvrAudioCur = s.dvrAudioCur.Next()
	}
}

// dvrVideoCursorAt is the arrival time of the GOP the video cursor is in — the
// reference the audio cursor is held to. Read from the atomic the video drain
// publishes, never from dvrCursor itself, which that goroutine owns.
func (s *Subscriber) dvrVideoCursorAt() time.Time {
	ms := s.dvrCursorAtMs.Load()
	if ms == 0 {
		// The video drain has not placed its cursor yet. Hold audio rather
		// than racing ahead of a video position that does not exist.
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func (s *Subscriber) sendCachedAudioConfig(scratch *[]byte) {
	cfg := s.hub.cachedAudioConfigSnapshot()
	if cfg == nil {
		return
	}
	s.writeAudioRecord(cfg, scratch)
}

// writeAudioRecord frames one audio datagram onto the DVR audio carrier,
// opening it lazily. The framing and the stream handling are the shared audio
// carrier (hub.go, "the audio carrier"), which R19's resilient lane now uses
// too; what stays here is the two things only a DVR subscriber has — the
// catch-up pacer, and the progress note its health check reads.
func (s *Subscriber) writeAudioRecord(dgram []byte, scratch *[]byte) bool {
	framed, n, ok := frameAudioRecord(dgram, scratch, s.dvrAudioCar == nil)
	if !ok {
		return true // undeliverable by construction; skip it
	}
	if !s.dvrPace.allow(n, s.dvr.LiveRateBps()) {
		return false
	}
	if !s.hub.registry.consumeBandwidth(n) {
		s.dropped.Add(1)
		s.hub.countBandwidthDrop(n)
		return false
	}
	if !s.emitAudioRecord(&s.dvrAudioCar, framed) {
		return false
	}
	s.dvrNoteProgress()
	return true
}
