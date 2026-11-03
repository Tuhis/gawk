package media

import "time"

// WindowConfig sizes D6's reorder window. Provisional until WG0 measures the
// loss rate and the recovery share on the reference path (docs/83 D6).
type WindowConfig struct {
	// InitialRTT is assumed before the first sample (50 ms).
	InitialRTT time.Duration
	// Floor and Cap clamp the window (60 ms, 150 ms).
	Floor, Cap time.Duration
	// Multiplier and Offset: window = Multiplier × RTT + Offset (1.25, 5 ms).
	Multiplier float64
	Offset     time.Duration
}

// DefaultWindowConfig is D6's provisional sizing (until WG0).
func DefaultWindowConfig() WindowConfig {
	return WindowConfig{
		InitialRTT: 50 * time.Millisecond,
		// 60 ms, not D6's first 20 ms (docs/83 §13.4, 2026-11-04): on
		// ioio the smoothed RTT swung 20–60 ms within one OBS session and
		// about 1 NACK in 5 missed a window sized on the mean.
		Floor:      60 * time.Millisecond,
		Cap:        150 * time.Millisecond,
		Multiplier: 1.25,
		Offset:     5 * time.Millisecond,
	}
}

// RTTEstimator measures the OBS → gateway round trip from NACKs: the time
// from a NACK to its retransmitted packet's arrival. As in RFC 6298 the first
// sample sets the estimate directly and later ones are smoothed with
// α = 1/8, like TCP's SRTT (D6).
//
// Karn's rule applies: only a packet NACKed exactly once is sampled, since a
// retransmit of a twice-NACKed packet can't say which NACK it answers. A
// packet that was merely reordered, not lost, still yields a (short) sample;
// without RTX nothing distinguishes the two, and the window's floor absorbs
// it.
type RTTEstimator struct {
	cfg     WindowConfig
	srtt    time.Duration
	sampled bool
	samples uint64
}

// NewRTTEstimator builds an estimator.
func NewRTTEstimator(cfg WindowConfig) *RTTEstimator {
	return &RTTEstimator{cfg: cfg, srtt: cfg.InitialRTT}
}

// Sample records one NACK → retransmit time.
func (e *RTTEstimator) Sample(d time.Duration) {
	if d < 0 {
		return
	}
	e.samples++
	if !e.sampled {
		e.srtt, e.sampled = d, true
		return
	}
	e.srtt += (d - e.srtt) / 8
}

// RTT is the current estimate.
func (e *RTTEstimator) RTT() time.Duration { return e.srtt }

// Samples is how many samples have been taken.
func (e *RTTEstimator) Samples() uint64 { return e.samples }

// Window is clamp(Multiplier × RTT + Offset, Floor, Cap).
func (e *RTTEstimator) Window() time.Duration {
	w := time.Duration(e.cfg.Multiplier*float64(e.srtt)) + e.cfg.Offset
	if w < e.cfg.Floor {
		w = e.cfg.Floor
	}
	if w > e.cfg.Cap {
		w = e.cfg.Cap
	}
	return w
}
