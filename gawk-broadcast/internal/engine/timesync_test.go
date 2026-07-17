package engine

import (
	"math"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-server/wire"
)

// The sample sequences below are lifted verbatim from
// gawk-app/src/transport/time-sync.test.ts. That is the point: the two
// implementations must agree, and the cheapest way to notice they have drifted
// is to feed them identical inputs and pin identical outputs. If a case here
// changes, change it there in the same commit.

func TestTimeSyncEstimatorEmpty(t *testing.T) {
	var e TimeSyncEstimator
	if _, ok := e.Best(); ok {
		t.Error("Best() returned a sample with no samples recorded")
	}
}

func TestTimeSyncEstimatorOneExchange(t *testing.T) {
	// Sent at t0=1_000_000µs, server clock read 500_000µs, received t1=1_020_000µs.
	// Midpoint 1_010_000 → offset = 500_000 − 1_010_000 = −510_000. RTT 20ms.
	var e TimeSyncEstimator
	e.Record(1_000_000, 500_000, 1_020_000)
	s, ok := e.Best()
	if !ok {
		t.Fatal("no sample")
	}
	if s.OffsetUs != -510_000 {
		t.Errorf("offsetUs = %d, want -510000 (time-sync.ts pins the same value)", s.OffsetUs)
	}
	if math.Abs(s.RttMs-20) > 0.001 {
		t.Errorf("rttMs = %v, want 20", s.RttMs)
	}
}

func TestTimeSyncEstimatorPrefersLowestRTT(t *testing.T) {
	var e TimeSyncEstimator
	e.Record(0, 1_000_000, 100_000)       // rtt 100ms, offset 950_000
	e.Record(200_000, 1_205_000, 210_000) // rtt 10ms, offset 1_000_000
	e.Record(400_000, 1_450_000, 480_000) // rtt 80ms, offset 1_010_000
	s, ok := e.Best()
	if !ok {
		t.Fatal("no sample")
	}
	if math.Abs(s.RttMs-10) > 0.001 {
		t.Errorf("rttMs = %v, want 10 — the fastest exchange is the most symmetric one", s.RttMs)
	}
	if s.OffsetUs != 1_000_000 {
		t.Errorf("offsetUs = %d, want 1000000", s.OffsetUs)
	}
}

func TestTimeSyncEstimatorSlidesWindow(t *testing.T) {
	var e TimeSyncEstimator
	e.Record(0, 0, 1_000) // rtt 1ms — the early best
	for i := uint64(1); i <= TimeSyncSampleWindow; i++ {
		t0 := i * 1_000_000
		e.Record(t0, t0+5_000, t0+20_000) // rtt 20ms, offset −5_000
	}
	s, ok := e.Best()
	if !ok {
		t.Fatal("no sample")
	}
	if math.Abs(s.RttMs-20) > 0.001 {
		t.Errorf("rttMs = %v, want 20 — the stale best should have aged out", s.RttMs)
	}
	if s.OffsetUs != -5_000 {
		t.Errorf("offsetUs = %d, want -5000", s.OffsetUs)
	}
}

func TestTimeSyncEstimatorIgnoresBogusEcho(t *testing.T) {
	var e TimeSyncEstimator
	e.Record(2_000_000, 1, 1_000_000) // t1 < t0: impossible, dropped
	if _, ok := e.Best(); ok {
		t.Error("accepted a reply that predates its own request")
	}
}

func TestTimeSyncClientPingAndConsume(t *testing.T) {
	var sent [][]byte
	clock := &FakeClock{Us: 10_000_000}
	c := NewTimeSyncClient(func(b []byte) error {
		sent = append(sent, b)
		return nil
	}, clock)

	c.Ping()
	if len(sent) != 1 {
		t.Fatalf("sent %d pings, want 1", len(sent))
	}
	t0, serverUs, err := wire.ParseTimeSync(sent[0])
	if err != nil {
		t.Fatalf("ping is not a valid TimeSync: %v", err)
	}
	if serverUs != 0 {
		t.Errorf("request serverTimeUs = %d, want 0", serverUs)
	}
	if t0 != 10_000_000 {
		t.Errorf("request clientTimeUs = %d, want the clock's 10000000", t0)
	}

	// Answer like the relay: echo clientTimeUs, fill serverTimeUs. 5ms later.
	clock.Advance(5 * time.Millisecond)
	if !c.HandleDatagram(wire.AppendTimeSync(nil, t0, 42_000_000)) {
		t.Error("HandleDatagram returned false for a TimeSync reply; it must consume it")
	}
	s, ok := c.Sample()
	if !ok {
		t.Fatal("no sample after a reply")
	}
	if math.Abs(s.RttMs-5) > 0.001 {
		t.Errorf("rttMs = %v, want 5", s.RttMs)
	}
}

// A TimeSync reply must never reach the video path, and a non-TimeSync
// datagram must never be swallowed by the ping loop.
func TestTimeSyncClientRoutesOnlyItsOwn(t *testing.T) {
	c := NewTimeSyncClient(func([]byte) error { return nil }, &FakeClock{})

	video, err := wire.AppendVideoChunk(nil, wire.VideoChunkHeader{ChunkCount: 1}, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if c.HandleDatagram(video) {
		t.Error("consumed a VideoChunk datagram")
	}
	if !c.HandleDatagram(wire.AppendTimeSync(nil, 1, 2)) {
		t.Error("did not consume a TimeSync datagram")
	}
	// Malformed but type-tagged: consumed and dropped (strict parsing, R2).
	if !c.HandleDatagram([]byte{wire.Version, wire.TypeTimeSync, 0x00}) {
		t.Error("did not consume a malformed TimeSync datagram")
	}
	if _, ok := c.Sample(); ok {
		t.Error("a malformed TimeSync produced a sample")
	}
}

// Publishing a mapping before the first pong would assert that this machine's
// clock IS the relay's — a plausible-looking lie. Nothing goes out until
// there is a real offset to publish.
func TestClockMappingWaitsForFirstPong(t *testing.T) {
	p := newClockMappingPublisher()
	clock := &FakeClock{}

	for i := 0; i < 10; i++ {
		if p.due(clock.NowUs(), false) {
			t.Fatal("published a ClockMapping before any pong landed")
		}
		clock.Advance(time.Second)
	}
	if !p.due(clock.NowUs(), true) {
		t.Fatal("did not publish once a sample existed")
	}
}

func TestClockMappingCadence(t *testing.T) {
	p := newClockMappingPublisher()
	clock := &FakeClock{}

	if !p.due(clock.NowUs(), true) {
		t.Fatal("first mapping did not go out promptly after the first pong")
	}
	// Nothing for the next interval...
	for i := 0; i < 4; i++ {
		clock.Advance(time.Second)
		if p.due(clock.NowUs(), true) {
			t.Fatalf("republished after only %ds, want every %v", i+1, ClockMappingInterval)
		}
	}
	// ...then exactly on it.
	clock.Advance(time.Second)
	if !p.due(clock.NowUs(), true) {
		t.Fatalf("no republish after %v", ClockMappingInterval)
	}
	clock.Advance(ClockMappingInterval)
	if !p.due(clock.NowUs(), true) {
		t.Error("no republish on the following interval")
	}
}

// The ping cadence and the relay's reply rate cap are a matched pair: the
// relay drops TimeSync replies past 5/s per session (a constant in
// transport/server.go). Pinging faster than that would silently lose
// measurements, which is why this is a constant here too, not a knob.
func TestPingIntervalIsWithinRelayRateCap(t *testing.T) {
	const relayReplyRatePerSec = 5.0
	if rate := 1.0 / TimeSyncInterval.Seconds(); rate > relayReplyRatePerSec {
		t.Errorf("ping rate %.1f/s exceeds the relay's %.1f/s reply cap", rate, relayReplyRatePerSec)
	}
	if TimeSyncInterval != 2*time.Second {
		t.Errorf("TimeSyncInterval = %v, want 2s to match TIME_SYNC_INTERVAL_MS", TimeSyncInterval)
	}
	if ClockMappingInterval != 5*time.Second {
		t.Errorf("ClockMappingInterval = %v, want 5s to match CLOCK_MAPPING_INTERVAL_MS", ClockMappingInterval)
	}
	if TimeSyncSampleWindow != 8 {
		t.Errorf("TimeSyncSampleWindow = %d, want 8 to match TIME_SYNC_SAMPLE_WINDOW", TimeSyncSampleWindow)
	}
}
