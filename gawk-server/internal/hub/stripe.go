package hub

// Striped delivery (R30, docs/35): a viewer's stripe legs are ordinary
// subscribers with a per-leg static filter — leg j of stripe N receives
// exactly the delta datagrams (video chunks + parity) whose stripe ordinal d
// satisfies d mod N == j, and nothing else. The primary session suppresses
// its own delta flow while legs are live, driven by the viewer's StripeState
// datagrams (level state with a TTL, so every lost-message case converges to
// duplicates, never holes).
//
// Nothing here coordinates across legs or pods: the filter parameters are
// fixed at dial time and each serving pod applies them independently, which
// is what makes cross-pod legs correct by construction (docs/35 §5.7).

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// StripeStateTTL bounds how long a primary's delta suppression survives
// without a StripeState refresh (the viewer re-sends at 1 Hz while striped).
// Expiry fails open toward duplicates — a wedged viewer state machine or a
// lost unstripe burst degrades to pre-R30 delivery, never to a starved
// session. A constant beside the eviction thresholds, not a knob.
const StripeStateTTL = 5 * time.Second

// ErrStripeRejected reports malformed or unusable stripe-leg dial parameters.
// Unlike parity negotiation (which degrades — a typo must not strip a
// viewer's protection), a mis-striped leg is useless to BOTH sides: serving
// it a wrong share would manufacture holes, so rejection (a 400 pre-upgrade)
// is the graceful outcome and the viewer simply stays unstriped.
var ErrStripeRejected = errors.New("hub: invalid stripe leg parameters")

// StripeLeg is a validated ?stripe=N&leg=j dial.
type StripeLeg struct {
	N      int // stripe width, [1, wire.MaxStripeLegs]
	Member int // this leg's index, [0, N)
}

// NegotiateStripe validates the stripe query parameters of a subscribe dial.
// isLeg is false with a nil error when neither parameter is present — the
// ordinary-viewer path. Reliable/DVR delivery cannot be striped (those modes
// ride retransmitting carrier streams; there is nothing to win — docs/35 §3),
// and a disabled fleet rejects legs outright (the capability bit is never
// advertised, so a well-behaved viewer never dials one).
func NegotiateStripe(stripeParam, legParam string, enabled, reliable bool) (StripeLeg, bool, error) {
	if stripeParam == "" && legParam == "" {
		return StripeLeg{}, false, nil
	}
	if stripeParam == "" || legParam == "" {
		return StripeLeg{}, false, fmt.Errorf("%w: stripe and leg must be given together", ErrStripeRejected)
	}
	if !enabled {
		return StripeLeg{}, false, fmt.Errorf("%w: striped delivery is disabled on this relay", ErrStripeRejected)
	}
	if reliable {
		return StripeLeg{}, false, fmt.Errorf("%w: striping applies to datagram delivery only", ErrStripeRejected)
	}
	n, err := strconv.Atoi(stripeParam)
	if err != nil || n < 1 || n > wire.MaxStripeLegs {
		return StripeLeg{}, false, fmt.Errorf("%w: stripe %q, want [1, %d]", ErrStripeRejected, stripeParam, wire.MaxStripeLegs)
	}
	member, err := strconv.Atoi(legParam)
	if err != nil || member < 0 || member >= n {
		return StripeLeg{}, false, fmt.Errorf("%w: leg %q, want [0, %d)", ErrStripeRejected, legParam, n)
	}
	return StripeLeg{N: n, Member: member}, true, nil
}

// SubscribeStripeLeg subscribes one stripe leg (R30, docs/35 §5.1): a plain
// datagram subscriber that receives only its share of delta datagrams —
// never keyframe streams, audio, control datagrams or join primes (the
// primary already has all of those). parityK is the leg's served parity
// prefix, negotiated exactly like the primary's so the leg's parity share
// composes with R29 unchanged.
//
// A leg counts against MaxSubscribers/MaxTotalSubscribers like any external
// session (it is real per-subscriber state, and the caps are what bound a
// counterfeit leg flood) but never in the R18 viewer count — one watching
// human is one count.
func (r *Registry) SubscribeStripeLeg(id string, conn Conn, leg StripeLeg, parityK int) (*Subscriber, error) {
	if leg.N < 1 || leg.N > wire.MaxStripeLegs || leg.Member < 0 || leg.Member >= leg.N {
		return nil, ErrStripeRejected
	}
	return r.subscribeOpts(id, conn, subscribeOpts{
		stripeLeg:       true,
		stripeN:         uint8(leg.N),
		stripeMember:    uint8(leg.Member),
		parityK:         parityK,
		parityRequested: parityK,
	})
}

// ApplyStripeState applies a viewer's StripeState (0x10) to its primary
// session. Only an external, datagram-delivery, non-leg subscriber may be
// suppressed; anywhere else the message is inert, exactly like any other
// unrecognized datagram (docs/35 §5.3).
//
// Level semantics: a striped state arms (or re-arms) a StripeStateTTL
// deadline; an unstriped state clears it immediately. Transitions are
// counted only when the level actually flips, so the 1 Hz refresh does not
// inflate the counter.
func (s *Subscriber) ApplyStripeState(st wire.StripeState) {
	if s.internal || s.reliable || s.dvr != nil || s.stripeLeg {
		return
	}
	if st.Striped {
		prev := s.stripeUntil.Swap(time.Now().Add(StripeStateTTL).UnixNano())
		s.stripeNInfo.Store(uint32(st.StripeN))
		if prev == 0 {
			s.stripeTransitions.Add(1)
		}
		return
	}
	if s.stripeUntil.Swap(0) != 0 {
		s.stripeTransitions.Add(1)
	}
	s.stripeNInfo.Store(0)
}

// stripeSuppressed reports whether delta datagrams are currently suppressed
// on this (primary) session. Reads one atomic; the deadline comparison is
// what makes a stale suppression expire without any timer.
func (s *Subscriber) stripeSuppressed(nowNanos int64) bool {
	until := s.stripeUntil.Load()
	return until != 0 && nowNanos < until
}

// wantsDeltaLocked is the per-subscriber routing decision for a delta
// datagram (video chunk or parity symbol) with stripe ordinal ord. Caller
// holds r.mu; nowNanos is the fan-out's lazily-computed clock read.
func (s *Subscriber) wantsDeltaLocked(ord uint32, nowNanos int64) bool {
	if s.stripeLeg {
		return ord%uint32(s.stripeN) == uint32(s.stripeMember)
	}
	return !s.stripeSuppressed(nowNanos)
}

// stripeStatN is the /statusz stripe width of a subscriber row: a leg's
// fixed filter width, a striped primary's viewer-reported width, 0 (omitted)
// for everyone else.
func stripeStatN(s *Subscriber, striped bool) int {
	if s.stripeLeg {
		return int(s.stripeN)
	}
	if striped {
		return int(s.stripeNInfo.Load())
	}
	return 0
}

// deltaOrdinalIfStriped classifies a fanned-out datagram for stripe routing:
// ord >= 0 with isDelta for video chunks and parity symbols (the datagrams a
// leg may carry), isDelta false for everything else (control and audio,
// which never reach legs and are never suppressed on a primary).
func deltaOrdinal(dgram []byte) (ord uint32, isDelta bool) {
	_, typ, err := wire.PeekType(dgram)
	if err != nil {
		return 0, false
	}
	switch typ {
	case wire.TypeVideoChunk:
		h, _, err := wire.ParseVideoChunk(dgram)
		if err != nil {
			return 0, false
		}
		return wire.StripeOrdinal(h.ChunkIndex, h.ChunkCount, -1), true
	case wire.TypeParityChunk:
		h, _, err := wire.ParseParityChunk(dgram)
		if err != nil {
			return 0, false
		}
		return wire.StripeOrdinal(0, h.ChunkCount, int(h.ParityIndex)), true
	default:
		return 0, false
	}
}
