package transport

import "time"

// docs/75 D10 (docs/57 D9's rule): every GOP opens a new uplink carrier, and
// quic-go starts each stream at a 512 KiB receive window, auto-tuning only
// once data is flowing. A GOP at 1440p (~1.8 MB) does not fit, so the first
// GOPs of a session block on flow control. The windows are sized instead from
// the highest bitrate the relay expects:
//
//	stream     ≥ bitrate × (deadline + RTT) × 2
//	connection ≈ 1.5 × stream (quic-go's own ratio)
//
// which is 2 MB / 3 MB at the default 50 Mbps. A window is an allowance, not
// an allocation: the relay holds only bytes that arrive out of order, so the
// viewer connections that share the config pay nothing.
const (
	// uplinkCarrierDeadline is the broadcaster's retire deadline for a
	// superseded carrier (docs/75 D3).
	uplinkCarrierDeadline = 150 * time.Millisecond
	// uplinkWindowRTT is docs/57 D9's RTT term.
	uplinkWindowRTT = 10 * time.Millisecond

	// quic-go v0.62's defaults (internal/protocol), restated because the
	// package is internal: the floors below which a derived window never
	// goes, and the maxima auto-tuning may still reach.
	quicDefaultInitialStreamWindow     = 512 << 10
	quicDefaultInitialConnectionWindow = 768 << 10
	quicDefaultMaxStreamWindow         = 6 << 20
	quicDefaultMaxConnectionWindow     = 15 << 20
)

// uplinkReceiveWindows derives the relay's initial QUIC receive windows from
// -uplink-max-bitrate in bytes/s. Zero returns zeros, which quic-go reads as
// its own defaults. The result is clamped between quic-go's initial defaults
// and its maxima, so a low setting never shrinks a window and a high one
// never exceeds what auto-tuning could reach anyway.
func uplinkReceiveWindows(bytesPerSec int64) (stream, conn uint64) {
	if bytesPerSec <= 0 {
		return 0, 0
	}
	span := uplinkCarrierDeadline + uplinkWindowRTT
	s := float64(bytesPerSec) * span.Seconds() * 2
	stream = clampWindow(uint64(s+0.5), quicDefaultInitialStreamWindow, quicDefaultMaxStreamWindow)
	conn = clampWindow(stream+stream/2, quicDefaultInitialConnectionWindow, quicDefaultMaxConnectionWindow)
	return stream, conn
}

func clampWindow(v, lo, hi uint64) uint64 {
	return min(max(v, lo), hi)
}
