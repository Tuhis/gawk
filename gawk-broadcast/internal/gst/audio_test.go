package gst

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/Tuhis/gawk/gawk-broadcast/internal/engine"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/mpegts"
)

// testLog lives here rather than in source_test.go because that file is
// Linux-only (it supervises real subprocesses) while these tests are pure.
var testLog = slog.New(slog.DiscardHandler)

// audioTrialer records which candidates were probed and answers per name.
type audioTrialer struct {
	tried []string
	ok    map[string]bool
}

func newAudioTrialer(working ...string) *audioTrialer {
	t := &audioTrialer{ok: map[string]bool{}}
	for _, w := range working {
		t.ok[w] = true
	}
	return t
}

func (tr *audioTrialer) fn() AudioTrialFunc {
	return func(ctx context.Context, c AudioCandidate, device string) error {
		tr.tried = append(tr.tried, c.Name)
		if tr.ok[c.Name] {
			return nil
		}
		return errors.New("element listed but produces no audio on this device")
	}
}

func audioNames() []string {
	names := make([]string, 0, len(audioCascade))
	for _, c := range audioCascade {
		names = append(names, c.Name)
	}
	return names
}

// Decision 2's ranking is the whole reason there is a cascade rather than one
// element: pipewiresrc leads because WirePlumber *follows* the default sink, so
// a headphone/speaker switch re-routes the stream instead of erroring it — the
// property NA1 run 2 measured directly (finding 9) and the one that narrows
// Decision 6's mid-session hole.
func TestPipeWireLeadsTheAudioCascade(t *testing.T) {
	if got := audioNames(); !slices.Equal(got, []string{"pipewire-monitor", "pulse-default-monitor"}) {
		t.Errorf("audio cascade = %v, want pipewire-monitor then pulse-default-monitor", got)
	}
	lead := pipelineString(BuildAudioTrialPipeline(audioCascade[0], ""))
	if !strings.Contains(lead, "stream.capture.sink=true") {
		t.Errorf("the lead candidate does not ask for the default sink's monitor:\n%s", lead)
	}
	if !strings.Contains(lead, "pipewiresrc") {
		t.Errorf("the lead candidate is not pipewiresrc:\n%s", lead)
	}
}

func TestAudioCascadePicksTheFirstThatActuallyCaptures(t *testing.T) {
	tr := newAudioTrialer("pulse-default-monitor")
	got, err := SelectAudioSource(context.Background(), "", "", tr.fn())
	if err != nil {
		t.Fatalf("SelectAudioSource: %v", err)
	}
	if got.Name != "pulse-default-monitor" {
		t.Errorf("chose %q, want pulse-default-monitor", got.Name)
	}
	if !slices.Equal(tr.tried, audioNames()) {
		t.Errorf("probed %v, want the cascade in order %v", tr.tried, audioNames())
	}
}

// No audio source is not an error a user has to act on: it is "publish video
// and say so" (Decision 6). The sentinel is what lets the caller tell that
// apart from a failure.
func TestNoAudioSourceIsASentinelNotAFailure(t *testing.T) {
	tr := newAudioTrialer()
	_, err := SelectAudioSource(context.Background(), "", "", tr.fn())
	if !errors.Is(err, ErrNoAudioSource) {
		t.Fatalf("error = %v, want ErrNoAudioSource", err)
	}
	if !slices.Equal(tr.tried, audioNames()) {
		t.Errorf("probed %v, want every candidate tried before giving up", tr.tried)
	}
	// It names what failed, so a log line is actionable.
	for _, name := range audioNames() {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error does not say %q was tried: %v", name, err)
		}
	}
}

// An explicit device pins exactly one candidate — the same rule as the encoder
// override, for the same reason: capturing something other than what the user
// named would be worse than capturing nothing.
func TestExplicitDevicePinsExactlyOneCandidate(t *testing.T) {
	tr := newAudioTrialer("pulse-device")
	got, err := SelectAudioSource(context.Background(), "alsa_output.pci-0000_00_1f.3.analog-stereo.monitor", "", tr.fn())
	if err != nil {
		t.Fatalf("SelectAudioSource with a device: %v", err)
	}
	if got.Name != "pulse-device" {
		t.Errorf("chose %q, want pulse-device", got.Name)
	}
	if !slices.Equal(tr.tried, []string{"pulse-device"}) {
		t.Errorf("probed %v, want only the named device", tr.tried)
	}
	// The device reaches the element rather than being quietly dropped.
	p := pipelineString(BuildAudioTrialPipeline(got, "my-sink.monitor"))
	if !strings.Contains(p, "pulsesrc device=my-sink.monitor") {
		t.Errorf("the named device does not reach pulsesrc:\n%s", p)
	}
}

// Unlike the encoder override, an explicit device is still trialled: audio is
// subordinate, so a device name that does not resolve must degrade to
// video-only rather than take the broadcast down.
func TestExplicitDeviceIsStillTrialled(t *testing.T) {
	tr := newAudioTrialer() // the named device does not resolve
	_, err := SelectAudioSource(context.Background(), "nonexistent.monitor", "", tr.fn())
	if !errors.Is(err, ErrNoAudioSource) {
		t.Fatalf("error = %v, want ErrNoAudioSource for an unusable device", err)
	}
	if !slices.Equal(tr.tried, []string{"pulse-device"}) {
		t.Errorf("probed %v, want the named device to have been trialled", tr.tried)
	}
}

// A cached answer saves a probe per start, and is re-verified rather than
// trusted — the same rule as LastGoodEncoder.
func TestLastGoodAudioSourceIsReverifiedFirst(t *testing.T) {
	tr := newAudioTrialer("pulse-default-monitor")
	got, err := SelectAudioSource(context.Background(), "", "pulse-default-monitor", tr.fn())
	if err != nil {
		t.Fatalf("SelectAudioSource: %v", err)
	}
	if got.Name != "pulse-default-monitor" {
		t.Errorf("chose %q, want the cached pulse-default-monitor", got.Name)
	}
	if !slices.Equal(tr.tried, []string{"pulse-default-monitor"}) {
		t.Errorf("probed %v, want only the cached source", tr.tried)
	}
}

func TestStaleLastGoodAudioSourceFallsBackToTheCascade(t *testing.T) {
	tr := newAudioTrialer("pipewire-monitor")
	got, err := SelectAudioSource(context.Background(), "", "pulse-default-monitor", tr.fn())
	if err != nil {
		t.Fatalf("SelectAudioSource: %v", err)
	}
	if got.Name != "pipewire-monitor" {
		t.Errorf("chose %q, want pipewire-monitor after the cache went stale", got.Name)
	}
	// The cached source, then the cascade from its head — which stops at the
	// first that works, so the stale one is not re-probed a third time.
	if !slices.Equal(tr.tried, []string{"pulse-default-monitor", "pipewire-monitor"}) {
		t.Errorf("probed %v, want the cached source then the cascade", tr.tried)
	}
}

func TestUnknownLastGoodAudioSourceIsIgnored(t *testing.T) {
	tr := newAudioTrialer("pipewire-monitor")
	got, err := SelectAudioSource(context.Background(), "", "some-source-we-removed", tr.fn())
	if err != nil {
		t.Fatalf("SelectAudioSource: %v", err)
	}
	if got.Name != "pipewire-monitor" {
		t.Errorf("chose %q, want pipewire-monitor", got.Name)
	}
}

// The trial has to terminate on its own, or the pre-flight hangs startup in
// front of the share picker — the exact place a hang is least forgivable.
func TestAudioTrialTerminatesAndEncodes(t *testing.T) {
	for _, c := range append(audioCascade, audioDeviceCandidate) {
		p := pipelineString(BuildAudioTrialPipeline(c, "dev.monitor"))
		if !strings.Contains(p, "num-buffers=") {
			t.Errorf("%s: the trial is unbounded and would never exit:\n%s", c.Name, p)
		}
		if !strings.Contains(p, "opusenc") {
			t.Errorf("%s: the trial does not exercise the encoder:\n%s", c.Name, p)
		}
		// A trial must not touch the portal: it runs *before* the handshake,
		// so there is not even a grant to use.
		if strings.Contains(p, "fd=3") || strings.Contains(p, "path=") {
			t.Errorf("%s: the audio trial reaches for the portal grant:\n%s", c.Name, p)
		}
	}
}

// Decision 3's properties are acceptance criteria, not incidental args: each
// one is load-bearing for something a viewer would feel.
func TestAudioBranchPinsTheDecision3Properties(t *testing.T) {
	cand := audioCascade[0]
	p := pipelineString(BuildPipeline(Cascade[0], engine.DefaultMediaConfig(), wholeScreen(engine.DefaultMediaConfig(), 42), CaptureAuto, &cand))

	for _, tc := range []struct{ want, why string }{
		{"rate=48000", "a resampled 48 kHz stream is what the AudioConfig advertises"},
		{"channels=2", "a 5.1 monitor would otherwise give multistream Opus, which WebCodecs cannot decode with an empty description"},
		{"dtx=false", "DTX would make silence look like packet loss to the viewer's gap detection"},
		{"frame-size=20", "20 ms ≈ 320 B is what makes one packet per datagram true"},
		{"inband-fec=false", "the viewer has no FEC hook, so redundancy spends bitrate nothing can read"},
		{"audio-type=restricted-lowdelay", "drops libopus's ~6.5 ms lookahead"},
		{"bitrate=128000", "the browser lane's AUDIO_BITRATE_BPS"},
		{"audioconvert", "downmix is what keeps the advertised channel count honest"},
		{"audioresample", "a 44.1 kHz monitor has to reach 48 kHz"},
	} {
		if !strings.Contains(p, tc.want) {
			t.Errorf("audio branch is missing %q (%s):\n%s", tc.want, tc.why, p)
		}
	}

	// A queue on both sides of the encoder, so audio scheduling jitter never
	// reaches the video path.
	if !strings.Contains(p, "queue ! audioconvert") {
		t.Errorf("no queue between the source and the encoder:\n%s", p)
	}
	if !strings.Contains(p, "! queue ! mux.") {
		t.Errorf("no queue between the encoder and the muxer:\n%s", p)
	}
}

// One child, one pipe, one muxer — and therefore one PTS timeline, which is
// the entire A/V sync design (Decision 5). A second pipe would be less code
// and would reintroduce a constant lip-sync bias nothing can measure.
func TestAudioIsMuxedIntoTheExistingStream(t *testing.T) {
	cand := audioCascade[0]
	args := BuildPipeline(Cascade[0], engine.DefaultMediaConfig(), wholeScreen(engine.DefaultMediaConfig(), 42), CaptureAuto, &cand)
	p := pipelineString(args)

	if !strings.Contains(p, "mpegtsmux name=mux") {
		t.Errorf("the muxer is not named, so the audio chain has nothing to link into:\n%s", p)
	}
	if strings.Count(p, "mpegtsmux") != 1 {
		t.Errorf("more than one muxer — audio must share the video's timeline:\n%s", p)
	}
	if strings.Count(p, "fdsink") != 1 {
		t.Errorf("more than one sink — one child, one pipe:\n%s", p)
	}
	if !strings.HasSuffix(p, "! mux.") {
		t.Errorf("the audio chain does not end by linking into the muxer:\n%s", p)
	}
	// gst-launch's syntax is positional; a stray or missing "!" changes the
	// graph rather than failing loudly.
	for i, a := range args {
		if a != "!" {
			continue
		}
		if i == 0 || i == len(args)-1 {
			t.Errorf("a link at position %d has nothing to link:\n%s", i, p)
		}
		if i+1 < len(args) && args[i+1] == "!" {
			t.Errorf("two consecutive links at %d:\n%s", i, p)
		}
	}
}

// The byte-identical guarantee, asserted rather than assumed: with audio off,
// every candidate and every capture mode must produce exactly the args this
// function produced before R25 — `name=mux` included, since a named muxer is
// still a difference on the command line.
func TestAudioOffIsByteIdenticalToTheVideoOnlyPipeline(t *testing.T) {
	cfg := engine.DefaultMediaConfig()
	for _, c := range Cascade {
		for _, mode := range CaptureModes {
			args := BuildPipeline(c, cfg, wholeScreen(cfg, 42), mode, nil)
			p := pipelineString(args)
			if !strings.HasSuffix(p, "! mpegtsmux ! fdsink fd=1") {
				t.Errorf("%s/%s: the video-only tail changed:\n%s", c.Element, mode, p)
			}
			for _, forbidden := range []string{"name=mux", "opusenc", "audioconvert", "audioresample", "pulsesrc", "audio/x-raw"} {
				if strings.Contains(p, forbidden) {
					t.Errorf("%s/%s: audio-off pipeline contains %q:\n%s", c.Element, mode, forbidden, p)
				}
			}
		}
	}
}

// Decision 5, pinned: audio and video are stamped through **one** ptsAnchor,
// so the relative A/V skew the mapping introduces is exactly zero.
//
// The test is built around the bias a split anchor would create. Each medium
// is delivered with its own pipeline latency — video 5 ms behind its PTS
// (encode, mux, ~64 kB of pipe), audio 1 ms — because that asymmetry is real:
// audio pays no GPU encode and no pipe backlog. One anchor keeps the single
// smallest offset it has seen and applies it to both, so two samples with
// equal PTS get equal stamps. Two anchors would each keep their own minimum,
// and audio would ship a constant 4 ms ahead of the picture: inaudible in a
// test, a lip-sync error in the field, and invisible to every instrument the
// viewer has.
func TestOneAnchorStampsBothMedia(t *testing.T) {
	clock := &engine.FakeClock{}
	s := &Source{
		cfg:   engine.DefaultMediaConfig(),
		clock: clock,
		log:   testLog,
		// Deep enough that the drop policies never fire and every sample is
		// observable.
		frames: make(chan engine.AccessUnit, 64),
		audio:  make(chan engine.AudioPacket, 64),
	}
	h := &pumpHandle{done: make(chan struct{})}

	// One 90 kHz timeline, as mpegtsmux emits it: video every 3000 ticks
	// (33.3 ms), audio every 1800 (20 ms), interleaved in timestamp order.
	// Ticks rather than microseconds on purpose — µs are derived with the
	// anchor's own integer arithmetic, so the test measures the mapping and
	// not a rounding difference of its own making.
	const (
		videoLatencyUs = 5000
		audioLatencyUs = 1000
		baseTicks      = 81_000_000 // 15 min in, where the 33-bit field is busy
	)
	type sample struct {
		ticks uint64
		video bool
	}
	var samples []sample
	for i := range 6 {
		samples = append(samples, sample{ticks: baseTicks + uint64(i)*3000, video: true})
	}
	for i := range 10 {
		samples = append(samples, sample{ticks: baseTicks + uint64(i)*1800})
	}
	slices.SortStableFunc(samples, func(a, b sample) int { return int(a.ticks) - int(b.ticks) })

	stamps := map[uint64]uint64{} // ticks → engine-clock stamp
	for _, sm := range samples {
		ptsUs := sm.ticks * 1000 / 90
		if sm.video {
			clock.Us = ptsUs + videoLatencyUs
			s.emitAU(h, mpegts.AU{Data: []byte{0, 0, 0, 1, 0x65}, PTS: sm.ticks, HasPTS: true})
			stamps[sm.ticks] = (<-s.frames).TimestampUs
			continue
		}
		clock.Us = ptsUs + audioLatencyUs
		s.emitAudio(h, mpegts.AudioPacket{Data: []byte{0xfc, 0x01}, PTS: sm.ticks, HasPTS: true})
		stamps[sm.ticks] = (<-s.audio).TimestampUs
	}

	// Both media land on one affine map: stamp − pts is the same constant for
	// every sample, whichever medium produced it.
	var offset int64
	first := true
	for _, sm := range samples {
		got := int64(stamps[sm.ticks]) - int64(sm.ticks*1000/90)
		if first {
			offset, first = got, false
			continue
		}
		if got != offset {
			t.Fatalf("pts %d ticks (video=%v) mapped with offset %d, but an earlier sample used %d — "+
				"audio and video are not sharing one anchor", sm.ticks, sm.video, got, offset)
		}
	}
	// And it is the *smaller* of the two latencies, i.e. audio's: a shared
	// minimum is dominated by whichever medium arrives earlier, which is the
	// documented consequence of one anchor rather than a bug to correct.
	if offset != audioLatencyUs {
		t.Errorf("shared offset = %d µs, want %d — the anchor is not taking the minimum across both media",
			offset, audioLatencyUs)
	}
}

// The queue between the demuxer and the sender evicts the **oldest** on
// overflow — deliberately the opposite of the video path's drop-newest
// (Decision 9, following docs/24 finding 14). Audio has no GOP to poison, so
// shedding the backlog keeps the listener near live where shedding the
// newcomer would strand them as far behind as the queue is deep.
func TestAudioQueueDropsTheOldest(t *testing.T) {
	s := &Source{
		cfg:   engine.DefaultMediaConfig(),
		clock: &engine.FakeClock{},
		log:   testLog,
		audio: make(chan engine.AudioPacket, 2),
	}
	for i := range 5 {
		s.offerAudio(engine.AudioPacket{TimestampUs: uint64(i)})
	}

	var got []uint64
	for len(s.audio) > 0 {
		got = append(got, (<-s.audio).TimestampUs)
	}
	// The queue holds the two most recent packets, not the two oldest.
	if !slices.Equal(got, []uint64{3, 4}) {
		t.Errorf("queue holds %v, want the newest [3 4] — a full audio queue must trend toward live", got)
	}
}

// Attribution decides whether a live failure costs the sound card or the whole
// broadcast (Decision 7). It matches the element names the builder itself
// chose — never free text — and deliberately excludes pipewiresrc, because the
// *video* capture is a pipewiresrc too and blaming audio for a compositor
// negotiation failure is the misdiagnosis ErrCaptureFormat exists to prevent.
func TestAudioAttributionMatchesOnlyAudioElements(t *testing.T) {
	pw, pulse := audioCascade[0], audioCascade[1]

	for _, tc := range []struct {
		name    string
		failure string
		cand    AudioCandidate
		want    bool
	}{
		{"opusenc death", "ERROR: from element /GstPipeline:pipeline0/GstOpusEnc:opusenc0: Encoding failed", pw, true},
		{"pulsesrc death", "ERROR: from element /GstPipeline:pipeline0/GstPulseSrc:pulsesrc0: Failed to connect", pulse, true},
		{"audioconvert death", "ERROR: from element GstAudioConvert:audioconvert0: not negotiated", pw, true},
		// The 2026-07-16 field failure, verbatim. Spelled out here rather than
		// shared with source_test.go's copy, which is Linux-only.
		{"pipewiresrc death",
			"ERROR: from element /GstPipeline:pipeline0/GstPipeWireSrc:pipewiresrc0: stream error: unhandled format", pw, false},
		{"encoder death", "ERROR: from element /GstPipeline:pipeline0/GstVaH264Enc:vah264enc0: cannot open device", pw, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := failureNamesAudioElement(tc.failure, tc.cand); got != tc.want {
				t.Errorf("failureNamesAudioElement = %v, want %v for:\n%s", got, tc.want, tc.failure)
			}
		})
	}
}
