package engine

import "time"

// Clock is the engine's single time source. Everything that produces a
// timestamp — frame arrival stamps and the TimeSync t0 alike — reads it.
//
// That sharing is load-bearing, not tidiness (docs/19 Decision 6). The
// broadcaster publishes a ClockMapping saying
//
//	relayClockUs = frameTimestampUs + offsetUs
//
// where offsetUs comes from TimeSync samples taken against the relay's clock.
// If frames were stamped on a different timeline than the pings, that equation
// would relate two unrelated clocks: every viewer's absolute capture→render
// latency would be wrong, while still looking entirely plausible. There is no
// test the viewer could run to notice. So the engine takes one clock and
// threads it everywhere, and TestOneClockFeedsFramesAndTimeSync guards it.
//
// Monotonic on purpose, mirroring the relay's own processStart/relayNowUs and
// the browser broadcaster's performance.now(): a wall-clock step mid-session
// (NTP, a laptop waking) must not jump the mapping.
type Clock interface {
	// NowUs returns microseconds since an arbitrary, fixed origin. Only
	// differences are meaningful.
	NowUs() uint64
}

type monotonicClock struct{ start time.Time }

// NewClock returns a Clock anchored at the moment of the call. time.Since
// reads the monotonic reading embedded in the time.Time, so it is immune to
// wall-clock adjustments.
func NewClock() Clock { return &monotonicClock{start: time.Now()} }

func (c *monotonicClock) NowUs() uint64 { return uint64(time.Since(c.start).Microseconds()) }

// FakeClock is a Clock tests advance by hand. The engine's cadences (ping
// interval, mapping interval) are time-driven, so every test that asserts one
// drives this instead of sleeping.
type FakeClock struct{ Us uint64 }

func (c *FakeClock) NowUs() uint64 { return c.Us }

// Advance moves the clock forward by d.
func (c *FakeClock) Advance(d time.Duration) { c.Us += uint64(d.Microseconds()) }
