package wire

// Striped delivery for the datagram delta path (R30, docs/35).
//
// A striping viewer holds its primary subscribe session plus up to
// MaxStripeLegs additional "leg" sessions, each dialed with ?stripe=N&leg=j
// and carrying exactly the delta datagrams whose ordinal d satisfies
// d mod N == j (data chunk i has ordinal i; parity symbol r has ordinal
// chunkCount+r, so parity keeps its measured tail-of-burst position on every
// leg). The only new wire message is StripeState: the viewer telling the
// relay to stop sending delta datagrams on the primary while the legs carry
// them. Everything else — chunk and parity headers, keyframe streams, audio,
// control — is byte-identical to pre-R30; striping redistributes datagrams,
// it never rewrites them (docs/35 §5.2).

import "fmt"

const (
	// StripeStateSize is the exact size of a StripeState datagram:
	// version, type, flags, stripeN, reserved.
	StripeStateSize = 5

	// MaxStripeLegs bounds a viewer's stripe width. Evidence-bound, not
	// tuned: docs/34 finding 5 measured per-connection headroom at up to 4
	// simultaneous connections and no further, and 4 legs at the burst
	// target of 6 chunks cover every delta frame size observed (~23 chunks
	// max). A constant, not a knob (docs/35 §11) — revisit only with a new
	// finding-5-shaped measurement at higher N.
	MaxStripeLegs = 4

	// stripeFlagStriped is bit 0 of the StripeState flags byte: the viewer
	// has live legs and wants delta datagrams suppressed on this session.
	stripeFlagStriped = 0x01
)

// StripeState is the primary-suppression signal (R30, docs/35 §5.3):
// client→relay, datagram, valid only on an external datagram-delivery
// subscribe session that is not itself a leg.
//
// It is LEVEL state, not an edge: the viewer re-sends it at 1 Hz while
// striped (plus a short burst on the unstripe transition), and the relay
// expires the suppression if refreshes stop — so every lost-message state
// converges to duplicates, never to holes.
type StripeState struct {
	// Striped reports whether delta datagrams should be suppressed on the
	// session this message arrived on.
	Striped bool
	// StripeN is the viewer's current stripe width, informational (for
	// /statusz and metrics — the relay's own behavior never depends on it).
	// In [1, MaxStripeLegs] when Striped, 0 otherwise.
	StripeN uint8
}

// AppendStripeState appends a StripeState datagram to dst.
func AppendStripeState(dst []byte, s StripeState) ([]byte, error) {
	if err := validateStripeState(s); err != nil {
		return nil, err
	}
	var flags uint8
	if s.Striped {
		flags |= stripeFlagStriped
	}
	dst = append(dst, Version, TypeStripeState, flags, s.StripeN, 0)
	return dst, nil
}

// ParseStripeState parses a StripeState datagram. Strict: unknown flag bits
// are rejected rather than ignored — a future revision of this message would
// gate on a new RelayCapabilities bit, never on old relays guessing.
func ParseStripeState(b []byte) (StripeState, error) {
	if len(b) != StripeStateSize {
		return StripeState{}, fmt.Errorf("%w: %d bytes, want exactly %d",
			ErrShortDatagram, len(b), StripeStateSize)
	}
	if b[0] != Version {
		return StripeState{}, fmt.Errorf("%w: 0x%02x", ErrBadVersion, b[0])
	}
	if b[1] != TypeStripeState {
		return StripeState{}, fmt.Errorf("%w: got 0x%02x, want stripe state 0x%02x",
			ErrBadType, b[1], TypeStripeState)
	}
	if b[2]&^uint8(stripeFlagStriped) != 0 {
		return StripeState{}, fmt.Errorf("%w: unknown stripe flags 0x%02x", ErrBadType, b[2])
	}
	s := StripeState{Striped: b[2]&stripeFlagStriped != 0, StripeN: b[3]}
	if err := validateStripeState(s); err != nil {
		return StripeState{}, err
	}
	return s, nil
}

// validateStripeState holds the one shape rule for both directions: a striped
// state names its width, an unstriped one names none.
func validateStripeState(s StripeState) error {
	if s.Striped {
		if s.StripeN < 1 || s.StripeN > MaxStripeLegs {
			return fmt.Errorf("%w: stripeN %d, want [1, %d] while striped",
				ErrBadChunkCount, s.StripeN, MaxStripeLegs)
		}
		return nil
	}
	if s.StripeN != 0 {
		return fmt.Errorf("%w: stripeN %d, want 0 while not striped",
			ErrBadChunkCount, s.StripeN)
	}
	return nil
}

// StripeOrdinal returns the stripe ordinal of a delta datagram: data chunk i
// has ordinal i, parity symbol r over an n-chunk frame has ordinal n+r. Leg j
// of stripe N carries the datagrams with StripeOrdinal % N == j.
func StripeOrdinal(chunkIndex, chunkCount uint16, parityIndex int) uint32 {
	if parityIndex >= 0 {
		return uint32(chunkCount) + uint32(parityIndex)
	}
	return uint32(chunkIndex)
}
