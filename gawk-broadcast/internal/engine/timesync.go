package engine

import (
	"sync"
	"time"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// The TimeSync client, mirroring gawk-app/src/transport/time-sync.ts. Both
// broadcasters run one over their existing session: ping the relay, and from
// each echoed reply take an NTP-style sample mapping this machine's clock onto
// the relay's monotonic clock:
//
//	rtt      = t1 − t0
//	offsetUs = serverTimeUs − (t0 + rtt/2)      (relayUs ≈ localUs + offsetUs)
//
// The lowest-RTT sample in a rolling window wins: queuing delay is what makes
// the out/back asymmetry (the one unmeasurable error) large, so the fastest
// exchange is the most symmetric one, and that sample's rtt/2 bounds the error.
const (
	// TimeSyncInterval is how often we ping. It mirrors the TS constant and
	// must stay a constant, not a flag: the relay rate-caps TimeSync replies
	// at 5/s per session (also a constant, transport/server.go), and a knob
	// here would let a user silently configure their own measurements into
	// being dropped.
	TimeSyncInterval = 2 * time.Second
	// TimeSyncSampleWindow is the rolling sample count (TS: TIME_SYNC_SAMPLE_WINDOW).
	TimeSyncSampleWindow = 8
	// ClockMappingInterval is how often the broadcaster re-publishes its
	// ClockMapping (TS: CLOCK_MAPPING_INTERVAL_MS).
	ClockMappingInterval = 5 * time.Second
)

// TimeSyncStats is the winning sample.
type TimeSyncStats struct {
	// OffsetUs: relayClockUs ≈ localClockUs + OffsetUs (signed).
	OffsetUs int64
	// RttMs is the round-trip of the winning sample — also a self-owned RTT
	// for this leg, independent of any getStats() (which no browser ships
	// today; docs/13 D7 — the native path never had it either).
	RttMs float64
}

type timeSyncSample struct {
	offsetUs int64
	rttUs    uint64
}

// TimeSyncEstimator is the pure half: samples in, best out. Mirrors the TS
// class of the same name so the two implementations can be compared on
// identical sample sequences.
type TimeSyncEstimator struct {
	samples []timeSyncSample
}

// Record adds a sample. t0Us is when we sent, serverTimeUs is the relay's
// clock as echoed, t1Us is when the reply landed.
func (e *TimeSyncEstimator) Record(t0Us, serverTimeUs, t1Us uint64) {
	if t1Us < t0Us {
		return // impossible exchange (bogus/forged echo)
	}
	rttUs := t1Us - t0Us
	offsetUs := int64(serverTimeUs) - int64(t0Us+rttUs/2)
	e.samples = append(e.samples, timeSyncSample{offsetUs: offsetUs, rttUs: rttUs})
	if len(e.samples) > TimeSyncSampleWindow {
		e.samples = e.samples[1:]
	}
}

// Best returns the lowest-RTT sample in the window, and whether there is one.
func (e *TimeSyncEstimator) Best() (TimeSyncStats, bool) {
	if len(e.samples) == 0 {
		return TimeSyncStats{}, false
	}
	best := e.samples[0]
	for _, s := range e.samples {
		if s.rttUs < best.rttUs {
			best = s
		}
	}
	return TimeSyncStats{OffsetUs: best.offsetUs, RttMs: float64(best.rttUs) / 1000}, true
}

// TimeSyncClient owns the estimator and the ping encoding. The caller drives
// the cadence (Session runs a ticker) and feeds every received datagram
// through HandleDatagram.
type TimeSyncClient struct {
	send  func([]byte) error
	clock Clock

	mu  sync.Mutex
	est TimeSyncEstimator
}

func NewTimeSyncClient(send func([]byte) error, clock Clock) *TimeSyncClient {
	return &TimeSyncClient{send: send, clock: clock}
}

// Ping sends one TimeSync request. Send failures are swallowed: a ping must
// never take the pipeline down — the session's own lifecycle owns that.
func (c *TimeSyncClient) Ping() {
	_ = c.send(wire.AppendTimeSync(nil, c.clock.NowUs(), 0))
}

// HandleDatagram reports whether the datagram was a TimeSync message. True
// means "consumed here" — well-formed or not — so it never reaches the video
// path. False means "not mine, route it on".
func (c *TimeSyncClient) HandleDatagram(dgram []byte) bool {
	if len(dgram) < 2 || dgram[1] != wire.TypeTimeSync {
		return false
	}
	if len(dgram) == wire.TimeSyncSize {
		if clientUs, serverUs, err := wire.ParseTimeSync(dgram); err == nil {
			c.mu.Lock()
			c.est.Record(clientUs, serverUs, c.clock.NowUs())
			c.mu.Unlock()
		}
		// malformed: dropped (strict parsing, R2 discipline)
	}
	return true
}

// Reset discards every sample.
//
// Mandatory when auto-resume reclaims the broadcast on a new relay session:
// the reference these samples measure against is the relay's *process*
// monotonic clock (transport/server.go's processStart), so a reclaim that
// lands on a different pod — the normal case behind a load balancer — is
// measuring a different origin entirely. Carrying the old offset over would
// put every viewer's absolute capture→render latency out by the difference
// between two pods' uptimes, with nothing in the numbers to say so.
func (c *TimeSyncClient) Reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.est = TimeSyncEstimator{}
}

// Sample returns the current best estimate, if any pong has landed.
func (c *TimeSyncClient) Sample() (TimeSyncStats, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.est.Best()
}

// clockMappingPublisher decides when a ClockMapping goes out. Pure and
// clock-injectable so the cadence is a unit test rather than a sleep.
//
// The rule it encodes: nothing before the first pong (without a sample there
// is no offset to publish, and publishing a zero would assert that this
// machine's clock IS the relay's), then every ClockMappingInterval.
type clockMappingPublisher struct {
	interval   time.Duration
	lastSentUs uint64
	everSent   bool
}

func newClockMappingPublisher() *clockMappingPublisher {
	return &clockMappingPublisher{interval: ClockMappingInterval}
}

// reset re-arms the "publish as soon as there is a sample" rule. Used on a
// resume: the relay dropped the broadcast's cached ClockMapping when the new
// publisher session claimed the hub, so waiting out the ordinary interval
// would leave every joining viewer without an offset for up to that long.
func (p *clockMappingPublisher) reset() {
	p.everSent = false
	p.lastSentUs = 0
}

// due reports whether to publish now. haveSample is whether a pong has landed.
func (p *clockMappingPublisher) due(nowUs uint64, haveSample bool) bool {
	if !haveSample {
		return false
	}
	if !p.everSent {
		p.everSent = true
		p.lastSentUs = nowUs
		return true
	}
	if nowUs-p.lastSentUs >= uint64(p.interval.Microseconds()) {
		p.lastSentUs = nowUs
		return true
	}
	return false
}
