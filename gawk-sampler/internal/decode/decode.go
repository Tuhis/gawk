// Package decode turns one keyframe into one model-ready RGB frame through an
// ffmpeg subprocess (docs/71 D4).
//
// A subprocess per sample is the crash isolation the design rests on: a
// malformed or hostile bitstream can take down one ffmpeg, never the sampler,
// and a decoder CVE never shares an address space with it. Every process
// runs with a wall-clock timeout, an RLIMIT_AS cap and RLIMIT_CORE = 0 — a
// core dump would write a frame to disk, which the evidence rule (D8) forbids.
// The limits are set by re-executing the sampler's own binary as a tiny
// helper (ExecHelper) that sets them on itself and then execs ffmpeg, so the
// image needs no shell and no prlimit.
package decode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/Tuhis/gawk/gawk-sampler/internal/bitstream"
)

// HelperCommand is the hidden subcommand the sampler binary answers by
// becoming ffmpeg under limits (see ExecHelper).
const HelperCommand = "ffmpeg-exec"

// ErrTimeout is a decode that ran past its wall-clock limit and was killed.
var ErrTimeout = errors.New("decode: timed out")

// ErrFailed is a decode that ffmpeg refused or that produced no frame.
var ErrFailed = errors.New("decode: failed")

// Decoder runs ffmpeg.
type Decoder struct {
	// FFmpeg is the ffmpeg executable (absolute, or found on PATH).
	FFmpeg string
	// Size is the model's square input; Decode returns Size·Size·3 bytes.
	Size int
	// Timeout is the per-sample wall clock (-decode-timeout).
	Timeout time.Duration
	// MemoryLimit is RLIMIT_AS for the subprocess, in bytes.
	MemoryLimit int64
	// Helper is the binary that answers HelperCommand; empty means this
	// process's own executable. Tests point it at the test binary.
	Helper string
	// HelperEnv is extra environment for the helper process (tests only).
	HelperEnv []string
}

// Args is the ffmpeg command line for one keyframe from a pipe: decode the
// first frame on one thread, scale it to fit the model's square input keeping
// its aspect ratio, convert to RGB, pad black on the right and bottom
// (§9.5), and write exactly one raw RGB24 frame to stdout.
//
// -xerror makes a frame the decoder had to conceal a failure: a truncated
// keyframe would otherwise come back as a smeared picture and be scored as if
// it were the broadcast. A failed decode is no sample at all (D20), which is
// the honest answer.
func Args(demuxer string, size int) []string {
	s := strconv.Itoa(size)
	vf := "scale=w=" + s + ":h=" + s + ":force_original_aspect_ratio=decrease:flags=bilinear," +
		"format=rgb24,pad=" + s + ":" + s + ":0:0:color=black"
	return []string{
		"-nostdin", "-hide_banner", "-loglevel", "error", "-xerror",
		"-threads", "1", "-filter_threads", "1",
		"-f", demuxer, "-i", "pipe:0",
		"-an", "-sn", "-dn",
		"-frames:v", "1",
		"-vf", vf,
		"-f", "rawvideo", "-pix_fmt", "rgb24", "pipe:1",
	}
}

// Decode runs one keyframe through ffmpeg and returns the letterboxed RGB24
// frame. The returned bytes are frame data: never logged, written or exposed
// (D8). Errors carry no frame bytes either — ffmpeg's own diagnostics are
// text, and only their last line, truncated, is kept.
func (d *Decoder) Decode(ctx context.Context, in bitstream.Input) ([]byte, error) {
	helper := d.Helper
	if helper == "" {
		exe, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("decode: locate own executable: %w", err)
		}
		helper = exe
	}
	ffmpeg, err := exec.LookPath(d.FFmpeg)
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, d.Timeout)
	defer cancel()
	args := append([]string{HelperCommand, "-as", strconv.FormatInt(d.MemoryLimit, 10), "--", ffmpeg}, Args(in.Demuxer, d.Size)...)
	cmd := exec.CommandContext(ctx, helper, args...)
	// Nothing of the sampler's environment reaches ffmpeg: it needs none, and
	// the environment holds the screening token and the OIDC secret.
	cmd.Env = append([]string{}, d.HelperEnv...)
	cmd.Stdin = bytes.NewReader(in.Data)
	want := d.Size * d.Size * 3
	out := &limitedBuffer{limit: want + 1}
	cmd.Stdout = out
	stderr := &limitedBuffer{limit: 4096}
	cmd.Stderr = stderr
	// A killed ffmpeg's pipes close with it; WaitDelay bounds the wait if
	// something inherited them.
	cmd.WaitDelay = time.Second

	runErr := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("%w after %s", ErrTimeout, d.Timeout)
	}
	if runErr != nil {
		return nil, fmt.Errorf("%w: %v: %s", ErrFailed, runErr, lastLine(stderr.Bytes()))
	}
	if out.Len() != want {
		return nil, fmt.Errorf("%w: ffmpeg wrote %d bytes, want %d", ErrFailed, out.Len(), want)
	}
	return out.Bytes(), nil
}

// limitedBuffer keeps at most limit bytes and silently drops the rest, so a
// misbehaving subprocess cannot make the sampler buffer without bound.
type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.Len(); room > 0 {
		if len(p) > room {
			b.Buffer.Write(p[:room])
		} else {
			b.Buffer.Write(p)
		}
	}
	return len(p), nil
}

var _ io.Writer = (*limitedBuffer)(nil)

func lastLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
