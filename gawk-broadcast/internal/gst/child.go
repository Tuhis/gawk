package gst

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// stderrKeepLines is how many trailing stderr lines we keep. GStreamer's fatal
// error is always in the last few, and a child that dies must surface *why* —
// "the subprocess exited" with no detail is the least actionable error there
// is.
const stderrKeepLines = 20

// ErrNoLaunchBinary is returned when gst-launch-1.0 is not installed.
//
// It is deliberately distinct from engine.ErrNoHardwareEncoder. Without the
// distinction, a machine missing a 200 kB CLI tool reports "no working
// hardware H.264 encoder found" and sends its owner off to reinstall GPU
// drivers — the single most likely first-run failure, diagnosed as the single
// hardest one to fix.
var ErrNoLaunchBinary = errors.New("gst-launch-1.0 is not installed")

// missingBinary reports whether err means "the executable isn't there".
// exec.ErrNotFound only covers a PATH lookup; an absolute path that doesn't
// exist surfaces as ENOENT instead.
func missingBinary(err error) bool {
	return errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist)
}

func missingBinaryErr(binary string) error {
	return fmt.Errorf("%w (looked for %q): install it with your package manager — "+
		"it is in gstreamer1.0-tools on Debian/Ubuntu, gstreamer on Arch, gstreamer1-tools on Fedora",
		ErrNoLaunchBinary, binary)
}

// EnsureBinary checks the GStreamer CLI is present *before* anything else
// happens.
//
// The ordering matters: this runs before the portal handshake, so a machine
// without GStreamer never gets asked to share its screen and only then told it
// cannot. It is the same instinct as connect-before-picker (Decision 10) — do
// not ask the user for something you cannot use.
func EnsureBinary(binary string) error {
	if strings.ContainsRune(binary, os.PathSeparator) {
		if _, err := os.Stat(binary); err != nil {
			return missingBinaryErr(binary)
		}
		return nil
	}
	if _, err := exec.LookPath(binary); err != nil {
		return missingBinaryErr(binary)
	}
	return nil
}

// child is one supervised gst-launch-1.0 process.
type child struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	log    *slog.Logger

	mu     sync.Mutex
	stderr []string

	waitOnce sync.Once
	waitErr  error
	done     chan struct{}
}

// startChild spawns gst-launch-1.0 with args, passing extraFD (the portal's
// PipeWire fd) as the child's fd 3.
func startChild(ctx context.Context, binary string, args []string, extraFD *os.File, log *slog.Logger) (*child, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	if extraFD != nil {
		// ExtraFiles[0] becomes fd 3 in the child — what the pipeline's
		// `pipewiresrc fd=3` reads.
		cmd.ExtraFiles = []*os.File{extraFD}
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		if missingBinary(err) {
			return nil, missingBinaryErr(binary)
		}
		return nil, err
	}

	c := &child{cmd: cmd, stdout: stdout, log: log, done: make(chan struct{})}
	go c.drainStderr(stderr)
	log.Debug("gst child started", "args", strings.Join(args, " "))
	return c, nil
}

// drainStderr keeps the tail of the child's stderr. It must run: a child whose
// stderr pipe fills would block forever, which is the silent hang this whole
// package is written to avoid.
func (c *child) drainStderr(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		c.log.Debug("gst", "stderr", line)
		c.mu.Lock()
		c.stderr = append(c.stderr, line)
		if len(c.stderr) > stderrKeepLines {
			c.stderr = c.stderr[1:]
		}
		c.mu.Unlock()
	}
}

// stderrTail returns the retained stderr lines.
func (c *child) stderrTail() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.stderr, "\n")
}

// wait reaps the child. A non-zero exit carries the stderr tail, so the error
// says what GStreamer actually complained about.
func (c *child) wait() error {
	c.waitOnce.Do(func() {
		err := c.cmd.Wait()
		if err != nil {
			if tail := c.stderrTail(); tail != "" {
				err = fmt.Errorf("%s exited: %w\n%s", LaunchBinary, err, tail)
			} else {
				err = fmt.Errorf("%s exited: %w", LaunchBinary, err)
			}
		}
		c.waitErr = err
		close(c.done)
	})
	<-c.done
	return c.waitErr
}

// stop terminates the child and reaps it. Killing is fine: gst-launch has no
// state worth flushing, the relay session is ours to close, and a capture that
// lingers after Stop is a screen still being shared.
func (c *child) stop() {
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.wait()
}

// runToCompletion runs a child that is expected to exit on its own (a trial).
func runToCompletion(ctx context.Context, binary string, args []string, log *slog.Logger) error {
	c, err := startChild(ctx, binary, args, nil, log)
	if err != nil {
		return err
	}
	// A trial writes to fakesink, so stdout carries nothing; drain it anyway
	// rather than risk a full pipe.
	go func() { _, _ = io.Copy(io.Discard, c.stdout) }()
	return c.wait()
}
