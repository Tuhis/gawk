//go:build linux

// Mid-session capture restart: docs/39 D2's second branch, the one the design
// doc named and the code did not implement.
//
// Linux-only for exactly the reasons source_test.go states: these supervise
// real child processes, and the probe windows here are load-bearing timings
// rather than arbitrary ones.
package gst

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-broadcast/internal/engine"
)

// flappingBinary returns a fake gst-launch that streams the fixture, outlives
// the probe window and *then* dies — for the first `deaths` invocations, after
// which it keeps streaming and stays up.
//
// The invocation count lives in a file because every attempt is a fresh
// process. Two details are load-bearing:
//
//   - the death is deliberately late. A child that dies inside the probe window
//     is the start-time cascade's business (source_test.go); everything here is
//     about a pipeline that was already working;
//   - the survivor streams the fixture repeatedly, paced. One burst of it says
//     nothing about which child produced what — the drop policy eats most of a
//     burst into an 8-deep queue, so counting totals cannot distinguish "the
//     rebuilt pipeline is producing" from "the dead one produced a lot". Paced
//     output makes the question a live one: are frames still arriving *now*?
//
// liveFor is how long a doomed invocation stays up, in shell `sleep` units. It
// has to outlast the probe window — otherwise the start-time cascade owns the
// failure, not the rebuild — and a test that needs to land an event inside a
// particular window stretches it.
func flappingBinary(t *testing.T, deaths int, liveFor, stderr string) string {
	t.Helper()
	state := t.TempDir() + "/runs"
	fixture := mustAbs(t, "../fixture/sample.ts")
	return fakeBinary(t, `n=$(cat `+state+` 2>/dev/null || echo 0)
echo $((n+1)) > `+state+`
if [ "$n" -lt `+strconv.Itoa(deaths)+` ]; then
  cat `+fixture+`
  sleep `+liveFor+`
  echo '`+stderr+`' >&2
  exit 1
fi
for i in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15; do
  cat `+fixture+`
  sleep 0.3
done
sleep 30
`)
}

// drainer consumes the frame channel the way the engine's pump does, so the
// source is never merely blocked on a full queue when a test asks whether it
// is still producing.
type drainer struct {
	mu     sync.Mutex
	frames int
	closed bool
}

func drainFrames(t *testing.T, frames <-chan engine.AccessUnit) *drainer {
	t.Helper()
	d := &drainer{}
	go func() {
		for range frames {
			d.mu.Lock()
			d.frames++
			d.mu.Unlock()
		}
		d.mu.Lock()
		d.closed = true
		d.mu.Unlock()
	}()
	return d
}

func (d *drainer) count() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.frames
}

func (d *drainer) channelClosed() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closed
}

// waitUntil polls cond, which is what these tests have instead of sleeps: the
// child is a real process and its death is observed, not scheduled.
func waitUntil(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", within, what)
}

// The field failure this exists for (2026-08-01, window share on a Wayland
// desktop): a pipeline that had been streaming for 27 s died with pipewiresrc's
// "unhandled format" when the compositor renegotiated the window's screencast
// format mid-stream. The capture ladder could not help — it only runs inside
// the start-time probe window — so the whole broadcast ended, viewers and all,
// over a negotiation the next attempt would very likely win.
//
// The rebuild reuses the portal grant in memory: no second picker, no new
// broadcast ID, and above all the *same* frame channel, because the engine
// treats that channel closing as the end of the session.
func TestMidSessionCaptureDeathRebuildsThePipeline(t *testing.T) {
	fp := &fakePortal{}
	s := newSource(t, Options{
		Binary:          flappingBinary(t, 1, "0.3", unhandledFormatStderr),
		OpenPortal:      fp.open,
		LiveProbeWindow: 100 * time.Millisecond,
	})

	frames, err := s.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()
	d := drainFrames(t, frames)

	waitUntil(t, 10*time.Second, "the dead capture to be rebuilt", func() bool {
		return s.CaptureRestarts() == 1
	})
	if d.channelClosed() {
		t.Fatalf("the frame channel closed on a recoverable capture death (%v) — the engine ends the broadcast on that", s.Err())
	}
	if err := s.Err(); err != nil {
		t.Errorf("Err() = %v after a successful rebuild, want nil — a recovered capture is not a failed one", err)
	}

	// Frames again, and demonstrably from the rebuilt pipeline: the dead child
	// can account for at most the queue depth of what arrives from here, so
	// growth past that is the new one producing.
	before := d.count()
	waitUntil(t, 10*time.Second, "frames from the rebuilt pipeline", func() bool {
		return d.count() > before+cap(s.frames)
	})

	if n := fp.openCount(); n != 1 {
		t.Errorf("portal opened %d times, want 1 — a rebuild must reuse the grant, not re-prompt for a window", n)
	}
	if s.Encoder() == "" || s.CapturePath() == "" {
		t.Errorf("Encoder()=%q CapturePath()=%q after a rebuild; the stats must describe the pipeline that is actually running",
			s.Encoder(), s.CapturePath())
	}
}

// A rebuild is rate-limited. Capture that dies over and over as fast as it can
// be started is not a renegotiation blip — it is a machine that cannot capture,
// and hiding it behind an endless restart loop would leave a broadcaster
// watching a permanently frozen stream with no error to act on. The budget
// spends, then the failure surfaces with the child's own last words.
//
// The budget is overridden rather than staged: sixty child deaths inside the
// production window would be a half-minute test that proves the same thing as
// three.
func TestCaptureRestartsAreRateLimited(t *testing.T) {
	fp := &fakePortal{}
	s := newSource(t, Options{
		// Dies after every adoption, forever.
		Binary:          flappingBinary(t, 1000, "0.3", "ERROR: the encoder device disappeared"),
		OpenPortal:      fp.open,
		LiveProbeWindow: 100 * time.Millisecond,
		RestartBudget:   3,
	})

	frames, err := s.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Stop()
	d := drainFrames(t, frames)

	waitUntil(t, 60*time.Second, "capture to give up", d.channelClosed)

	if n := s.CaptureRestarts(); n != 3 {
		t.Errorf("CaptureRestarts() = %d, want the full budget of 3 spent before giving up", n)
	}
	err = s.Err()
	if err == nil {
		t.Fatal("Err() = nil after capture gave up; the session would hang live with no frames")
	}
	if !strings.Contains(err.Error(), "the encoder device disappeared") {
		t.Errorf("error lost the child's last words: %v", err)
	}
}

// The bound is a rate, not a session total, and this is the difference: a
// rebuild that has aged out of the window stops counting. A total was the
// first cut of this, and it would have told a broadcaster who resized their
// window a few times over a long stream that their machine was broken — while
// still not being the thing worth refusing, which is a hot loop.
func TestOnlyRecentRebuildsCountAgainstTheBudget(t *testing.T) {
	now := uint64(10 * time.Minute / time.Microsecond)
	aged := now - uint64((captureRestartWindow + time.Second).Microseconds())
	recent := now - uint64((captureRestartWindow / 2).Microseconds())

	got := withinWindow([]uint64{aged, recent}, now, captureRestartWindow)
	if len(got) != 1 || got[0] != recent {
		t.Errorf("withinWindow kept %v, want only the one inside the window (%d)", got, recent)
	}
}

// Stop is not a capture failure, and it must not race a rebuild: the child
// dies *because* we killed it, and a source that answered by starting a fresh
// one would leave a screen being shared after the broadcaster stopped sharing
// it — the exact leak finish()'s teardown exists to prevent.
func TestStopIsNotRebuilt(t *testing.T) {
	fp := &fakePortal{}
	s := newSource(t, Options{Binary: streamingBinary(t), OpenPortal: fp.open})
	if _, err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if s.restartCapture(errors.New("gst-launch-1.0 exited: exit status 1")) {
		t.Error("a stopped source rebuilt its capture — the portal session is already released")
	}
	if n := s.CaptureRestarts(); n != 0 {
		t.Errorf("CaptureRestarts() = %d after a clean stop, want 0", n)
	}
	if err := s.Err(); err != nil {
		t.Errorf("Err() = %v after a clean stop, want nil", err)
	}
	if n := fp.openCount(); n != 1 {
		t.Errorf("portal opened %d times, want 1", n)
	}
}

// Stop landing *inside* a rebuild is the ownership question the whole
// mechanism turns on, and the only moment two pumps exist at once: the dying
// one, which closes the frame channel if the rebuild fails, and the rebuilt
// one, which closes it if it is adopted and then stopped. Both closing is a
// panic that takes the process down — so adoption and the stop flag are
// settled under one lock, and a child that loses that race is killed instead
// of adopted.
//
// The interleaving is staged, not swept: a sweep across a 20 ms probe window
// passes with the bug still in (measured). The probe window is stretched to
// 400 ms and Stop is aimed at the middle of the *rebuild's*, where the new
// child is running and not yet adopted.
func TestStopInsideARebuildDoesNotDoubleCloseTheFrameChannel(t *testing.T) {
	const probe = 400 * time.Millisecond
	fp := &fakePortal{}
	s := newSource(t, Options{
		// Every child dies a second in — comfortably past the probe window, so
		// each death is a rebuild rather than a cascade advance.
		Binary:          flappingBinary(t, 1000, "1.0", "ERROR: the encoder device disappeared"),
		OpenPortal:      fp.open,
		LiveProbeWindow: probe,
	})

	frames, err := s.Start(context.Background())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	drainFrames(t, frames)

	// Start returned the moment the first child was adopted, 600 ms before it
	// dies; half a probe window past that is inside the rebuild's.
	time.Sleep(600*time.Millisecond + probe/2)
	if err := s.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := s.Err(); err != nil {
		t.Errorf("Err() = %v after a stop that interrupted a rebuild, want nil", err)
	}
}

// A rebuilt pipeline is a new SPS lineage on a channel the viewer is already
// decoding. Two things follow, and both live on the pump handle:
//
//   - nothing may go out until its first keyframe — a delta from the new
//     encoder would reference a frame the viewer never had, and (as ever on
//     this path) frameIds stay contiguous, so freeze-on-gap cannot catch it;
//   - that first frame carries the epoch flag, which is what makes the sender
//     re-derive the DecoderConfig it otherwise caches for the whole session.
func TestFirstFrameAfterARebuildIsAKeyframeCarryingTheEpoch(t *testing.T) {
	s := &Source{frames: make(chan engine.AccessUnit, 4), log: testLog}
	h := &pumpHandle{done: make(chan struct{}), newEpoch: true, droppingGOP: true}

	s.offer(au(false), h) // a delta from the new lineage: nothing to decode it against
	if n := len(s.frames); n != 0 {
		t.Fatalf("%d frames queued before the rebuilt pipeline's first keyframe, want 0", n)
	}

	s.offer(au(true), h)
	first := <-s.frames
	if !first.Keyframe {
		t.Fatal("the first frame after a rebuild is not a keyframe")
	}
	if !first.EncoderRestarted {
		t.Error("the first frame after a rebuild does not carry the epoch; the sender would keep advertising the dead pipeline's codec")
	}

	s.offer(au(true), h)
	next := <-s.frames
	if next.EncoderRestarted {
		t.Error("the epoch flag survived past the frame that carried it; every later keyframe would re-derive the config")
	}
}
