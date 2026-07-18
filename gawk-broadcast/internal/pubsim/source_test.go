package pubsim

import (
	"bytes"
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-broadcast/internal/engine"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/fixture"
)

const (
	fixtureFrames    = 60
	fixtureGOPFrames = 15
)

func TestDemuxFixture(t *testing.T) {
	aus, err := Demux(fixture.TS)
	if err != nil {
		t.Fatalf("Demux: %v", err)
	}
	if len(aus) != fixtureFrames {
		t.Fatalf("demuxed %d access units, want %d", len(aus), fixtureFrames)
	}
	var keyframes int
	for i, au := range aus {
		if len(au.Data) == 0 {
			t.Fatalf("AU %d is empty", i)
		}
		if au.Keyframe != (i%fixtureGOPFrames == 0) {
			t.Errorf("AU %d keyframe = %v, want %v (GOP %d)", i, au.Keyframe, i%fixtureGOPFrames == 0, fixtureGOPFrames)
		}
		if au.Keyframe {
			keyframes++
		}
	}
	if keyframes != fixtureFrames/fixtureGOPFrames {
		t.Errorf("%d keyframes, want %d", keyframes, fixtureFrames/fixtureGOPFrames)
	}
}

func TestDemuxRejectsGarbage(t *testing.T) {
	if _, err := Demux([]byte("not an mpeg-ts stream")); err == nil {
		t.Fatal("Demux of garbage succeeded; the CLI would publish nothing, silently")
	}
}

// The loop and its timestamps, deterministically: the source must replay the
// AU sequence with wraparound, preserve the keyframe cadence across the wrap,
// and stamp every emitted AU with the clock's value at emit time — never the
// fixture's own two-second timeline, which would wreck the viewer's live-edge
// and ClockMapping math on the second loop.
func TestSourceLoopsWithLiveTimestamps(t *testing.T) {
	aus, err := Demux(fixture.TS)
	if err != nil {
		t.Fatalf("Demux: %v", err)
	}
	clock := &engine.FakeClock{Us: 1_000_000}
	src, err := NewSource(aus, 30, clock)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	tick := make(chan time.Time)
	src.tick = tick

	frames, err := src.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer src.Stop()

	// 2.5 fixture loops. The test owns sequencing: set the clock, tick, then
	// receive — the channel handoffs order the clock accesses, so FakeClock
	// needs no locking here.
	var lastTs uint64
	for i := 0; i < fixtureFrames*2+fixtureFrames/2; i++ {
		clock.Advance(33_333 * time.Microsecond)
		tick <- time.Time{}
		au, ok := <-frames
		if !ok {
			t.Fatalf("frames channel closed at AU %d", i)
		}
		want := aus[i%fixtureFrames]
		if au.TimestampUs != clock.Us {
			t.Fatalf("AU %d timestamp = %d, want the live clock %d", i, au.TimestampUs, clock.Us)
		}
		if au.TimestampUs <= lastTs {
			t.Fatalf("AU %d timestamp %d not monotonic (previous %d)", i, au.TimestampUs, lastTs)
		}
		lastTs = au.TimestampUs
		if au.Keyframe != want.Keyframe {
			t.Fatalf("AU %d keyframe = %v, want %v — cadence broke at the wrap?", i, au.Keyframe, want.Keyframe)
		}
		if !bytes.Equal(au.Data, want.Data) {
			t.Fatalf("AU %d bytes differ from fixture AU %d", i, i%fixtureFrames)
		}
	}
}

func TestSourceStop(t *testing.T) {
	aus, err := Demux(fixture.TS)
	if err != nil {
		t.Fatalf("Demux: %v", err)
	}
	clock := &engine.FakeClock{}
	src, err := NewSource(aus, 30, clock)
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	tick := make(chan time.Time)
	src.tick = tick

	frames, err := src.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	tick <- time.Time{}
	if _, ok := <-frames; !ok {
		t.Fatal("no AU before Stop")
	}
	if err := src.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, ok := <-frames; ok {
		t.Fatal("frames channel still open after Stop")
	}
	// Idempotent.
	if err := src.Stop(); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

// Stop before Start must not hang: the engine's teardown calls media.Stop on
// failure paths where capture may never have started.
func TestSourceStopBeforeStart(t *testing.T) {
	aus, err := Demux(fixture.TS)
	if err != nil {
		t.Fatalf("Demux: %v", err)
	}
	src, err := NewSource(aus, 30, &engine.FakeClock{})
	if err != nil {
		t.Fatalf("NewSource: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = src.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop before Start hung")
	}
}

// The factory must hand the engine-provided clock to the source — that
// sharing is docs/19 Decision 6's invariant, and a pubsim that quietly used
// its own clock would ship wrong ClockMappings that nothing else catches.
func TestFactoryUsesEngineClock(t *testing.T) {
	aus, err := Demux(fixture.TS)
	if err != nil {
		t.Fatalf("Demux: %v", err)
	}
	clock := &engine.FakeClock{Us: 42}
	ms, err := Factory(aus, 30)(engine.MediaConfig{}, clock, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	src, ok := ms.(*Source)
	if !ok {
		t.Fatalf("factory returned %T, want *Source", ms)
	}
	if src.clock != engine.Clock(clock) {
		t.Fatal("factory did not thread the engine's clock into the source")
	}
	if _, err := NewSource(aus, 0, clock); err == nil {
		t.Fatal("want an error for fps <= 0")
	}
	if _, err := NewSource(nil, 30, clock); err == nil {
		t.Fatal("want an error for an empty AU list")
	}
}
