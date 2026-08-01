package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-broadcast/internal/pwproto"
	"github.com/Tuhis/gawk/gawk-broadcast/internal/pwtest"
)

// helperBinary is built once for the package. The tests drive the *shipped*
// binary rather than calling run() in-process, for two reasons: the kill matrix
// (AG5) needs a real process to SIGKILL, and the thing whose cleanup is being
// asserted is a process's connection to the daemon, not a function call.
var (
	buildOnce sync.Once
	buildPath string
	buildErr  error
)

func helperBinary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "gawk-pw-helper-build")
		if err != nil {
			buildErr = err
			return
		}
		buildPath = filepath.Join(dir, "gawk-pw-helper")
		out, err := exec.Command("go", "build", "-o", buildPath, ".").CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatalf("building the helper: %v", buildErr)
	}
	return buildPath
}

// session drives one helper process.
type session struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	events chan pwproto.Event
	stderr *strings.Builder

	// seen records every event, so a timeout can show what the helper *did*
	// say. A protocol test that fails with "timed out" and nothing else is a
	// test that has to be re-run under a debugger to be useful.
	seenMu sync.Mutex
	seen   []string
}

func startHelper(t *testing.T, d *pwtest.Daemon) *session {
	t.Helper()
	cmd := d.Command(helperBinary(t))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var errBuf strings.Builder
	cmd.Stderr = &errBuf
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	s := &session{t: t, cmd: cmd, stdin: stdin, events: make(chan pwproto.Event, 256), stderr: &errBuf}
	go func() {
		defer close(s.events)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 4096), pwproto.MaxLine)
		for sc.Scan() {
			var e pwproto.Event
			if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
				continue
			}
			s.seenMu.Lock()
			s.seen = append(s.seen, sc.Text())
			s.seenMu.Unlock()
			s.events <- e
		}
	}()
	t.Cleanup(func() {
		_ = stdin.Close()
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
		if t.Failed() && errBuf.Len() > 0 {
			t.Logf("helper stderr:\n%s", errBuf.String())
		}
	})
	return s
}

func (s *session) send(req pwproto.Request) {
	s.t.Helper()
	b, err := json.Marshal(req)
	if err != nil {
		s.t.Fatal(err)
	}
	if _, err := s.stdin.Write(append(b, '\n')); err != nil {
		s.t.Fatalf("writing %v to the helper: %v", req, err)
	}
}

// await waits for an event matching pred. Failing here prints the helper's
// stderr, which is where a libpipewire complaint would be.
func (s *session) await(what string, pred func(pwproto.Event) bool) pwproto.Event {
	s.t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case e, ok := <-s.events:
			if !ok {
				s.seenMu.Lock()
				seen := strings.Join(s.seen, "\n  ")
				s.seenMu.Unlock()
				s.t.Fatalf("the helper exited while waiting for %s\n  events seen:\n  %s\n  stderr: %s",
					what, seen, s.stderr.String())
			}
			if pred(e) {
				return e
			}
		case <-deadline:
			s.seenMu.Lock()
			seen := strings.Join(s.seen, "\n  ")
			s.seenMu.Unlock()
			s.t.Fatalf("timed out waiting for %s\n  events seen:\n  %s\n  stderr: %s",
				what, seen, s.stderr.String())
		}
	}
}

func (s *session) awaitReady() pwproto.Event {
	return s.await("ready", func(e pwproto.Event) bool { return e.Event == pwproto.EventReady })
}

// awaitApp waits for an app list containing every named binary.
//
// One call per list, not one per binary: the helper sends the list whole and
// only when it changes, so waiting for "game-one" and then for "game-two"
// would consume the event that already had both and then wait forever for a
// change that has no reason to happen.
func (s *session) awaitApp(binaries ...string) pwproto.Event {
	return s.await("the app list to contain "+strings.Join(binaries, " and "), func(e pwproto.Event) bool {
		if e.Event != pwproto.EventApps {
			return false
		}
		for _, want := range binaries {
			found := false
			for _, a := range e.Apps {
				if a.Binary == want {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		return true
	})
}

func (s *session) awaitLinks(n int) pwproto.Event {
	return s.await(fmt.Sprintf("%d links", n), func(e pwproto.Event) bool {
		if e.Event != pwproto.EventLinks {
			return false
		}
		got, ok := e.LinkCount()
		return ok && got == n
	})
}

func gawkSink(t *testing.T, d *pwtest.Daemon) (pwtest.Node, bool) {
	t.Helper()
	return d.FindNode(func(n pwtest.Node) bool {
		return strings.HasPrefix(n.Name, "gawk-app-capture")
	})
}

// AS2: the whose-audio card is only as good as this list, and the list is only
// correct because identity is resolved through the owning *client* — a
// GStreamer emitter's node carries no application.process.binary at all.
func TestHelperListsEmittingApplications(t *testing.T) {
	d := pwtest.Start(t)
	d.StartEmitter("fake-game", 440)

	s := startHelper(t, d)
	s.awaitReady()
	s.send(pwproto.Request{Op: pwproto.OpWatch})
	e := s.awaitApp("fake-game")

	for _, a := range e.Apps {
		if a.Binary != "fake-game" {
			continue
		}
		if a.Streams != 1 {
			t.Errorf("streams = %d, want 1", a.Streams)
		}
		if a.Name == "" {
			t.Error("the app has no display name; the card would render a blank row")
		}
	}
}

// The list is live: an application that starts playing after the card is on
// screen appears without the user doing anything, which is what makes the
// "start the game's audio first" caveat a wait rather than a dead end.
func TestAppListUpdatesLive(t *testing.T) {
	d := pwtest.Start(t)
	s := startHelper(t, d)
	s.awaitReady()
	s.send(pwproto.Request{Op: pwproto.OpWatch})

	d.StartEmitter("late-starter", 660)
	s.awaitApp("late-starter")
}

// AS3's headline: capture is a **tee**. The application keeps playing to the
// speakers and the gawk sink receives a copy — asserted by capturing both
// monitors and requiring a signal on each, not merely by counting links.
func TestCaptureIsATeeNotAReroute(t *testing.T) {
	d := pwtest.Start(t)
	d.StartEmitter("fake-game", 440)

	s := startHelper(t, d)
	s.awaitReady()
	s.awaitApp("fake-game")
	s.send(pwproto.Request{Op: pwproto.OpCapture, Binary: "fake-game"})

	sinkEvent := s.await("the sink", func(e pwproto.Event) bool { return e.Event == pwproto.EventSink })
	if sinkEvent.Serial == 0 {
		t.Fatal("the sink event carries no serial; the gst pipeline would have nothing to address")
	}
	s.awaitLinks(2)

	speakers, ok := d.FindNode(func(n pwtest.Node) bool { return n.Name == "speakers" })
	if !ok {
		t.Fatal("the stand-in speakers vanished")
	}
	// Still playing to the speakers: the links into them are untouched.
	if got := d.LinksInto(speakers.ID); got != 2 {
		t.Errorf("links into the speakers = %d, want 2 — capture must never re-route", got)
	}

	// And both monitors carry the tone.
	if peak := d.Capture(sinkEvent.Serial, 700*time.Millisecond); peak < 0.05 {
		t.Errorf("the gawk sink's monitor peaked at %.3f, want a real signal", peak)
	}
	if peak := d.Capture(speakers.Serial, 700*time.Millisecond); peak < 0.05 {
		t.Errorf("the speakers' monitor peaked at %.3f — the application stopped being audible", peak)
	}
}

// The sink must not show up in the device lists applications and volume
// controls draw. A user finding a "gawk" output device and wondering whether to
// select it is a support question this class exists to prevent.
func TestTheCaptureSinkIsHiddenFromDeviceLists(t *testing.T) {
	d := pwtest.Start(t)
	d.StartEmitter("fake-game", 440)
	s := startHelper(t, d)
	s.awaitReady()
	s.awaitApp("fake-game")
	s.send(pwproto.Request{Op: pwproto.OpCapture, Binary: "fake-game"})
	s.await("the sink", func(e pwproto.Event) bool { return e.Event == pwproto.EventSink })

	sink, ok := gawkSink(t, d)
	if !ok {
		t.Fatal("no gawk sink in the graph")
	}
	if sink.MediaClass != "Audio/Sink/Internal" {
		t.Errorf("media.class = %q, want Audio/Sink/Internal", sink.MediaClass)
	}
}

// AG4: the captured application closing and reopening its audio streams — an
// in-game menu, an engine restart — re-links within one registry round-trip,
// with the gst pipeline untouched. This is the case single-node capture cannot
// serve, and the reason the sink exists at all.
func TestLinksFollowStreamChurn(t *testing.T) {
	d := pwtest.Start(t)
	emitter := d.StartEmitter("fake-game", 440)

	s := startHelper(t, d)
	s.awaitReady()
	s.awaitApp("fake-game")
	s.send(pwproto.Request{Op: pwproto.OpCapture, Binary: "fake-game"})
	sinkEvent := s.await("the sink", func(e pwproto.Event) bool { return e.Event == pwproto.EventSink })
	s.awaitLinks(2)

	sink, ok := gawkSink(t, d)
	if !ok {
		t.Fatal("no gawk sink")
	}

	// The application's audio goes away…
	emitter.Kill()
	s.awaitLinks(0)
	d.WaitFor("the links to drop", func() bool { return d.LinksInto(sink.ID) == 0 })

	// …and comes back as a brand-new process with brand-new node and port ids.
	d.StartEmitter("fake-game", 880)
	s.awaitLinks(2)
	d.WaitFor("the links to return", func() bool { return d.LinksInto(sink.ID) == 2 })

	// And it is really audible again, not merely linked.
	if peak := d.Capture(sinkEvent.Serial, 700*time.Millisecond); peak < 0.05 {
		t.Errorf("after the restart the sink monitor peaked at %.3f, want a real signal", peak)
	}

	// The sink itself never moved: the gst pipeline addresses it by serial and
	// must not have to renegotiate anything.
	if after, ok := gawkSink(t, d); !ok || after.Serial != sink.Serial {
		t.Errorf("the sink changed identity across churn (%+v → %+v)", sink, after)
	}
}

// A second stream from the same binary is linked too. Games routinely open one
// stream for music and another for effects, and capturing only the first is a
// broadcast that is missing half its sound.
func TestSecondStreamFromTheSameBinaryIsLinked(t *testing.T) {
	d := pwtest.Start(t)
	d.StartEmitter("fake-game", 440)

	s := startHelper(t, d)
	s.awaitReady()
	s.awaitApp("fake-game")
	s.send(pwproto.Request{Op: pwproto.OpCapture, Binary: "fake-game"})
	s.await("the sink", func(e pwproto.Event) bool { return e.Event == pwproto.EventSink })
	s.awaitLinks(2)

	d.StartEmitter("fake-game", 880) // same binary name, second process
	s.awaitLinks(4)
}

// Re-targeting is a re-link, not a renegotiation: the sink — and therefore the
// gst pipeline reading it — survives a switch to a different application. That
// is what makes docs/39 D5's mid-session switch invisible to viewers.
func TestRetargetingKeepsTheSameSink(t *testing.T) {
	d := pwtest.Start(t)
	d.StartEmitter("game-one", 440)
	d.StartEmitter("game-two", 660)

	s := startHelper(t, d)
	s.awaitReady()
	s.awaitApp("game-one", "game-two")

	s.send(pwproto.Request{Op: pwproto.OpCapture, Binary: "game-one"})
	first := s.await("the sink", func(e pwproto.Event) bool { return e.Event == pwproto.EventSink })
	s.awaitLinks(2)

	s.send(pwproto.Request{Op: pwproto.OpCapture, Binary: "game-two"})
	s.await("the new target's links", func(e pwproto.Event) bool {
		n, ok := e.LinkCount()
		return e.Event == pwproto.EventLinks && ok && n == 2 && e.Binary == "game-two"
	})

	sink, ok := gawkSink(t, d)
	if !ok {
		t.Fatal("no gawk sink after re-targeting")
	}
	if sink.Serial != first.Serial {
		t.Errorf("the sink was recreated (serial %d → %d); the gst pipeline would have lost its target",
			first.Serial, sink.Serial)
	}
	if got := d.LinksInto(sink.ID); got != 2 {
		t.Errorf("links into the sink = %d, want exactly the new target's 2", got)
	}
}

// AG5, clean exit: stdin EOF is the teardown call and it leaves nothing behind.
func TestStdinEOFExitsCleanly(t *testing.T) {
	d := pwtest.Start(t)
	d.StartEmitter("fake-game", 440)

	s := startHelper(t, d)
	s.awaitReady()
	s.awaitApp("fake-game")
	s.send(pwproto.Request{Op: pwproto.OpCapture, Binary: "fake-game"})
	s.await("the sink", func(e pwproto.Event) bool { return e.Event == pwproto.EventSink })
	s.awaitLinks(2)

	_ = s.stdin.Close()
	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("the helper exited with %v, want a clean exit", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the helper did not exit on stdin EOF")
	}

	d.WaitFor("the sink to disappear", func() bool {
		_, ok := gawkSink(t, d)
		return !ok
	})
	d.AssertNoGawkObjects()
}

// AG5's kill matrix: SIGKILL — of the helper, and of the application it is
// capturing — must leave the sound server pristine. The guarantee is not this
// program's cleanup code running (it does not run); it is that every object it
// created belongs to a connection the kernel just closed, with no
// `object.linger` anywhere to outlive it.
func TestKillMinusNineLeavesTheSoundServerPristine(t *testing.T) {
	for _, victim := range []string{"the helper", "the application then the helper"} {
		t.Run(victim, func(t *testing.T) {
			d := pwtest.Start(t)
			emitter := d.StartEmitter("fake-game", 440)

			s := startHelper(t, d)
			s.awaitReady()
			s.awaitApp("fake-game")
			s.send(pwproto.Request{Op: pwproto.OpCapture, Binary: "fake-game"})
			s.await("the sink", func(e pwproto.Event) bool { return e.Event == pwproto.EventSink })
			s.awaitLinks(2)

			sink, ok := gawkSink(t, d)
			if !ok {
				t.Fatal("no gawk sink to orphan")
			}
			speakers, _ := d.FindNode(func(n pwtest.Node) bool { return n.Name == "speakers" })

			if strings.HasPrefix(victim, "the application") {
				emitter.Kill()
				d.WaitFor("the application's links to drop", func() bool {
					return d.LinksInto(sink.ID) == 0
				})
			}

			if err := s.cmd.Process.Signal(syscall.SIGKILL); err != nil {
				t.Fatalf("SIGKILL: %v", err)
			}
			_ = s.cmd.Wait()

			d.WaitFor("the orphaned sink to be reaped", func() bool {
				_, ok := gawkSink(t, d)
				return !ok
			})
			d.AssertNoGawkObjects()
			for _, l := range d.Links() {
				if l.InNode == sink.ID || l.OutputNode == sink.ID {
					t.Errorf("an orphaned link into the dead sink survived: %+v", l)
				}
			}
			// And the user's own audio routing is exactly as it was found —
			// the property the tee shape exists to guarantee.
			if speakers.ID != 0 && !strings.HasPrefix(victim, "the application") {
				if got := d.LinksInto(speakers.ID); got != 2 {
					t.Errorf("links into the speakers = %d, want 2 — the application's own routing was disturbed", got)
				}
			}
		})
	}
}

// Releasing gives the daemon back exactly what it lent us, without the helper
// exiting: the engine uses this between broadcasts.
func TestReleaseDropsEverythingButKeepsWatching(t *testing.T) {
	d := pwtest.Start(t)
	d.StartEmitter("fake-game", 440)

	s := startHelper(t, d)
	s.awaitReady()
	s.awaitApp("fake-game")
	s.send(pwproto.Request{Op: pwproto.OpCapture, Binary: "fake-game"})
	s.await("the sink", func(e pwproto.Event) bool { return e.Event == pwproto.EventSink })
	s.awaitLinks(2)

	s.send(pwproto.Request{Op: pwproto.OpRelease})
	s.awaitLinks(0)

	sink, ok := gawkSink(t, d)
	if ok {
		d.WaitFor("the links to be gone", func() bool { return d.LinksInto(sink.ID) == 0 })
	}

	// Still alive and still watching.
	s.send(pwproto.Request{Op: pwproto.OpPing})
	s.await("a pong", func(e pwproto.Event) bool { return e.Event == pwproto.EventPong })
	d.StartEmitter("another-app", 660)
	s.awaitApp("another-app")
}

// A capture request for an application that is not playing is not an error: the
// user may have picked from a list that has just gone stale, and the right
// answer is zero links and a live sink waiting for the streams to come back.
func TestCapturingASilentApplicationYieldsNoLinksAndNoError(t *testing.T) {
	d := pwtest.Start(t)
	s := startHelper(t, d)
	s.awaitReady()

	s.send(pwproto.Request{Op: pwproto.OpCapture, Binary: "not-running"})
	s.await("the sink", func(e pwproto.Event) bool { return e.Event == pwproto.EventSink })
	s.awaitLinks(0)

	// And when it does start playing, it links without a second request.
	d.StartEmitter("not-running", 440)
	s.awaitLinks(2)
}

// The capture sink's layout is chosen from the application's own ports, not
// hardcoded (docs/39 D3, as refined in AS3). Raw port-to-port links perform no
// channel mixing, so a stereo sink under a surround application would silently
// drop the centre channel — where the dialogue lives. The stereo downmix the
// viewer receives stays where it belongs, in the gst branch's audioconvert.
func TestTheSinkMatchesTheApplicationsChannelLayout(t *testing.T) {
	// A surround machine: the speakers are 5.1, so the application's stream
	// node negotiates 5.1 output ports toward them.
	d := pwtest.StartWithSpeakers(t, "FL,FR,FC,LFE,SL,SR")
	d.StartEmitterChannels("surround-game", 440, 6)

	s := startHelper(t, d)
	s.awaitReady()
	s.awaitApp("surround-game")
	s.send(pwproto.Request{Op: pwproto.OpCapture, Binary: "surround-game"})

	sink := s.await("the sink", func(e pwproto.Event) bool { return e.Event == pwproto.EventSink })
	if sink.Channels != 6 {
		t.Errorf("the capture sink has %d channels, want 6 — a narrower sink drops channels silently", sink.Channels)
	}
	// And every one of them is linked, not just the two a stereo sink could
	// have taken.
	s.awaitLinks(6)
}
