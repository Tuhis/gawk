package gst

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/Tuhis/gawk/gawk-broadcast/internal/engine"
)

func testCfg() engine.MediaConfig { return engine.DefaultMediaConfig() }

// trialer records which candidates were probed and answers per element.
type trialer struct {
	tried []string
	ok    map[string]bool
}

func newTrialer(working ...string) *trialer {
	t := &trialer{ok: map[string]bool{}}
	for _, w := range working {
		t.ok[w] = true
	}
	return t
}

func (tr *trialer) fn() TrialFunc {
	return func(ctx context.Context, c Candidate, cfg engine.MediaConfig) error {
		tr.tried = append(tr.tried, c.Element)
		if tr.ok[c.Element] {
			return nil
		}
		return errors.New("element listed but does not encode on this device")
	}
}

// Decision 4 + Decision 21: Vulkan Video is the target encode API — the only
// one spanning RADV, ANV and NVIDIA without an asterisk — so it leads and gets
// exercised on every capable machine from day one.
func TestVulkanLeadsTheCascade(t *testing.T) {
	if Cascade[0].Element != "vulkanh264enc" {
		t.Errorf("cascade starts with %q, want vulkanh264enc (Decision 21: the target API leads)", Cascade[0].Element)
	}
	want := []string{"vulkanh264enc", "nvh264enc", "vah264enc"}
	if got := CandidateNames(); !slices.Equal(got, want) {
		t.Errorf("cascade = %v, want %v", got, want)
	}
}

// There is no software rung, and that is the decision, not an omission.
func TestCascadeHasNoSoftwareRung(t *testing.T) {
	for _, c := range Cascade {
		for _, soft := range []string{"x264", "openh264", "avenc"} {
			if strings.Contains(c.Element, soft) {
				t.Errorf("cascade contains software encoder %q — Decision 4 refuses instead, and points at the browser", c.Element)
			}
		}
	}
}

// Availability is never the test: gst-inspect happily lists elements that fail
// at runtime on the actual device.
func TestCascadePicksTheFirstThatActuallyEncodes(t *testing.T) {
	tr := newTrialer("nvh264enc")
	got, err := SelectEncoder(context.Background(), testCfg(), "", tr.fn())
	if err != nil {
		t.Fatalf("SelectEncoder: %v", err)
	}
	if got.Element != "nvh264enc" {
		t.Errorf("chose %q, want nvh264enc", got.Element)
	}
	// It tried Vulkan first and rejected it on the trial's evidence.
	if !slices.Equal(tr.tried, []string{"vulkanh264enc", "nvh264enc"}) {
		t.Errorf("probed %v, want [vulkanh264enc nvh264enc]", tr.tried)
	}
}

// Decision 4: no candidate ⇒ refusal, never a software fallback, never a
// stack trace.
func TestNoHardwareCandidateRefusesAndPointsAtTheBrowser(t *testing.T) {
	tr := newTrialer() // nothing works
	_, err := SelectEncoder(context.Background(), testCfg(), "", tr.fn())
	if !errors.Is(err, engine.ErrNoHardwareEncoder) {
		t.Fatalf("error = %v, want ErrNoHardwareEncoder", err)
	}
	// Every candidate was genuinely tried before refusing.
	if !slices.Equal(tr.tried, CandidateNames()) {
		t.Errorf("probed %v, want the whole cascade %v", tr.tried, CandidateNames())
	}
	// The error names what failed, so a user can act on it.
	if !strings.Contains(err.Error(), "vulkanh264enc") {
		t.Errorf("error does not say what was tried: %v", err)
	}
	// And the message a user actually sees points at the browser.
	if !strings.Contains(NoHardwareMessage, "browser") {
		t.Error("the refusal message does not point at the browser broadcaster")
	}
}

// A cached answer saves four serial probes before every session — but it is
// re-verified, so it stays runtime truth rather than a trusted cache.
func TestLastGoodEncoderIsReverifiedFirst(t *testing.T) {
	tr := newTrialer("vah264enc")
	got, err := SelectEncoder(context.Background(), testCfg(), "vah264enc", tr.fn())
	if err != nil {
		t.Fatalf("SelectEncoder: %v", err)
	}
	if got.Element != "vah264enc" {
		t.Errorf("chose %q, want the cached vah264enc", got.Element)
	}
	if !slices.Equal(tr.tried, []string{"vah264enc"}) {
		t.Errorf("probed %v, want only the cached encoder [vah264enc]", tr.tried)
	}
}

// A stale cache (driver update, GPU swap) must fall through to the full
// cascade, not refuse.
func TestStaleLastGoodFallsBackToTheFullCascade(t *testing.T) {
	tr := newTrialer("nvh264enc")
	got, err := SelectEncoder(context.Background(), testCfg(), "vah264enc", tr.fn())
	if err != nil {
		t.Fatalf("SelectEncoder: %v", err)
	}
	if got.Element != "nvh264enc" {
		t.Errorf("chose %q, want nvh264enc after the cache went stale", got.Element)
	}
	if !slices.Equal(tr.tried, []string{"vah264enc", "vulkanh264enc", "nvh264enc"}) {
		t.Errorf("probed %v, want the cached one then the full cascade", tr.tried)
	}
}

// An unknown cached encoder must not wedge startup.
func TestUnknownLastGoodIsIgnored(t *testing.T) {
	tr := newTrialer("vulkanh264enc")
	got, err := SelectEncoder(context.Background(), testCfg(), "some-element-we-removed", tr.fn())
	if err != nil {
		t.Fatalf("SelectEncoder: %v", err)
	}
	if got.Element != "vulkanh264enc" {
		t.Errorf("chose %q, want vulkanh264enc", got.Element)
	}
}

func TestEncoderOverrideSkipsTheCascade(t *testing.T) {
	tr := newTrialer() // nothing would pass a trial
	cfg := testCfg()
	cfg.Encoder = "vah264enc"
	got, err := SelectEncoder(context.Background(), cfg, "", tr.fn())
	if err != nil {
		t.Fatalf("SelectEncoder with an override: %v", err)
	}
	if got.Element != "vah264enc" {
		t.Errorf("chose %q, want the forced vah264enc", got.Element)
	}
	if len(tr.tried) != 0 {
		t.Errorf("probed %v with an override set; the override skips the cascade", tr.tried)
	}
}

func TestUnknownEncoderOverrideIsRejectedWithTheChoices(t *testing.T) {
	cfg := testCfg()
	cfg.Encoder = "h264_nvenc" // an ffmpeg name, a plausible mistake
	_, err := SelectEncoder(context.Background(), cfg, "", newTrialer().fn())
	if err == nil {
		t.Fatal("accepted an unknown encoder name")
	}
	for _, want := range CandidateNames() {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not offer %q as a choice: %v", want, err)
		}
	}
}

// The GOP is time-based (item 11) because a frame-count GOP is 24s at 5fps.
// The encoders only take frames, so the conversion has to happen here.
func TestGOPFramesFromMilliseconds(t *testing.T) {
	for _, tc := range []struct {
		fps, gopMs, want int
	}{
		{60, 500, 30}, // the shipped rung
		{30, 500, 15},
		{5, 500, 2},
		{60, 2000, 120},
		{0, 500, 30}, // nonsense config: a safe default, not a divide by zero
		{60, 0, 30},  // ditto
		{60, 1, 1},   // never zero: an all-keyframe stream beats a crash
	} {
		cfg := engine.MediaConfig{Fps: tc.fps, GOPMs: tc.gopMs}
		if got := gopFrames(cfg); got != tc.want {
			t.Errorf("gopFrames(fps=%d, gop=%dms) = %d, want %d", tc.fps, tc.gopMs, got, tc.want)
		}
	}
}
