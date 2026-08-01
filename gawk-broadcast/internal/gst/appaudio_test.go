package gst

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-broadcast/internal/appaudio"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/engine"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/portal"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/pwproto"
)

// fakeHelper is a shell script that speaks the helper protocol.
//
// The real helper has its own integration suite against a real daemon
// (cmd/gawk-pw-helper); what these tests are about is the *engine's* behavior
// around it — every branch of docs/39 D6 — and a script makes each of those
// branches reachable in milliseconds and without a sound server.
func fakeHelper(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), appaudio.HelperBinary)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// workingHelper answers a capture request with a sink and two links.
func workingHelper(t *testing.T, serial string) string {
	return fakeHelper(t, `
echo '{"event":"ready","version":"fake-1.0"}'
echo '{"event":"apps","apps":[{"binary":"fake-game","name":"Fake Game","streams":1}]}'
while IFS= read -r line; do
  case "$line" in
    *'"capture"'*|*'"capture-system"'*)
      echo '{"event":"sink","serial":`+serial+`,"nodeId":42,"channels":2}'
      echo '{"event":"links","binary":"fake-game","links":2}'
      ;;
  esac
done
`)
}

// deviceMediaConfig is the rung with an explicit audio device named.
func deviceMediaConfig(device string) engine.MediaConfig {
	cfg := engine.DefaultMediaConfig()
	cfg.AudioDevice = device
	return cfg
}

// windowPortal is a portal that returns a window of the given size.
func windowPortal(w, h int) *fakePortal {
	return &fakePortal{sourceType: portal.SourceWindow, width: w, height: h}
}

// The app-mode audio branch differs from the system one in **exactly one
// element**: the source. Everything downstream — the queue, the converters, the
// caps, every opusenc property — is the R25 branch byte for byte, which is what
// makes the viewer's decoder configuration and the wire contract identical
// whichever mode a broadcast is in.
func TestAppAudioChangesOnlyTheSourceElement(t *testing.T) {
	cfg := engine.DefaultMediaConfig()
	c, _ := FindCandidate("vah264enc")
	sys, _ := FindAudioCandidate("pipewire-monitor")
	app := AppAudioCandidate(4242)

	sysArgs := BuildPipeline(c, cfg, wholeScreen(cfg, 42), CaptureAuto, &sys)
	appArgs := BuildPipeline(c, cfg, wholeScreen(cfg, 42), CaptureAuto, &app)

	// Find where the audio chain starts in each (the second pipewiresrc).
	sysTail := audioChainAfterSource(t, sysArgs)
	appTail := audioChainAfterSource(t, appArgs)
	if strings.Join(sysTail, " ") != strings.Join(appTail, " ") {
		t.Errorf("the audio branch differs downstream of the source:\n system: %s\n    app: %s",
			strings.Join(sysTail, " "), strings.Join(appTail, " "))
	}

	// And the source itself addresses our sink by serial, with the same
	// capture-sink property the system candidate uses.
	p := pipelineString(appArgs)
	if !strings.Contains(p, "pipewiresrc target-object=4242") {
		t.Errorf("the app source does not address the capture sink by serial:\n%s", p)
	}
	if !strings.Contains(p, "stream.capture.sink=true") {
		t.Errorf("the app source does not ask for the sink's monitor:\n%s", p)
	}
	// The video half is untouched — the same guarantee AG2 makes for the
	// whole-screen path.
	if !strings.Contains(p, "pipewiresrc fd=3 path=42") {
		t.Errorf("the video source changed in app mode:\n%s", p)
	}
}

// audioChainAfterSource returns everything from the audio branch's first "!"
// onwards, i.e. the part that must not vary between audio modes.
func audioChainAfterSource(t *testing.T, args []string) []string {
	t.Helper()
	// The audio chain begins at the second pipewiresrc (the first is video).
	seen := 0
	for i, a := range args {
		if a != "pipewiresrc" {
			continue
		}
		seen++
		if seen < 2 {
			continue
		}
		for j := i; j < len(args); j++ {
			if args[j] == "queue" {
				return args[j:]
			}
		}
	}
	t.Fatalf("no audio chain found in %v", args)
	return nil
}

// AD1, asserted where it matters most: a whole-screen broadcast never reaches
// the whose-audio step at all. Not "shows nothing" — is never asked.
func TestWholeScreenNeverAsksAboutAppAudio(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   portal.SourceType
	}{
		{"a monitor", portal.SourceMonitor},
		{"a portal that reports no source type", portal.SourceUnknown},
		{"a virtual display", portal.SourceVirtual},
	} {
		t.Run(tc.name, func(t *testing.T) {
			asked := false
			fp := &fakePortal{sourceType: tc.st, width: 1920, height: 1080}
			s := newSource(t, Options{
				Binary:     streamingBinary(t),
				OpenPortal: fp.open,
				ChooseAudioTarget: func(context.Context, AppAudioOffer) engine.AudioTarget {
					asked = true
					return engine.AudioTarget{Mode: engine.AudioTargetApp, Binary: "fake-game"}
				},
				HelperBinary: workingHelper(t, "4242"),
			})
			if _, err := s.Start(context.Background()); err != nil {
				t.Fatalf("Start: %v", err)
			}
			defer s.Stop()
			if asked {
				t.Error("a whole-screen broadcast was asked whose audio to capture")
			}
			if got := s.AudioApp(); got != "" {
				t.Errorf("AudioApp = %q, want empty for a whole-screen broadcast", got)
			}
		})
	}
}

// The happy path: a window is picked, an application is chosen, and the source
// captures that application's sink rather than the machine's.
func TestWindowWithAnApplicationChosenCapturesItsSink(t *testing.T) {
	fp := windowPortal(1000, 700)
	var offer AppAudioOffer
	s := newSource(t, Options{
		Binary:     streamingBinary(t),
		OpenPortal: fp.open,
		AudioTrial: newAudioTrialer("pipewire-monitor", AppAudioSourceName).fn(),
		ChooseAudioTarget: func(_ context.Context, o AppAudioOffer) engine.AudioTarget {
			offer = o
			return engine.AudioTarget{Mode: engine.AudioTargetApp, Binary: "fake-game"}
		},
		HelperBinary: workingHelper(t, "4242"),
	})
	if _, err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	if offer.Err != nil {
		t.Errorf("the offer carried an error: %v", offer.Err)
	}
	if len(offer.Apps) != 1 || offer.Apps[0].Binary != "fake-game" {
		t.Errorf("the offer listed %+v, want the helper's one application", offer.Apps)
	}
	if got := s.AudioApp(); got != "fake-game" {
		t.Errorf("AudioApp = %q, want fake-game", got)
	}
	format, ok := s.AudioFormat()
	if !ok {
		t.Fatal("no audio format: the app-audio lane did not start")
	}
	if format.Source != AppAudioSourceName {
		t.Errorf("audio source = %q, want %q", format.Source, AppAudioSourceName)
	}
	// The format the viewer is promised is the R25 format, unchanged.
	if format.SampleRate != engine.AudioSampleRate || format.Channels != engine.AudioChannels {
		t.Errorf("audio format = %dHz/%dch, want the R25 contract", format.SampleRate, format.Channels)
	}
	if n, ok := s.AudioLinks(); !ok || n != 2 {
		t.Errorf("AudioLinks = %d (known=%v), want 2", n, ok)
	}
}

// Choosing whole-system audio in the card is a first-class answer, not a
// fallback: the broadcast behaves exactly as a whole-screen one would.
func TestWindowWithSystemAudioChosenBehavesLikeBefore(t *testing.T) {
	fp := windowPortal(1000, 700)
	s := newSource(t, Options{
		Binary:     streamingBinary(t),
		OpenPortal: fp.open,
		AudioTrial: newAudioTrialer("pipewire-monitor").fn(),
		ChooseAudioTarget: func(context.Context, AppAudioOffer) engine.AudioTarget {
			return engine.AudioTarget{Mode: engine.AudioTargetSystem}
		},
		HelperBinary: workingHelper(t, "4242"),
	})
	if _, err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	if got := s.AudioApp(); got != "" {
		t.Errorf("AudioApp = %q, want empty for system audio", got)
	}
	format, ok := s.AudioFormat()
	if !ok {
		t.Fatal("system audio did not start")
	}
	if format.Source != "pipewire-monitor" {
		t.Errorf("audio source = %q, want the system candidate", format.Source)
	}
}

// "No audio" is a choice, and it must read as *chosen silence* rather than as
// a machine that could not manage any — the distinction docs/28 Decision 6
// exists to keep visible.
func TestChoosingNoAudioTurnsTheLaneOff(t *testing.T) {
	fp := windowPortal(1000, 700)
	s := newSource(t, Options{
		Binary:     streamingBinary(t),
		OpenPortal: fp.open,
		AudioTrial: newAudioTrialer("pipewire-monitor").fn(),
		ChooseAudioTarget: func(context.Context, AppAudioOffer) engine.AudioTarget {
			return engine.AudioTarget{Mode: engine.AudioTargetNone}
		},
		HelperBinary: workingHelper(t, "4242"),
	})
	if _, err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	if state := s.AudioState(); state != engine.AudioOff {
		t.Errorf("AudioState = %q, want %q", state, engine.AudioOff)
	}
	if _, ok := s.AudioFormat(); ok {
		t.Error("an audio format was offered after the broadcaster chose silence")
	}
	if s.Audio() != nil {
		t.Error("an audio channel exists after the broadcaster chose silence")
	}
}

// D6, row 1: no helper binary means the step still happens — the shell is told
// so it can say a sentence and offer system audio — and the broadcast is
// entirely unaffected.
func TestAMissingHelperOffersSystemAudioAndNeverFailsTheStart(t *testing.T) {
	fp := windowPortal(1000, 700)
	var offer AppAudioOffer
	asked := false
	s := newSource(t, Options{
		Binary:     streamingBinary(t),
		OpenPortal: fp.open,
		AudioTrial: newAudioTrialer("pipewire-monitor").fn(),
		ChooseAudioTarget: func(_ context.Context, o AppAudioOffer) engine.AudioTarget {
			asked, offer = true, o
			// The shell's honest answer when app audio is unavailable.
			return engine.AudioTarget{Mode: engine.AudioTargetSystem}
		},
		HelperBinary: filepath.Join(t.TempDir(), "does-not-exist"),
	})
	if _, err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start failed over an audio helper: %v", err)
	}
	defer s.Stop()

	if !asked {
		t.Fatal("the shell was never asked, so it could not say anything to the user")
	}
	if offer.Err == nil {
		t.Error("the offer did not report that per-application audio is unavailable")
	}
	if !errors.Is(offer.Err, appaudio.ErrHelperMissing) {
		t.Errorf("offer.Err = %v, want ErrHelperMissing", offer.Err)
	}
	if _, ok := s.AudioFormat(); !ok {
		t.Error("system audio did not carry on when app audio was unavailable")
	}
}

// D6: an application chosen but not capturable degrades to system audio. The
// user picked a window and granted a portal session; throwing that away over a
// sound card would be the worst possible trade.
func TestAFailedAppCaptureDegradesToSystemAudio(t *testing.T) {
	// A helper that answers everything but never produces a sink.
	silent := fakeHelper(t, `
echo '{"event":"ready","version":"fake-1.0"}'
echo '{"event":"apps","apps":[{"binary":"fake-game","name":"Fake Game","streams":1}]}'
while IFS= read -r line; do :; done
`)
	fp := windowPortal(1000, 700)
	s := newSource(t, Options{
		Binary:     streamingBinary(t),
		OpenPortal: fp.open,
		AudioTrial: newAudioTrialer("pipewire-monitor").fn(),
		ChooseAudioTarget: func(context.Context, AppAudioOffer) engine.AudioTarget {
			return engine.AudioTarget{Mode: engine.AudioTargetApp, Binary: "fake-game"}
		},
		HelperBinary: silent,
	})
	if _, err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start failed over an audio helper: %v", err)
	}
	defer s.Stop()

	if got := s.AudioApp(); got != "" {
		t.Errorf("AudioApp = %q after a failed capture, want empty", got)
	}
	format, ok := s.AudioFormat()
	if !ok {
		t.Fatal("audio did not fall back to the system source")
	}
	if format.Source != "pipewire-monitor" {
		t.Errorf("audio source = %q, want the system candidate", format.Source)
	}
}

// D6: the capture path is trialled before the broadcast depends on it, and a
// trial that fails degrades rather than shipping a pipeline that will die three
// seconds into the live probe.
func TestAnAppSinkThatWillNotCaptureDegradesToSystemAudio(t *testing.T) {
	fp := windowPortal(1000, 700)
	s := newSource(t, Options{
		Binary:     streamingBinary(t),
		OpenPortal: fp.open,
		// The system candidate passes; the app sink does not.
		AudioTrial:   newAudioTrialer("pipewire-monitor").fn(),
		HelperBinary: workingHelper(t, "4242"),
		ChooseAudioTarget: func(context.Context, AppAudioOffer) engine.AudioTarget {
			return engine.AudioTarget{Mode: engine.AudioTargetApp, Binary: "fake-game"}
		},
	})
	if _, err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	if got := s.AudioApp(); got != "" {
		t.Errorf("AudioApp = %q after a failed trial, want empty", got)
	}
	if format, ok := s.AudioFormat(); !ok || format.Source != "pipewire-monitor" {
		t.Errorf("audio did not degrade to the system source (ok=%v, %+v)", ok, format)
	}
}

// The explicit-device override is the bigger hammer: naming a device pins it
// and the whose-audio step does not appear at all. Two audio masters would be
// worse than either.
func TestAnExplicitAudioDeviceSkipsTheWhoseAudioStep(t *testing.T) {
	asked := false
	fp := windowPortal(1000, 700)
	src, err := NewFactory(Options{
		Binary:     streamingBinary(t),
		OpenPortal: fp.open,
		AudioTrial: newAudioTrialer("pulse-device").fn(),
		Trial:      newTrialer(CandidateNames()...).fn(),
		ChooseAudioTarget: func(context.Context, AppAudioOffer) engine.AudioTarget {
			asked = true
			return engine.AudioTarget{Mode: engine.AudioTargetApp, Binary: "fake-game"}
		},
		HelperBinary:    workingHelper(t, "4242"),
		LiveProbeWindow: 150 * time.Millisecond,
	})(deviceMediaConfig("some.device"), &engine.FakeClock{Us: 1000}, testLog)
	if err != nil {
		t.Fatal(err)
	}
	s := src.(*Source)
	if _, err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	if asked {
		t.Error("the whose-audio step appeared even though a device was named")
	}
	if format, ok := s.AudioFormat(); !ok || format.Source != "pulse-device" {
		t.Errorf("the named device was not captured (ok=%v, %+v)", ok, format)
	}
}

// Silence chosen up front (-audio=false) means the step is pointless: there is
// nothing to ask about.
func TestDisabledAudioSkipsTheWhoseAudioStep(t *testing.T) {
	asked := false
	fp := windowPortal(1000, 700)
	cfg := engine.DefaultMediaConfig()
	cfg.DisableAudio = true
	src, err := NewFactory(Options{
		Binary:     streamingBinary(t),
		OpenPortal: fp.open,
		Trial:      newTrialer(CandidateNames()...).fn(),
		AudioTrial: newAudioTrialer().fn(),
		ChooseAudioTarget: func(context.Context, AppAudioOffer) engine.AudioTarget {
			asked = true
			return engine.AudioTarget{Mode: engine.AudioTargetApp, Binary: "fake-game"}
		},
		HelperBinary:    workingHelper(t, "4242"),
		LiveProbeWindow: 150 * time.Millisecond,
	})(cfg, &engine.FakeClock{Us: 1000}, testLog)
	if err != nil {
		t.Fatal(err)
	}
	s := src.(*Source)
	if _, err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	if asked {
		t.Error("the whose-audio step appeared for a broadcast that asked for silence")
	}
	if s.AudioState() != engine.AudioOff {
		t.Errorf("AudioState = %q, want off", s.AudioState())
	}
}

// D5's mid-session switch, from the engine's side: it is a re-link, so the
// source's audio candidate — and therefore the gst pipeline — is untouched, and
// only the reported application changes.
func TestSwitchToSystemAudioKeepsTheSameCandidate(t *testing.T) {
	fp := windowPortal(1000, 700)
	s := newSource(t, Options{
		Binary:     streamingBinary(t),
		OpenPortal: fp.open,
		AudioTrial: newAudioTrialer("pipewire-monitor", AppAudioSourceName).fn(),
		ChooseAudioTarget: func(context.Context, AppAudioOffer) engine.AudioTarget {
			return engine.AudioTarget{Mode: engine.AudioTargetApp, Binary: "fake-game"}
		},
		HelperBinary: workingHelper(t, "4242"),
	})
	if _, err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	before, ok := s.AudioFormat()
	if !ok {
		t.Fatal("app audio did not start")
	}
	if err := s.SwitchToSystemAudio(context.Background()); err != nil {
		t.Fatalf("SwitchToSystemAudio: %v", err)
	}
	after, ok := s.AudioFormat()
	if !ok {
		t.Fatal("audio stopped across the switch")
	}
	if after.Source != before.Source {
		t.Errorf("the audio source changed across the switch (%q → %q); the pipeline would have had to renegotiate",
			before.Source, after.Source)
	}
	if got := s.AudioApp(); got != "" {
		t.Errorf("AudioApp = %q after switching to system audio, want empty", got)
	}
}

// Switching when there is nothing to switch says so plainly instead of doing
// something surprising.
func TestSwitchToSystemAudioWithoutAppAudioIsRefusedPlainly(t *testing.T) {
	fp := &fakePortal{sourceType: portal.SourceMonitor, width: 1920, height: 1080}
	s := newSource(t, Options{Binary: streamingBinary(t), OpenPortal: fp.open})
	if _, err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()
	if err := s.SwitchToSystemAudio(context.Background()); err == nil {
		t.Error("switching to system audio succeeded on a broadcast that was already using it")
	}
}

// The helper is a per-broadcast object: stopping the source stops it, and with
// it the sink and every link, because they belong to its connection.
func TestStoppingTheSourceStopsTheHelper(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "helper-exited")
	helper := fakeHelper(t, `
trap 'touch `+marker+`' EXIT
echo '{"event":"ready","version":"fake-1.0"}'
echo '{"event":"apps","apps":[{"binary":"fake-game","name":"Fake Game","streams":1}]}'
while IFS= read -r line; do
  case "$line" in
    *'"capture"'*)
      echo '{"event":"sink","serial":4242,"nodeId":42,"channels":2}'
      echo '{"event":"links","binary":"fake-game","links":2}'
      ;;
  esac
done
`)
	fp := windowPortal(1000, 700)
	s := newSource(t, Options{
		Binary:     streamingBinary(t),
		OpenPortal: fp.open,
		AudioTrial: newAudioTrialer("pipewire-monitor", AppAudioSourceName).fn(),
		ChooseAudioTarget: func(context.Context, AppAudioOffer) engine.AudioTarget {
			return engine.AudioTarget{Mode: engine.AudioTargetApp, Binary: "fake-game"}
		},
		HelperBinary: helper,
	})
	if _, err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Error("the app-audio helper outlived the broadcast that spawned it")
	}
}

// The app list the shell renders comes through live, not only in the initial
// offer: an application that starts playing while the card is open appears.
func TestAppListUpdatesReachTheShell(t *testing.T) {
	helper := fakeHelper(t, `
echo '{"event":"ready","version":"fake-1.0"}'
echo '{"event":"apps","apps":[{"binary":"first","name":"First","streams":1}]}'
echo '{"event":"apps","apps":[{"binary":"first","name":"First","streams":1},{"binary":"second","name":"Second","streams":1}]}'
while IFS= read -r line; do
  case "$line" in
    *'"capture"'*) echo '{"event":"sink","serial":7,"nodeId":8,"channels":2}' ;;
  esac
done
`)
	updates := make(chan []pwproto.App, 8)
	fp := windowPortal(1000, 700)
	s := newSource(t, Options{
		Binary:      streamingBinary(t),
		OpenPortal:  fp.open,
		AudioTrial:  newAudioTrialer("pipewire-monitor").fn(),
		OnAudioApps: func(apps []pwproto.App) { updates <- apps },
		ChooseAudioTarget: func(context.Context, AppAudioOffer) engine.AudioTarget {
			// Wait until the second list has arrived, the way a card waits for
			// a click.
			for apps := range updates {
				if len(apps) == 2 {
					break
				}
			}
			return engine.AudioTarget{Mode: engine.AudioTargetSystem}
		},
		HelperBinary: helper,
	})
	if _, err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()
}
