package media

import "time"

// Packet is one video RTP packet as the adapter needs it.
type Packet struct {
	Seq       uint16
	Timestamp uint32
	Marker    bool
	Payload   []byte
}

// Frame is one access unit's RTP payloads, in sequence order, complete (no
// sequence gap from its first packet to its marker).
type Frame struct {
	Timestamp uint32
	Payloads  [][]byte
	// AfterGap: packets were given up on since the previous frame, so a
	// whole frame may be missing right before this one; only an IDR can be
	// decoded from here (D6: "the deltas after it are dropped until the next
	// IDR").
	AfterGap bool
}

// ReorderResult is what one Push or Tick produced.
type ReorderResult struct {
	// Frames are complete frames, in order, ready now.
	Frames []Frame
	// Dropped counts frames given up on: incomplete when their window
	// closed, or missing their first packet.
	Dropped int
	// Gap reports that packets were given up on during this call (the frame
	// after it carries AfterGap, in this call or a later one).
	Gap bool
	// NACK lists sequence numbers to NACK now (newly detected holes).
	NACK []uint16
}

// ReorderConfig configures the window.
type ReorderConfig struct {
	Window WindowConfig
	// MaxPackets bounds the buffer: past it the head frame is dropped
	// without waiting for its deadline.
	MaxPackets int
	// MaxJump is the largest forward sequence jump treated as loss; a larger
	// one is a sender restart and resets the buffer.
	MaxJump int
	// MaxNACK bounds the sequence numbers one burst NACKs.
	MaxNACK int
	// FrameStart reports whether a payload can open a frame; nil uses
	// PacketStartsFrame.
	FrameStart func([]byte) bool
}

// DefaultReorderConfig is the gateway's default.
func DefaultReorderConfig() ReorderConfig {
	return ReorderConfig{
		Window:     DefaultWindowConfig(),
		MaxPackets: 4096,
		MaxJump:    3000,
		MaxNACK:    512,
	}
}

type bufPkt struct {
	ts      uint32
	marker  bool
	payload []byte
}

type hole struct {
	seenAt   time.Time
	nackedAt time.Time
	nacks    int
}

// Reorder is D6's reorder window. Packets go in with their arrival time;
// complete frames come out in order. A frame that is whole with nothing
// incomplete ahead of it leaves at once. A frame with a hole waits until the
// hole's window closes (window = clamp(1.25 × RTT + 5 ms, 20, 150 ms),
// measured from when the hole was first seen) and is then dropped, along
// with the knowledge that whatever follows needs an IDR. Frames queued behind
// it wait with it, so none waits longer than one window.
//
// Holes are NACKed once, the moment they're seen; the time to the
// retransmit's arrival is the RTT sample (RTTEstimator).
type Reorder struct {
	cfg ReorderConfig
	rtt *RTTEstimator

	started bool
	next    int64 // extended sequence number of the next packet to consume
	highest int64
	buf     map[int64]*bufPkt
	holes   map[int64]*hole
	// late remembers NACK times for holes given up on, so a retransmit
	// arriving after its frame was dropped still yields an RTT sample.
	late map[int64]time.Time

	// boundary is true when the packet before next ended a frame, so the
	// packet at next opens one. False at the start and after a skipped hole.
	boundary bool
	// skipping: dropping the frame at skipTS until its marker.
	skipping bool
	skipTS   uint32
	// gapPending: a gap since the last emitted frame.
	gapPending bool

	nacksSent uint64
	lost      uint64
	first     int64
}

// NewReorder builds a reorder window.
func NewReorder(cfg ReorderConfig) *Reorder {
	if cfg.FrameStart == nil {
		cfg.FrameStart = PacketStartsFrame
	}
	if cfg.MaxPackets <= 0 {
		cfg.MaxPackets = 4096
	}
	if cfg.MaxJump <= 0 {
		cfg.MaxJump = 3000
	}
	if cfg.MaxNACK <= 0 {
		cfg.MaxNACK = 512
	}
	return &Reorder{
		cfg:   cfg,
		rtt:   NewRTTEstimator(cfg.Window),
		buf:   make(map[int64]*bufPkt),
		holes: make(map[int64]*hole),
		late:  make(map[int64]time.Time),
	}
}

// RTT exposes the estimator (status, metrics, tests).
func (r *Reorder) RTT() *RTTEstimator { return r.rtt }

// Window is the current window.
func (r *Reorder) Window() time.Duration { return r.rtt.Window() }

// NACKsSent counts NACKed sequence numbers.
func (r *Reorder) NACKsSent() uint64 { return r.nacksSent }

func (r *Reorder) extend(seq uint16) int64 {
	return r.highest + int64(int16(seq-uint16(r.highest)))
}

// Push adds one packet that arrived at now.
func (r *Reorder) Push(p Packet, now time.Time) ReorderResult {
	var res ReorderResult
	if !r.started {
		r.started = true
		r.next, r.highest, r.first = int64(p.Seq), int64(p.Seq), int64(p.Seq)
		r.buf[r.next] = &bufPkt{ts: p.Timestamp, marker: p.Marker, payload: p.Payload}
		r.drain(now, &res)
		return res
	}
	ext := r.extend(p.Seq)
	switch {
	case ext-r.highest > int64(r.cfg.MaxJump):
		// A sender restart (or a burst no window could recover): start over.
		r.reset(ext)
		res.Gap = true
		r.gapPending = true
	case ext < r.next:
		// Late: its frame was emitted or given up on. Still an RTT sample if
		// it answers a NACK.
		if t, ok := r.late[ext]; ok {
			r.rtt.Sample(now.Sub(t))
			delete(r.late, ext)
		}
		return res
	case r.buf[ext] != nil:
		return res // duplicate
	}
	if h, ok := r.holes[ext]; ok {
		if h.nacks == 1 {
			r.rtt.Sample(now.Sub(h.nackedAt))
		}
		delete(r.holes, ext)
	}
	if ext > r.highest {
		for s := r.highest + 1; s < ext; s++ {
			h := &hole{seenAt: now}
			if len(res.NACK) < r.cfg.MaxNACK {
				h.nackedAt, h.nacks = now, 1
				res.NACK = append(res.NACK, uint16(s))
				r.nacksSent++
			}
			r.holes[s] = h
		}
		r.highest = ext
	}
	r.buf[ext] = &bufPkt{ts: p.Timestamp, marker: p.Marker, payload: p.Payload}
	r.drain(now, &res)
	return res
}

// Tick releases whatever a deadline passing at now allows.
func (r *Reorder) Tick(now time.Time) ReorderResult {
	var res ReorderResult
	if r.started {
		r.drain(now, &res)
	}
	return res
}

// Deadline is when the head frame's window closes, if it waits on a hole.
func (r *Reorder) Deadline() (time.Time, bool) {
	if !r.started {
		return time.Time{}, false
	}
	// Drain stops only on a hole whose window is open, or on a frame still
	// arriving with no hole at all; the first hole from next is therefore the
	// one everything waits on.
	for s := r.next; s <= r.highest; s++ {
		if r.buf[s] == nil {
			if h := r.holes[s]; h != nil {
				return h.seenAt.Add(r.rtt.Window()), true
			}
			return time.Time{}, false
		}
	}
	return time.Time{}, false
}

func (r *Reorder) reset(ext int64) {
	r.buf = make(map[int64]*bufPkt)
	r.holes = make(map[int64]*hole)
	r.late = make(map[int64]time.Time)
	expected := int64(r.Expected())
	r.next, r.highest = ext, ext
	r.first = ext - expected // keep Expected monotonic across a reset
	r.boundary = false
	r.skipping = false
}

// expired reports whether the hole at s has outlived its window (or the
// buffer is over its bound, which drops the head without waiting).
func (r *Reorder) expired(s int64, now time.Time) bool {
	if len(r.buf) > r.cfg.MaxPackets {
		return true
	}
	h := r.holes[s]
	if h == nil {
		return true
	}
	return !now.Before(h.seenAt.Add(r.rtt.Window()))
}

// giveUpHole skips the hole at next.
func (r *Reorder) giveUpHole(res *ReorderResult) {
	if h := r.holes[r.next]; h != nil {
		if h.nacks == 1 {
			r.late[r.next] = h.nackedAt
			r.pruneLate()
		}
		delete(r.holes, r.next)
	}
	r.next++
	r.boundary = false
	r.lost++
	res.Gap = true
	r.gapPending = true
}

// Lost counts sequence numbers given up on (never received in time).
func (r *Reorder) Lost() uint64 { return r.lost }

// Expected counts sequence numbers from the first to the newest.
func (r *Reorder) Expected() uint64 {
	if !r.started {
		return 0
	}
	return uint64(r.highest-r.first) + 1
}

func (r *Reorder) pruneLate() {
	if len(r.late) <= 2048 {
		return
	}
	for s := range r.late {
		if s < r.next-4096 {
			delete(r.late, s)
		}
	}
	if len(r.late) > 2048 {
		r.late = make(map[int64]time.Time)
	}
}

// consume removes the packet at next.
func (r *Reorder) consume() *bufPkt {
	p := r.buf[r.next]
	delete(r.buf, r.next)
	r.next++
	r.boundary = p.marker
	return p
}

func (r *Reorder) startSkip(ts uint32, res *ReorderResult) {
	r.skipping, r.skipTS = true, ts
	res.Dropped++
	res.Gap = true
	r.gapPending = true
}

func (r *Reorder) drain(now time.Time, res *ReorderResult) {
	for r.next <= r.highest {
		p := r.buf[r.next]
		if r.skipping {
			switch {
			case p == nil:
				// A hole inside the frame being dropped: nothing to wait for.
				r.giveUpHole(res)
				continue
			case p.ts == r.skipTS:
				if r.consume().marker {
					r.skipping = false
				}
				continue
			default:
				r.skipping = false // the dropped frame's end was in a hole
			}
		}
		if p == nil {
			if !r.expired(r.next, now) {
				return
			}
			r.giveUpHole(res)
			continue
		}
		if !r.boundary && !r.cfg.FrameStart(p.payload) {
			// The frame lost its head (or this is the tail of one already
			// dropped): it can't be decoded.
			r.startSkip(p.ts, res)
			continue
		}
		// Walk the head frame.
		s := r.next
		for {
			q := r.buf[s]
			if q == nil {
				if s > r.highest {
					return // still arriving; no loss seen yet
				}
				if !r.expired(s, now) {
					return
				}
				r.startSkip(p.ts, res)
				break
			}
			if q.ts != p.ts {
				// The frame ended without a marker: the next timestamp
				// opened a new one.
				r.emit(s-1, res)
				r.boundary = true
				break
			}
			if q.marker {
				r.emit(s, res)
				break
			}
			s++
		}
	}
}

// emit releases packets next..last as one frame.
func (r *Reorder) emit(last int64, res *ReorderResult) {
	f := Frame{Timestamp: r.buf[r.next].ts, AfterGap: r.gapPending}
	r.gapPending = false
	for r.next <= last {
		f.Payloads = append(f.Payloads, r.consume().payload)
	}
	res.Frames = append(res.Frames, f)
}
