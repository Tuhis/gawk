package gst

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-broadcast/internal/engine"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/portal"
)

var testLog = slog.New(slog.DiscardHandler)

// The child is a real subprocess in these tests, standing in for
// gst-launch-1.0. Supervision is exactly the kind of thing a mock would test
// vacuously: what we care about is that a dying process is noticed, that its
// stderr survives, and that nothing hangs.

// fakeBinary writes a shell script and returns its path.
func fakeBinary(t *testing.T, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-gst")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// A child that dies immediately, like an encoder element that lists fine but
// cannot touch the device.
func failingBinary(t *testing.T, msg string) string {
	return fakeBinary(t, "echo '"+msg+"' >&2\nexit 1\n")
}

// A child that streams the TS fixture and then stays alive, like a working
// pipeline.
func streamingBinary(t *testing.T) string {
	fixture, err := filepath.Abs("../mpegts/testdata/sample.ts")
	if err != nil {
		t.Fatal(err)
	}
	return fakeBinary(t, "cat "+fixture+"\nsleep 30\n")
}

// fakePortal counts handshakes so a test can prove the picker appears on every
// Start (we never persist the choice — docs/19).
type fakePortal struct {
	mu    sync.Mutex
	opens int
	err   error
	files []*os.File
}

func (f *fakePortal) open(ctx context.Context, opts portal.Options) (*portal.Stream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens++
	if f.err != nil {
		return nil, f.err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	w.Close()
	f.files = append(f.files, r)
	return &portal.Stream{NodeID: 7, FD: r, Version: 4}, nil
}

func (f *fakePortal) openCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opens
}

func newSource(t *testing.T, opts Options) *Source {
	t.Helper()
	if opts.LiveProbeWindow == 0 {
		opts.LiveProbeWindow = 150 * time.Millisecond
	}
	if opts.Trial == nil {
		opts.Trial = newTrialer(CandidateNames()...).fn()
	}
	src, err := NewFactory(opts)(engine.DefaultMediaConfig(), &engine.FakeClock{Us: 1000}, testLog)
	if err != nil {
		t.Fatal(err)
	}
	return src.(*Source)
}

// Decision 4's "the live start is the final probe": a videotestsrc trial cannot
// prove the real path's dmabuf import, so a child that dies immediately means
// try the next encoder — and, thanks to the restore token, without a second
// share dialog.
func TestLiveFailureAdvancesCascadeWithoutANewPicker(t *testing.T) {
	fp := &fakePortal{}
	// The first candidate the cascade reaches will fail live; every candidate
	// passed its trial, which is exactly the situation this rule exists for.
	s := newSource(t, Options{
		Binary:     failingBinary(t, "ERROR: cannot import dmabuf into CUDA"),
		OpenPortal: fp.open,
	})

	_, err := s.Start(context.Background())
	if !errors.Is(err, engine.ErrNoHardwareEncoder) {
		t.Fatalf("error = %v, want ErrNoHardwareEncoder after every candidate failed live", err)
	}
	// Every candidate was tried on the live pipeline...
	for _, name := range CandidateNames() {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error does not mention %q: %v", name, err)
		}
	}
	// ...and the user was asked for their screen exactly once.
	if n := fp.openCount(); n != 1 {
		t.Errorf("portal opened %d times, want 1 — cascade retries must reuse the grant, not re-prompt", n)
	}
	// The child's own words survive; "the subprocess exited" is unactionable.
	if !strings.Contains(err.Error(), "cannot import dmabuf into CUDA") {
		t.Errorf("error lost the child's stderr: %v", err)
	}
}

// A working pipeline produces access units, stamped from the engine's clock
// and classified by their own bitstream.
func TestSourceEmitsAccessUnits(t *testing.T) {
	fp := &fakePortal{}
	s := newSource(t, Options{Binary: streamingBinary(t), OpenPortal: fp.open})

	frames, err := s.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()

	var got []engine.AccessUnit
	timeout := time.After(5 * time.Second)
	for len(got) < 3 {
		select {
		case au, ok := <-frames:
			if !ok {
				t.Fatalf("frame channel closed after %d frames: %v", len(got), s.Err())
			}
			got = append(got, au)
		case <-timeout:
			t.Fatalf("only %d frames in 5s", len(got))
		}
	}

	// The first frame of a stream is a keyframe, and it is detected from the
	// bitstream rather than announced by anything.
	if !got[0].Keyframe {
		t.Error("first access unit is not a keyframe")
	}
	// Stamped from the injected clock (Decision 6: one clock for frames and
	// TimeSync alike).
	if got[0].TimestampUs != 1000 {
		t.Errorf("TimestampUs = %d, want the injected clock's 1000", got[0].TimestampUs)
	}
	if !got[0].HasPTS {
		t.Error("PES PTS not carried through (Decision 6's upgrade path needs it)")
	}
	if s.Encoder() != Cascade[0].Name {
		t.Errorf("Encoder() = %q, want %q", s.Encoder(), Cascade[0].Name)
	}
}

// A child that dies *after* the probe window was working: that is a genuine
// failure, and it must surface with its stderr rather than hang or go quiet.
func TestChildDeathSurfacesWithStderr(t *testing.T) {
	fp := &fakePortal{}
	// Streams briefly, then dies complaining.
	bin := fakeBinary(t, "cat "+mustAbs(t, "../mpegts/testdata/sample.ts")+"\necho 'ERROR: device disappeared' >&2\nexit 1\n")
	s := newSource(t, Options{Binary: bin, OpenPortal: fp.open, LiveProbeWindow: time.Millisecond})

	frames, err := s.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Drain until the source gives up.
	for range frames {
	}
	err = s.Err()
	if err == nil {
		t.Fatal("Err() = nil after the child died; the session would hang live with no frames")
	}
	if !strings.Contains(err.Error(), "device disappeared") {
		t.Errorf("error lost the child's last words: %v", err)
	}
}

// A clean Stop is not a failure: the child dies because we killed it.
func TestStopIsNotAnError(t *testing.T) {
	fp := &fakePortal{}
	s := newSource(t, Options{Binary: streamingBinary(t), OpenPortal: fp.open})
	if _, err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Stop(); err != nil {
		t.Errorf("Stop: %v", err)
	}
	if err := s.Err(); err != nil {
		t.Errorf("Err() = %v after a clean stop, want nil", err)
	}
	if err := s.Stop(); err != nil {
		t.Errorf("second Stop: %v", err)
	}
}

// We deliberately do not persist the screen choice: every capture start opens a
// fresh portal handshake, so the desktop's picker appears each run (docs/19).
func TestPickerAppearsOnEveryStart(t *testing.T) {
	fp := &fakePortal{}
	s := newSource(t, Options{
		Binary:     streamingBinary(t),
		OpenPortal: fp.open,
	})
	if _, err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.Stop()

	// A second, independent Source (a restart) hits the portal again — there is
	// no stored grant to short-circuit the picker with.
	s2 := newSource(t, Options{
		Binary:     streamingBinary(t),
		OpenPortal: fp.open,
	})
	if _, err := s2.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	s2.Stop()

	if got := fp.openCount(); got != 2 {
		t.Errorf("portal opened %d times across two starts, want 2 — the picker must appear each run", got)
	}
}

// The cascade winner is cached so the next run skips three serial probes.
func TestChosenEncoderIsPersisted(t *testing.T) {
	fp := &fakePortal{}
	var chosen string
	s := newSource(t, Options{
		Binary:          streamingBinary(t),
		OpenPortal:      fp.open,
		OnEncoderChosen: func(e string) { chosen = e },
	})
	if _, err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.Stop()
	if chosen != Cascade[0].Element {
		t.Errorf("persisted encoder = %q, want %q", chosen, Cascade[0].Element)
	}
}

// A cancelled picker must reach the caller as itself, so the shells can say
// "you cancelled" rather than blaming the relay or the GPU.
func TestPortalFailurePropagates(t *testing.T) {
	fp := &fakePortal{err: portal.ErrCancelled}
	s := newSource(t, Options{Binary: streamingBinary(t), OpenPortal: fp.open})
	if _, err := s.Start(context.Background()); !errors.Is(err, portal.ErrCancelled) {
		t.Fatalf("error = %v, want ErrCancelled", err)
	}
}

// An explicit override runs exactly one candidate: silently running a
// different encoder than the one the user named would be worse than failing.
func TestOverrideDoesNotAdvanceTheCascade(t *testing.T) {
	fp := &fakePortal{}
	cfg := engine.DefaultMediaConfig()
	cfg.Encoder = "vah264enc"
	src, err := NewFactory(Options{
		Binary:          failingBinary(t, "ERROR: nope"),
		OpenPortal:      fp.open,
		LiveProbeWindow: 100 * time.Millisecond,
		Trial:           newTrialer(CandidateNames()...).fn(),
	})(cfg, &engine.FakeClock{}, testLog)
	if err != nil {
		t.Fatal(err)
	}
	_, err = src.Start(context.Background())
	if err == nil {
		t.Fatal("Start succeeded with a failing forced encoder")
	}
	if errors.Is(err, engine.ErrNoHardwareEncoder) {
		t.Errorf("a forced encoder fell through to the cascade: %v", err)
	}
	if !strings.Contains(err.Error(), "vah264enc") {
		t.Errorf("error does not name the forced encoder: %v", err)
	}
}

// A missing gst-launch-1.0 is the single most likely first-run failure, and it
// must be diagnosed as itself. Reporting it as "no working hardware H.264
// encoder" — which is what happens if the cascade is allowed to swallow it —
// sends someone off to reinstall GPU drivers over a missing 200 kB CLI tool.
func TestMissingBinarySaysWhatToInstall(t *testing.T) {
	fp := &fakePortal{}
	s := newSource(t, Options{
		Binary:     filepath.Join(t.TempDir(), "definitely-not-installed"),
		OpenPortal: fp.open,
	})
	_, err := s.Start(context.Background())
	if err == nil {
		t.Fatal("Start succeeded with no gst-launch-1.0")
	}
	if !errors.Is(err, ErrNoLaunchBinary) {
		t.Errorf("error = %v, want ErrNoLaunchBinary", err)
	}
	if errors.Is(err, engine.ErrNoHardwareEncoder) {
		t.Error("a missing GStreamer is reported as missing hardware — the user would go and reinstall their GPU driver")
	}
	if !strings.Contains(err.Error(), "gstreamer1.0-tools") {
		t.Errorf("error does not name the package to install: %v", err)
	}
	// And it must fail before asking for the screen.
	if n := fp.openCount(); n != 0 {
		t.Errorf("portal opened %d times before discovering GStreamer is missing; never ask for a screen you cannot capture", n)
	}
}

// The same check must not reject a perfectly good binary found on PATH.
func TestEnsureBinaryAcceptsWhatExists(t *testing.T) {
	if err := EnsureBinary("sh"); err != nil {
		t.Errorf("EnsureBinary(sh) = %v, want nil (it is on PATH)", err)
	}
	if err := EnsureBinary(fakeBinary(t, "exit 0\n")); err != nil {
		t.Errorf("EnsureBinary(absolute path to a real file) = %v, want nil", err)
	}
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
