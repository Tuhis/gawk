package transport

import "github.com/Tuhis/gawk/gawk-server/wire"

// rewriteClockMappingBack translates a ClockMapping from PoP-clock into
// home-clock terms for an R74 uplink leg (docs/76 item 26): the PoP's
// mapping says popUs = tsUs + X (its broadcaster's TimeSync ran against the
// PoP), and the leg's estimator homeUs ≈ popUs + est, so homeUs = tsUs +
// (X + est). It is the edge's forward rewrite (rewriteClockMapping) pointed
// the other way, and composes with it: home's edges subtract their own
// offset from what this sends. ok is false — the mapping must be held,
// never served across an arbitrary epoch gap — until the estimator has a
// sample, or when the datagram is malformed.
func rewriteClockMappingBack(dgram []byte, est *timeSyncEstimator) ([]byte, bool) {
	x, err := wire.ParseClockMapping(dgram)
	if err != nil {
		return nil, false
	}
	off, _, ok := est.best()
	if !ok {
		return nil, false
	}
	return wire.AppendClockMapping(nil, x+off), true
}
