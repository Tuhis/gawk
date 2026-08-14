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

// DefaultStripeLegLease is how long a stripe leg may go without ANY inbound
// datagram before the relay reaps it as orphaned (docs/35 §14 Decision 5).
// The viewer heartbeats each leg with its 1 Hz StripeState refresh, so 20 s
// is 20 missed sends — generous against background-tab timer throttling
// while still bounding how long a dead viewer's cross-pod legs hold
// subscriber slots (the case the ownership reap cannot see, §5.7). Armed at
// subscribe: since owner enforcement, every admitted leg has promised the
// heartbeat, so a leg that never sends anything IS the failure mode. The
// close is CloseCodeStripeLegOrphaned, non-terminal — a live viewer's
// leg-death fallback re-engages. Overridable via Options for tests only,
// like DVRProgressTimeout; there is deliberately no flag.
const DefaultStripeLegLease = 20 * time.Second

// ownerTokenLen is the exact length of an ?owner= token: 8 random bytes,
// lowercase hex. Chosen by the viewer per transport attempt (docs/35 §14
// Decision 1) — long enough that guessing another viewer's token is not a
// practical attack, short enough to cost nothing on every dial.
const ownerTokenLen = 16

// ValidOwnerToken reports whether s is a well-formed ?owner= token: exactly
// 16 lowercase-hex characters. Exported for the transport's primary dial
// path, where an invalid token degrades to unowned (the session works but
// can never stripe) rather than rejecting — only LEG dials require it.
func ValidOwnerToken(s string) bool {
	if len(s) != ownerTokenLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// ErrStripeRejected reports malformed or unusable stripe-leg dial parameters.
// Unlike parity negotiation (which degrades — a typo must not strip a
// viewer's protection), a mis-striped leg is useless to BOTH sides: serving
// it a wrong share would manufacture holes, so rejection (a 400 pre-upgrade)
// is the graceful outcome and the viewer simply stays unstriped.
var ErrStripeRejected = errors.New("hub: invalid stripe leg parameters")

// StripeLeg is a validated ?stripe=N&leg=j&owner=<token> dial.
type StripeLeg struct {
	N      int // stripe width, [1, wire.MaxStripeLegs]
	Member int // this leg's index, [0, N)
	// Owner is the viewer-minted token shared with the primary dial (docs/35
	// §14 Decision 1): the registry's only handle tying one viewer's sessions
	// together. Required on every leg — an unowned leg is unreapable, which
	// is exactly the orphan class §14 exists to close.
	Owner string
}

// NegotiateStripe validates the stripe query parameters of a subscribe dial.
// isLeg is false with a nil error when neither stripe parameter is present —
// the ordinary-viewer path. Reliable/DVR delivery cannot be striped (those
// modes ride retransmitting carrier streams; there is nothing to win —
// docs/35 §3), and a disabled fleet rejects legs outright (the capability
// bit is never advertised, so a well-behaved viewer never dials one).
//
// A leg additionally REQUIRES a valid ?owner= token (docs/35 §14, owner
// decision 2026-08-13): an unowned leg could outlive its viewer with nothing
// able to reap it, so — like a mis-striped leg — rejection is the graceful
// outcome and the (pre-token) viewer simply stays unstriped.
func NegotiateStripe(stripeParam, legParam, ownerParam string, enabled, reliable bool) (StripeLeg, bool, error) {
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
	if !ValidOwnerToken(ownerParam) {
		return StripeLeg{}, false, fmt.Errorf("%w: a leg requires a valid owner token", ErrStripeRejected)
	}
	n, err := strconv.Atoi(stripeParam)
	if err != nil || n < 1 || n > wire.MaxStripeLegs {
		return StripeLeg{}, false, fmt.Errorf("%w: stripe %q, want [1, %d]", ErrStripeRejected, stripeParam, wire.MaxStripeLegs)
	}
	member, err := strconv.Atoi(legParam)
	if err != nil || member < 0 || member >= n {
		return StripeLeg{}, false, fmt.Errorf("%w: leg %q, want [0, %d)", ErrStripeRejected, legParam, n)
	}
	return StripeLeg{N: n, Member: member, Owner: ownerParam}, true, nil
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
	// Guards a future transport refactor: NegotiateStripe already rejected
	// unowned legs pre-upgrade, and an unowned leg admitted here would be
	// exactly the unreapable orphan §14 closes.
	if !ValidOwnerToken(leg.Owner) {
		return nil, ErrStripeRejected
	}
	return r.subscribeOpts(id, conn, subscribeOpts{
		stripeLeg:       true,
		stripeN:         uint8(leg.N),
		stripeMember:    uint8(leg.Member),
		owner:           leg.Owner,
		parityK:         parityK,
		parityRequested: parityK,
	})
}

// ApplyStripeState applies a viewer's StripeState (0x10) to its primary
// session. Only an external, datagram-delivery, non-leg, OWNED subscriber
// may be suppressed; anywhere else the message is inert, exactly like any
// other unrecognized datagram (docs/35 §5.3). The owner requirement (§14,
// 2026-08-13) is what completes the striped-surface enforcement: an unowned
// primary can never suppress its deltas, so a client that somehow dials
// tagless legs-and-primary degrades to duplicates, never to a topology the
// registry cannot reap.
//
// Level semantics: a striped state arms (or re-arms) a StripeStateTTL
// deadline; an unstriped state clears it immediately. Transitions are
// counted only when the level actually flips, so the 1 Hz refresh does not
// inflate the counter.
func (s *Subscriber) ApplyStripeState(st wire.StripeState) {
	if s.internal || s.reliable || s.dvr != nil || s.stripeLeg || s.owner == "" {
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

// NoteLegAlive renews a stripe leg's liveness lease (docs/35 §14
// Decision 5). The transport calls it for every datagram the leg session
// receives — the viewer heartbeats with its 1 Hz StripeState refresh, but
// any inbound datagram is proof of life. A no-op on non-leg subscribers.
func (s *Subscriber) NoteLegAlive() {
	if !s.stripeLeg {
		return
	}
	s.legLastInbound.Store(time.Now().UnixNano())
}

// legLeaseWatch reaps this leg once it has gone lease without any inbound
// datagram — the cross-pod half of the §14 invariant, covering legs whose
// primary this pod never saw (§5.7) and whose viewer died without closing
// anything. Armed at subscribe: owner enforcement means every admitted leg
// has promised the heartbeat. Runs on its own goroutine; exits when the
// subscriber closes for any other reason (s.done).
func (s *Subscriber) legLeaseWatch(lease time.Duration) {
	timer := time.NewTimer(lease)
	defer timer.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-timer.C:
			elapsed := time.Since(time.Unix(0, s.legLastInbound.Load()))
			if remaining := lease - elapsed; remaining > 0 {
				timer.Reset(remaining)
				continue
			}
			if s.closed.Load() {
				// A concurrent Close (session end, owner reap, expiry) beat
				// the timer — this is not a reap and must not count as one.
				return
			}
			s.hub.log.Warn("reaping orphaned stripe leg: liveness lease expired",
				"subscriber", s.statsKey, "lease", lease)
			s.hub.countStripeLegsReaped(1)
			_ = s.sender.CloseWithError(uint32(wire.CloseCodeStripeLegOrphaned),
				"stripe leg orphaned: liveness lease expired")
			s.Close()
			return
		}
	}
}

// orphanedLegsLocked collects the stripe legs this closing PRIMARY owns —
// same hub, same owner token — and counts the reap. Caller holds r.mu and
// closes the returned legs outside it (stream/session ops must never hold
// the registry lock; the CloseInternalSubscribers pattern). Nil for legs,
// internal sessions and unowned subscribers, and when the hub is already
// deregistered: there the expiry sweep is closing every subscriber with the
// terminal 4000, which must not be raced into a non-terminal code that
// would invite a reconnect into a dead broadcast.
func (s *Subscriber) orphanedLegsLocked() []*Subscriber {
	if s.stripeLeg || s.internal || s.owner == "" {
		return nil
	}
	b := s.hub
	if b.registry.hubs[b.id] != b {
		return nil
	}
	var legs []*Subscriber
	for l := range b.subs {
		if l.stripeLeg && l.owner == s.owner {
			legs = append(legs, l)
		}
	}
	if len(legs) > 0 {
		b.stripeLegsReaped += uint64(len(legs))
	}
	return legs
}

// reapLegs closes legs a primary's Close orphaned (docs/35 §14 Decision 2).
// Outside the registry lock. The owner token itself is deliberately not
// logged — it is never exposed anywhere the viewer didn't put it.
func (s *Subscriber) reapLegs(legs []*Subscriber) {
	if len(legs) == 0 {
		return
	}
	s.hub.log.Warn("reaping orphaned stripe legs: owning primary session ended",
		"primary", s.statsKey, "legs", len(legs))
	for _, l := range legs {
		_ = l.sender.CloseWithError(uint32(wire.CloseCodeStripeLegOrphaned),
			"stripe leg orphaned: owning primary session ended")
		l.Close()
	}
}

// countStripeLegsReaped credits a lease-fired reap. Like countBandwidthDrop,
// the hub may already be deregistered by the time the watch fires — credit
// the registry totals then (counters survive their owner, CODE-REVIEW).
func (b *broadcastHub) countStripeLegsReaped(n uint64) {
	r := b.registry
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hubs[b.id] == b {
		b.stripeLegsReaped += n
	} else {
		r.totalStripeLegsReaped += n
	}
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
