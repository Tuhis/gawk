package hub

import (
	"strconv"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// Per-subscriber forward-parity filtering (R29, docs/34).
//
// The relay computes NOTHING here. The producer emits up to the fleet's parity
// level, and each subscriber is served a PREFIX of those symbols matching its
// own k — which works because P alone is the k=1 code (docs/34 §4.1). That is
// what keeps the relay a byte forwarder, keeps the origin/edge cascade working
// unchanged, and makes per-subscriber k cheaper than a common setting: the
// symbols are shared across the fan-out exactly like data chunks, so only
// egress varies, never CPU.

// NegotiateParity resolves the ?parity= query param against the fleet default
// and the subscriber's delivery mode.
//
// Returns (requested, served). They differ whenever a viewer asks for more
// than the fleet emits, or asks at all on a reliable/DVR subscription — and
// surfacing both is deliberate: a single number could not distinguish "this
// viewer chose less" from "this viewer was refused", which is the R19
// "reliable requested / datagrams served" lesson (docs/24).
//
// No value can reject a session. Every unusable one degrades to a working
// mode, matching the delivery negotiation next to it.
func NegotiateParity(param string, fleetDefault int, reliable bool) (requested, served int) {
	requested = fleetDefault
	if param != "" {
		// Garbage and out-of-range both fall back to the fleet default rather
		// than to zero: a typo should not silently strip a viewer's
		// protection.
		if v, err := strconv.Atoi(param); err == nil && v >= 0 && v <= wire.MaxParitySymbols {
			requested = v
		}
	}
	served = requested
	if served > fleetDefault {
		// A viewer cannot conjure symbols the producer never emitted.
		served = fleetDefault
	}
	if reliable {
		// Carrier delivery already recovers loss via QUIC retransmission, so
		// parity would be pure egress waste (docs/34 §3).
		served = 0
	}
	if served < 0 {
		served = 0
	}
	return requested, served
}

// isParityDatagram reports whether a fanned-out datagram is a parity symbol.
// Deliberately separate from isVideoChunkDatagram: parity must NOT enter the
// DVR ring (its consumers are reliable and cannot use it) and must not be
// counted as a relayed frame by the R9 ingress-loss window.
func isParityDatagram(dgram []byte) bool {
	_, typ, err := wire.PeekType(dgram)
	return err == nil && typ == wire.TypeParityChunk
}

// parityIndexOf returns the symbol index of a parity datagram, or -1 if it is
// not one. Used by the fan-out to decide the per-subscriber prefix without
// re-parsing the whole header.
func parityIndexOf(dgram []byte) int {
	h, _, err := wire.ParseParityChunk(dgram)
	if err != nil {
		return -1
	}
	return int(h.ParityIndex)
}
