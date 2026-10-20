package hub

import (
	"strconv"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// Per-subscriber forward-parity filtering (docs/34 §4.1).
//
// The relay computes NOTHING here. The producer emits up to the fleet's parity
// level, and each subscriber is served a PREFIX of those symbols matching its
// own k, which works because P alone is the k=1 code. The relay stays a byte
// forwarder: symbols are shared across the fan-out like data chunks, so only
// egress varies per subscriber, never CPU.

// NegotiateParity resolves the ?parity= query param against the fleet default.
//
// Returns (requested, served). They differ whenever a viewer asks for more
// than the fleet emits; both are surfaced because one number cannot
// distinguish "this viewer chose less" from "this viewer was refused".
//
// Every delivery mode negotiates the same way (docs/75 D11, revising docs/34
// §3): carrier retransmission repairs loss between the relay and the viewer,
// but a chunk lost on the broadcaster's uplink never reached the relay, and
// only parity brings it back — for a reliable or DVR viewer as for a
// live-edge one.
//
// No value can reject a session: every unusable one degrades to a working mode.
func NegotiateParity(param string, fleetDefault int) (requested, served int) {
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
	if served < 0 {
		served = 0
	}
	return requested, served
}

// isParityDatagram reports whether a fanned-out datagram is a parity symbol.
// Deliberately separate from isVideoChunkDatagram: parity must not be counted
// as a relayed frame by the ingress-loss window. Where parity travels WITH the
// video chunks — the DVR ring, the carrier's dead-GOP shed — the check is
// isCarrierMediaDatagram.
func isParityDatagram(dgram []byte) bool {
	_, typ, err := wire.PeekType(dgram)
	return err == nil && typ == wire.TypeParityChunk
}

// isCarrierMediaDatagram reports whether a datagram belongs to a GOP's
// carrier: a video delta chunk, or a parity symbol protecting one. Both enter
// the DVR ring, a DVR subscriber reads both from the ring rather than its
// queue, and both are shed with a dead GOP (docs/75 D11).
func isCarrierMediaDatagram(dgram []byte) bool {
	_, typ, err := wire.PeekType(dgram)
	return err == nil && (typ == wire.TypeVideoChunk || typ == wire.TypeParityChunk)
}

// parityIndexOf returns the symbol index of a parity datagram, or -1 if it is
// not one.
func parityIndexOf(dgram []byte) int {
	h, _, err := wire.ParseParityChunk(dgram)
	if err != nil {
		return -1
	}
	return int(h.ParityIndex)
}
