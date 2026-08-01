// Package appaudio is the engine's side of the PipeWire helper: spawn it,
// speak its protocol, and survive its death (R35, docs/39 D4/D6).
//
// The rule the whole package is built around is R25 Decision 6, inherited
// whole: **audio is subordinate**. Nothing here returns an error that a caller
// is expected to turn into a failed broadcast. A missing helper binary, a
// helper that dies, a sink that cannot be created — each degrades to system
// audio or to silence, with a sentence for the user, while video carries on
// exactly as it would have.
package appaudio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/Tuhis/gawk/gawk-broadcast/internal/pwproto"
)

// HelperBinary is the name the helper ships under. It is looked for next to
// the running binary first — the two travel together in the CI artifact and in
// every install layout INSTALL.md describes — and only then on PATH.
const HelperBinary = "gawk-pw-helper"

// ErrHelperMissing is returned when the helper binary cannot be found or run.
//
// It is a sentence, not a stack trace, because it is shown to a broadcaster
// who is about to go live and whose next action is either "install the other
// file" or "use system audio instead".
var ErrHelperMissing = errors.New(
	"per-application audio needs the gawk-pw-helper program next to gawk-broadcast; " +
		"whole-system audio still works")

// captureTimeout bounds the wait for the helper to build the sink. Audio is
// subordinate, so the important property is that it is *bounded*: a sound
// server that has stopped answering must delay a broadcast by seconds, not
// hold it.
const captureTimeout = 10 * time.Second

// Options configure Start.
type Options struct {
	// Binary overrides the helper path (tests, unusual installs).
	Binary string
	// Log receives the helper's own diagnostics.
	Log *slog.Logger
	// OnApps is called whenever the list of emitting applications changes,
	// including once with the initial list. The GUI's whose-audio card renders
	// this live.
	OnApps func([]pwproto.App)
	// OnLinks is called whenever the number of links into the capture sink
	// changes. Zero means the captured application has gone silent, which is
	// what drives the GUI's silence hint (D6) — a link count, not a level
	// meter.
	OnLinks func(int)
	// OnFatal is called once if the helper dies or reports a fatal condition.
	// The caller's job is to degrade, never to fail.
	OnFatal func(error)
}

// Controller owns one helper process.
type Controller struct {
	cmd  *exec.Cmd
	in   io.WriteCloser
	log  *slog.Logger
	opts Options

	mu       sync.Mutex
	apps     []pwproto.App
	links    int
	linksSet bool
	sink     uint32
	channels int
	fatal    error
	closed   bool

	// ready closes when the helper has reported both its readiness and its
	// first application list. Both, not just the first: the list is what the
	// whose-audio card renders, and handing the shell an empty one because we
	// asked a millisecond early would make a running game look like a machine
	// with nothing playing.
	ready     chan struct{}
	sawReady  bool
	sawApps   bool
	readyOnce sync.Once
	// sinks carries EventSink to whoever is waiting on a capture request.
	sinks chan pwproto.Event
	done  chan struct{}
}

// Start spawns the helper and waits for it to be ready.
//
// "Ready" means it has connected to PipeWire and completed one registry
// round-trip, so an empty application list is an answer rather than a guess —
// which the GUI needs, because "nothing is playing audio" and "we have not
// looked yet" call for different words on screen.
func Start(ctx context.Context, opts Options) (*Controller, error) {
	bin := opts.Binary
	if bin == "" {
		var err error
		if bin, err = findHelper(); err != nil {
			return nil, err
		}
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}

	cmd := exec.Command(bin)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHelperMissing, err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHelperMissing, err)
	}
	// The helper's stderr is libpipewire's complaints. They go to the same
	// place every other child's do — our log — rather than to a terminal
	// nobody is watching.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHelperMissing, err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrHelperMissing, err)
	}

	c := &Controller{
		cmd:   cmd,
		in:    stdin,
		log:   opts.Log,
		opts:  opts,
		ready: make(chan struct{}),
		sinks: make(chan pwproto.Event, 4),
		done:  make(chan struct{}),
	}
	go c.readEvents(stdout)
	go c.drainStderr(stderr)

	select {
	case <-c.ready:
		return c, nil
	case <-c.done:
		c.Close()
		return nil, fmt.Errorf("%w: the helper exited before it was ready", ErrHelperMissing)
	case <-ctx.Done():
		c.Close()
		return nil, ctx.Err()
	case <-time.After(captureTimeout):
		c.Close()
		return nil, fmt.Errorf("%w: the helper did not answer in %s", ErrHelperMissing, captureTimeout)
	}
}

// findHelper looks next to the running binary, then on PATH.
func findHelper() (string, error) {
	if exe, err := os.Executable(); err == nil {
		candidate := filepath.Join(filepath.Dir(exe), HelperBinary)
		if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
			return candidate, nil
		}
	}
	path, err := exec.LookPath(HelperBinary)
	if err != nil {
		return "", ErrHelperMissing
	}
	return path, nil
}

func (c *Controller) readEvents(r io.Reader) {
	defer close(c.done)
	reader := pwproto.NewReader[pwproto.Event](r, pwproto.MaxLine)
	for {
		e, err := reader.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				c.setFatal(err)
			} else if !c.isClosed() {
				c.setFatal(errors.New("the app-audio helper exited"))
			}
			return
		}
		switch e.Event {
		case pwproto.EventReady:
			c.log.Info("app-audio helper ready", "libpipewire", e.Version)
			c.mu.Lock()
			c.sawReady = true
			c.mu.Unlock()
			c.maybeReady()
		case pwproto.EventApps:
			c.mu.Lock()
			c.apps = e.Apps
			c.sawApps = true
			c.mu.Unlock()
			c.maybeReady()
			if c.opts.OnApps != nil {
				c.opts.OnApps(e.Apps)
			}
		case pwproto.EventSink:
			c.mu.Lock()
			c.sink, c.channels = e.Serial, e.Channels
			c.mu.Unlock()
			select {
			case c.sinks <- e:
			default:
			}
		case pwproto.EventLinks:
			n, ok := e.LinkCount()
			if !ok {
				break
			}
			if e.Message != "" {
				c.log.Warn("app-audio helper could not make a link", "err", e.Message)
			}
			c.mu.Lock()
			changed := !c.linksSet || c.links != n
			c.links, c.linksSet = n, true
			c.mu.Unlock()
			if changed && c.opts.OnLinks != nil {
				c.opts.OnLinks(n)
			}
		case pwproto.EventFatal:
			c.setFatal(errors.New(e.Message))
		}
	}
}

// maybeReady releases Start once both the readiness signal and the first
// application list have arrived.
func (c *Controller) maybeReady() {
	c.mu.Lock()
	both := c.sawReady && c.sawApps
	c.mu.Unlock()
	if both {
		c.readyOnce.Do(func() { close(c.ready) })
	}
}

func (c *Controller) drainStderr(r io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			c.log.Warn("app-audio helper", "stderr", string(buf[:n]))
		}
		if err != nil {
			return
		}
	}
}

func (c *Controller) setFatal(err error) {
	c.mu.Lock()
	first := c.fatal == nil && !c.closed
	if first {
		c.fatal = err
	}
	c.mu.Unlock()
	if !first {
		return
	}
	c.log.Warn("app-audio helper failed; audio will degrade, video is unaffected", "err", err)
	if c.opts.OnFatal != nil {
		c.opts.OnFatal(err)
	}
}

func (c *Controller) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// Apps returns the applications currently emitting audio.
func (c *Controller) Apps() []pwproto.App {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]pwproto.App(nil), c.apps...)
}

// Links returns the number of ports currently linked into the capture sink,
// and whether the helper has reported one yet.
func (c *Controller) Links() (int, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.links, c.linksSet
}

// Err returns the failure that ended the helper, or nil.
func (c *Controller) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fatal
}

// Capture points the sink at one application and returns the sink's serial —
// what the gst pipeline addresses with `target-object`.
//
// Zero links on return is not an error: an application that has gone quiet
// since the user picked it will be linked the moment it plays again, without
// another request.
func (c *Controller) Capture(ctx context.Context, binary string) (uint32, error) {
	return c.capture(ctx, pwproto.Request{Op: pwproto.OpCapture, Binary: binary})
}

// CaptureSystem re-links the sink to the machine's own output — docs/39 D5's
// mid-session escape hatch. The sink keeps its identity, so the gst pipeline
// and the Opus stream carry on untouched.
func (c *Controller) CaptureSystem(ctx context.Context) (uint32, error) {
	return c.capture(ctx, pwproto.Request{Op: pwproto.OpCaptureSystem})
}

func (c *Controller) capture(ctx context.Context, req pwproto.Request) (uint32, error) {
	if serial, ok := c.sinkSerial(); ok {
		// The sink already exists; re-targeting is a re-link and needs no
		// round-trip to learn an identity that has not changed.
		if err := c.send(req); err != nil {
			return 0, err
		}
		return serial, nil
	}
	if err := c.send(req); err != nil {
		return 0, err
	}
	select {
	case e := <-c.sinks:
		if e.Serial == 0 {
			return 0, errors.New("the app-audio helper created a sink with no serial")
		}
		return e.Serial, nil
	case <-c.done:
		return 0, c.errOr("the app-audio helper exited before creating a capture sink")
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-time.After(captureTimeout):
		return 0, fmt.Errorf("the app-audio helper did not create a capture sink within %s", captureTimeout)
	}
}

func (c *Controller) sinkSerial() (uint32, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sink, c.sink != 0
}

// Channels is the capture sink's channel count, or 0 before one exists.
func (c *Controller) Channels() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.channels
}

func (c *Controller) errOr(msg string) error {
	if err := c.Err(); err != nil {
		return err
	}
	return errors.New(msg)
}

// Watch asks for the current application list. The helper reports changes
// unprompted; this is for a caller that has just started listening.
func (c *Controller) Watch() error {
	return c.send(pwproto.Request{Op: pwproto.OpWatch})
}

// Release drops the links and the sink without stopping the helper.
func (c *Controller) Release() error {
	return c.send(pwproto.Request{Op: pwproto.OpRelease})
}

func (c *Controller) send(req pwproto.Request) error {
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return errors.New("the app-audio helper is closed")
	}
	if _, err := c.in.Write(append(b, '\n')); err != nil {
		return c.errOr(err.Error())
	}
	return nil
}

// Close ends the helper.
//
// Closing stdin is the whole protocol: the helper exits, and the daemon
// destroys the sink and every link because they belong to a connection that
// just went away. The Kill below is a backstop for a helper wedged somewhere it
// cannot see stdin, and the cleanup is identical either way — which is the
// property docs/39 D4 asks for and AG5 asserts.
func (c *Controller) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.mu.Unlock()

	_ = c.in.Close()
	select {
	case <-c.done:
	case <-time.After(2 * time.Second):
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
	}
	_ = c.cmd.Wait()
	return nil
}
