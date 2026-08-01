package gst

import (
	"context"
	"strings"
	"testing"

	"github.com/Tuhis/gawk/gawk-broadcast/internal/engine"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/pwtest"
)

// The seam between the engine and the real helper, against a real (headless)
// PipeWire daemon.
//
// The unit tests above drive a shell script through the protocol, which proves
// the engine's branches; this proves the two halves actually agree — that the
// serial the helper reports is the serial the pipeline is built around, and
// that a real application's ports really are linked into a real sink. Only the
// GStreamer child is a stand-in here, because a runner has no GPU and no
// portal (docs/19's standing warning).
func TestEngineCapturesARealApplicationsAudio(t *testing.T) {
	d := pwtest.Start(t)
	d.StartEmitter("fake-game", 440)
	helper := pwtest.BuildHelper(t)

	// The source's own children inherit our environment, so point this process
	// at the private daemon for the duration of the test.
	for _, kv := range d.Env {
		if k, v, ok := strings.Cut(kv, "="); ok && (k == "XDG_RUNTIME_DIR" || k == "PIPEWIRE_RUNTIME_DIR" || k == "DBUS_SESSION_BUS_ADDRESS") {
			t.Setenv(k, v)
		}
	}

	fp := windowPortal(1000, 700)
	var chosen []string
	s := newSource(t, Options{
		Binary:     streamingBinary(t),
		OpenPortal: fp.open,
		// The video cascade and the audio trial stay fakes: this test is about
		// the app-audio control plane, and a real gst-launch would need a
		// portal fd it cannot have.
		AudioTrial: newAudioTrialer("pipewire-monitor", AppAudioSourceName).fn(),
		ChooseAudioTarget: func(_ context.Context, o AppAudioOffer) engine.AudioTarget {
			for _, a := range o.Apps {
				chosen = append(chosen, a.Binary)
			}
			return engine.AudioTarget{Mode: engine.AudioTargetApp, Binary: "fake-game"}
		},
		HelperBinary: helper,
	})

	if _, err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	// The offer really listed the running application…
	found := false
	for _, b := range chosen {
		if b == "fake-game" {
			found = true
		}
	}
	if !found {
		t.Errorf("the whose-audio offer listed %v, want the running fake-game", chosen)
	}
	// …and the engine really captured it.
	if got := s.AudioApp(); got != "fake-game" {
		t.Errorf("AudioApp = %q, want fake-game", got)
	}

	// A real sink, with the application's ports really linked into it.
	sink, ok := d.FindNode(func(n pwtest.Node) bool {
		return strings.HasPrefix(n.Name, "gawk-app-capture")
	})
	if !ok {
		t.Fatal("no capture sink exists in the daemon")
	}
	d.WaitFor("the application's ports to be linked", func() bool {
		return d.LinksInto(sink.ID) == 2
	})

	// The pipeline the engine would run addresses that exact sink.
	cand := s.audioCandidate()
	if cand == nil {
		t.Fatal("no audio candidate after a successful app capture")
	}
	args := BuildPipeline(Cascade[0], s.cfg, s.encodeTarget(), CaptureAuto, cand)
	if want := "target-object=" + itoa(sink.Serial); !strings.Contains(pipelineString(args), want) {
		t.Errorf("the pipeline does not address the live sink (%s):\n%s", want, pipelineString(args))
	}

	// And the mid-session switch really re-links, against a real daemon.
	if err := s.SwitchToSystemAudio(context.Background()); err != nil {
		t.Fatalf("SwitchToSystemAudio: %v", err)
	}
	d.WaitFor("the system-audio links to replace the application's", func() bool {
		return d.LinksInto(sink.ID) == 2
	})
	after, ok := d.FindNode(func(n pwtest.Node) bool { return n.ID == sink.ID })
	if !ok || after.Serial != sink.Serial {
		t.Error("the capture sink changed identity across the mid-session switch")
	}

	// Stopping the broadcast leaves the sound server exactly as it was found.
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	d.WaitFor("the capture sink to be reaped", func() bool {
		_, ok := d.FindNode(func(n pwtest.Node) bool { return n.ID == sink.ID })
		return !ok
	})
	d.AssertNoGawkObjects()
}

func itoa(v uint32) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}
