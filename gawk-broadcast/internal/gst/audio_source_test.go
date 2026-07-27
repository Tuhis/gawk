//go:build linux

// Linux-only for the same reason source_test.go is: these supervise real child
// processes against load-bearing probe-window timings. See that file's header.
package gst

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-broadcast/internal/engine"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/portal"
)

// na1Fixture is the real muxed Opus + H.264 capture from the NA1 spike. It
// lives in internal/mpegts/testdata because that is the package whose parser
// it is ground truth for; here it stands in for a pipeline that actually
// carries audio.
func na1Fixture(t *testing.T) string {
	t.Helper()
	return mustAbs(t, "../mpegts/testdata/opus-h264-na1.ts")
}

// newAudioSource builds a Source with an explicit media config, so a test can
// turn audio off — newSource() always uses the defaults.
func newAudioSource(t *testing.T, cfg engine.MediaConfig, opts Options) *Source {
	t.Helper()
	if opts.LiveProbeWindow == 0 {
		opts.LiveProbeWindow = 150 * time.Millisecond
	}
	if opts.Trial == nil {
		opts.Trial = newTrialer(CandidateNames()...).fn()
	}
	if opts.AudioTrial == nil {
		opts.AudioTrial = newAudioTrialer(audioNames()...).fn()
	}
	src, err := NewFactory(opts)(cfg, &engine.FakeClock{Us: 1000}, testLog)
	if err != nil {
		t.Fatal(err)
	}
	return src.(*Source)
}

// Decision 2's ordering rule: an audio trial opens no picker, needs no
// permission and touches no GPU, so it runs *before* the portal handshake —
// the same instinct as EnsureBinary. A broadcaster learns "no audio on this
// machine" before they are asked to pick a window, not after.
func TestAudioTrialRunsBeforeThePicker(t *testing.T) {
	var mu sync.Mutex
	var order []string
	record := func(what string) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, what)
	}

	fp := &fakePortal{}
	s := newSource(t, Options{
		Binary: streamingBinary(t),
		OpenPortal: func(ctx context.Context, opts portal.Options) (*portal.Stream, error) {
			record("portal")
			return fp.open(ctx, opts)
		},
		AudioTrial: func(ctx context.Context, c AudioCandidate, device string) error {
			record("audio-trial")
			return nil
		},
	})

	if _, err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	mu.Lock()
	defer mu.Unlock()
	if len(order) < 2 {
		t.Fatalf("recorded %v, want an audio trial and a portal handshake", order)
	}
	if order[0] != "audio-trial" || order[1] != "portal" {
		t.Errorf("order = %v, want the audio trial before the picker", order)
	}
}

// Audio off means off: no probe, no audio elements, and — the guarantee that
// matters — a pipeline byte-identical to the one this app shipped before R25.
func TestDisabledAudioNeverProbes(t *testing.T) {
	cfg := engine.DefaultMediaConfig()
	cfg.DisableAudio = true

	tr := newAudioTrialer(audioNames()...)
	fp := &fakePortal{}
	s := newAudioSource(t, cfg, Options{
		Binary:     streamingBinary(t),
		OpenPortal: fp.open,
		AudioTrial: tr.fn(),
	})

	if _, err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	if len(tr.tried) != 0 {
		t.Errorf("probed %v with audio disabled", tr.tried)
	}
	if got := s.AudioState(); got != engine.AudioOff {
		t.Errorf("AudioState = %q, want %q", got, engine.AudioOff)
	}
	if _, ok := s.AudioFormat(); ok {
		t.Error("AudioFormat reports a format with audio disabled")
	}
	if s.Audio() != nil {
		t.Error("a disabled lane still handed out an audio channel")
	}
}

// No usable audio source is not a failure — it is "publish video and say so"
// (Decision 6). The broadcast starts, frames flow, and the state says why
// there is no sound.
func TestNoAudioSourceStillBroadcastsVideo(t *testing.T) {
	fp := &fakePortal{}
	s := newSource(t, Options{
		Binary:     streamingBinary(t),
		OpenPortal: fp.open,
		AudioTrial: newAudioTrialer().fn(), // nothing captures on this machine
	})

	frames, err := s.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v — a machine with no audio source must still broadcast", err)
	}
	defer s.Stop()
	if _, ok := <-frames; !ok {
		t.Fatalf("no frames: %v", s.Err())
	}
	if got := s.AudioState(); got != engine.AudioUnavailable {
		t.Errorf("AudioState = %q, want %q", got, engine.AudioUnavailable)
	}
	if _, ok := s.AudioFormat(); ok {
		t.Error("AudioFormat reports a format when no source passed the trial")
	}
}

// Decision 6/7: a live pipeline that dies naming an audio element must retry
// **the same rung** without audio. Advancing the cascade there would burn
// encoders over a sound card, and could land on "no working hardware encoder"
// with a working encoder in hand.
func TestAudioElementDeathRetriesTheSameRung(t *testing.T) {
	fp := &fakePortal{}
	bin := fakeBinary(t, `case "$*" in
*opusenc*) echo 'ERROR: from element /GstPipeline:pipeline0/GstOpusEnc:opusenc0: Encoding failed' >&2
exit 1 ;;
*) cat `+na1Fixture(t)+`
sleep 30 ;;
esac
`)
	s := newSource(t, Options{
		Binary:     bin,
		OpenPortal: fp.open,
		AudioTrial: newAudioTrialer(audioNames()...).fn(),
	})

	frames, err := s.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v — an audio element failure must cost audio, not the broadcast", err)
	}
	defer s.Stop()
	if _, ok := <-frames; !ok {
		t.Fatalf("no frames after dropping audio: %v", s.Err())
	}

	// The same rung: the first encoder and the first capture mode, neither
	// advanced by a failure that was never theirs.
	if got := s.Encoder(); got != Cascade[0].Name {
		t.Errorf("Encoder = %q, want %q — the cascade advanced over an audio failure", got, Cascade[0].Name)
	}
	if got := s.CapturePath(); got != "zero-copy (capped)" {
		t.Errorf("CapturePath = %q, want the leading rung — the capture ladder advanced over an audio failure", got)
	}
	if got := s.AudioState(); got != engine.AudioUnavailable {
		t.Errorf("AudioState = %q, want %q", got, engine.AudioUnavailable)
	}
	if n := fp.openCount(); n != 1 {
		t.Errorf("portal opened %d times, want 1 — the audio retry must reuse the grant", n)
	}
}

// The outer half of Decision 6: when every rung fails with audio on and
// nothing implicates an audio element, the cascade re-runs once without it. A
// machine that cannot encode video must not be told the problem is its sound
// card — so the diagnosis a user finally sees comes from a pass with no audio
// in it at all.
func TestEveryRungFailingWithAudioRetriesWithoutIt(t *testing.T) {
	fp := &fakePortal{}
	// Dies whenever the muxer is named (i.e. whenever audio is present), and
	// blames the *encoder* — so per-rung attribution deliberately does not
	// fire and only the outer re-run can save this start.
	bin := fakeBinary(t, `case "$*" in
*name=mux*) echo 'ERROR: from element /GstPipeline:pipeline0/GstVaH264Enc:vah264enc0: device busy' >&2
exit 1 ;;
*) cat `+na1Fixture(t)+`
sleep 30 ;;
esac
`)
	s := newSource(t, Options{
		Binary:     bin,
		OpenPortal: fp.open,
		AudioTrial: newAudioTrialer(audioNames()...).fn(),
	})

	frames, err := s.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v — the audio-off re-run must rescue this start", err)
	}
	defer s.Stop()
	if _, ok := <-frames; !ok {
		t.Fatalf("no frames from the audio-off pass: %v", s.Err())
	}
	if got := s.AudioState(); got != engine.AudioUnavailable {
		t.Errorf("AudioState = %q, want %q", got, engine.AudioUnavailable)
	}
	if n := fp.openCount(); n != 1 {
		t.Errorf("portal opened %d times, want 1 — the audio-off re-run must reuse the grant", n)
	}
}

// End to end through the real demuxer: muxed Opus in, engine packets out, on
// the engine clock and with the control header stripped.
func TestAudioPacketsReachTheEngineSeam(t *testing.T) {
	fp := &fakePortal{}
	s := newSource(t, Options{
		Binary:     fakeBinary(t, "cat "+na1Fixture(t)+"\nsleep 30\n"),
		OpenPortal: fp.open,
		AudioTrial: newAudioTrialer(audioNames()...).fn(),
	})

	if _, err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	format, ok := s.AudioFormat()
	if !ok {
		t.Fatal("AudioFormat reports nothing after a successful audio start")
	}
	// The format is the one the caps filter forces, not one read back from a
	// device: the pipeline decides, and the viewer is told exactly that.
	if format.Codec != engine.AudioCodec || format.SampleRate != engine.AudioSampleRate || format.Channels != engine.AudioChannels {
		t.Errorf("AudioFormat = %+v, want 48 kHz stereo opus", format)
	}
	if format.Source != audioCascade[0].Name {
		t.Errorf("AudioFormat.Source = %q, want the winning candidate %q", format.Source, audioCascade[0].Name)
	}

	packets := s.Audio()
	if packets == nil {
		t.Fatal("no audio channel after a successful audio start")
	}
	deadline := time.After(5 * time.Second)
	var got []engine.AudioPacket
	for len(got) < 4 {
		select {
		case p, ok := <-packets:
			if !ok {
				t.Fatalf("the audio channel closed after %d packets: %v", len(got), s.Err())
			}
			got = append(got, p)
		case <-deadline:
			t.Fatalf("timed out with %d audio packets", len(got))
		}
	}
	for i, p := range got {
		if len(p.Data) == 0 {
			t.Errorf("packet %d is empty", i)
		}
		// The control header must be stripped: what reaches the engine is the
		// Opus packet, starting at its TOC.
		if p.Data[0] == 0x7f {
			t.Errorf("packet %d still carries its MPEG-TS control header", i)
		}
		if p.TimestampUs == 0 {
			t.Errorf("packet %d was not stamped on the engine clock", i)
		}
	}
}
