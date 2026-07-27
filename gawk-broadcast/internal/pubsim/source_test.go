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
	ms, err := Factory(aus, 30, nil)(engine.MediaConfig{}, clock, slog.New(slog.DiscardHandler))
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

// With packets supplied the source grows an audio lane; without them it stays
// the *video-only type*. That distinction is the point of docs/28 Decision 8's
// optional interface: R20 tier-1 asserts the no-audio path continuously, and
// that assertion is only meaningful while video-only is a real shape rather
// than an audio source returning nothing.
func TestAudioLaneIsOptIn(t *testing.T) {
	aus, err := Demux(fixture.TS)
	if err != nil {
		t.Fatalf("Demux: %v", err)
	}
	clock := &engine.FakeClock{Us: 42}
	log := slog.New(slog.DiscardHandler)

	videoOnly, err := Factory(aus, 30, nil)(engine.MediaConfig{}, clock, log)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	if _, ok := videoOnly.(engine.AudioSource); ok {
		t.Error("a source built with no packets still satisfies engine.AudioSource")
	}

	packets, err := fixture.SplitAudio(fixture.Audio)
	if err != nil {
		t.Fatalf("SplitAudio: %v", err)
	}
	withAudio, err := Factory(aus, 30, packets)(engine.MediaConfig{}, clock, log)
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	as, ok := withAudio.(engine.AudioSource)
	if !ok {
		t.Fatal("a source built with packets does not satisfy engine.AudioSource")
	}
	format, ok := as.AudioFormat()
	if !ok {
		t.Fatal("AudioFormat reports nothing")
	}
	if format.Codec != engine.AudioCodec || format.SampleRate != engine.AudioSampleRate || format.Channels != engine.AudioChannels {
		t.Errorf("AudioFormat = %+v, want 48 kHz stereo opus", format)
	}
}

// The audio lane reads the engine's clock, like the video lane does — pubsim
// must not weaken docs/28 Decision 5's one-timeline invariant just because its
// packets are canned.
func TestAudioPacketsAreStampedFromTheEngineClock(t *testing.T) {
	aus, err := Demux(fixture.TS)
	if err != nil {
		t.Fatalf("Demux: %v", err)
	}
	packets, err := fixture.SplitAudio(fixture.Audio)
	if err != nil {
		t.Fatalf("SplitAudio: %v", err)
	}
	clock := &engine.FakeClock{Us: 7_000_000}
	ms, err := Factory(aus, 30, packets)(engine.MediaConfig{}, clock, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	src := ms.(engine.AudioSource)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if _, err := ms.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer ms.Stop()

	select {
	case p := <-src.Audio():
		if p.TimestampUs != clock.Us {
			t.Errorf("TimestampUs = %d, want the engine clock's %d", p.TimestampUs, clock.Us)
		}
		if !bytes.Equal(p.Data, packets[0]) {
			t.Error("the first packet is not the fixture's first packet")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no audio packet")
	}
}
