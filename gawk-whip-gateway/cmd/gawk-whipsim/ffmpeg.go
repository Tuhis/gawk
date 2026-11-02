package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"github.com/Tuhis/gawk/gawk-whip-gateway/internal/streamkey"
)

// errNoFFmpeg: the FFmpeg binary is missing or not executable, which no
// restart fixes.
var errNoFFmpeg = errors.New("ffmpeg cannot be started")

// ffmpegArgs is the WHIP sender's command line: what docs/obs-setup.md asks
// OBS for, with FFmpeg's names. A moving test pattern (testsrc2 carries a
// frame counter and a clock), x264 with a fixed keyframe interval (-g and
// -keyint_min equal, scene-cut off, so every IDR is on the interval), no
// B-frames, SPS/PPS repeated before every IDR, and an Opus tone.
func ffmpegArgs(o options, key streamkey.Key) []string {
	gopFrames := int(math.Round(o.gop.Seconds() * float64(o.fps)))
	if gopFrames < 1 {
		gopFrames = 1
	}
	rate := strconv.Itoa(o.bitrateKbps) + "k"
	args := []string{
		"-hide_banner", "-nostats", "-loglevel", "warning",
		"-re", "-f", "lavfi", "-i", fmt.Sprintf("testsrc2=size=%s:rate=%d", o.size, o.fps),
	}
	if o.audio {
		args = append(args, "-re", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000")
	}
	args = append(args,
		"-c:v", "libx264", "-preset", "veryfast", "-tune", "zerolatency",
		"-profile:v", "high", "-pix_fmt", "yuv420p",
		"-g", strconv.Itoa(gopFrames), "-keyint_min", strconv.Itoa(gopFrames), "-sc_threshold", "0",
		"-bf", "0", "-x264-params", "repeat-headers=1",
		"-b:v", rate, "-maxrate", rate, "-bufsize", strconv.Itoa(o.bitrateKbps/2)+"k",
	)
	if o.audio {
		args = append(args, "-c:a", "libopus", "-b:a", "64k", "-ar", "48000", "-ac", "2")
	}
	return append(args, "-f", "whip", "-authorization", key.String(), o.whipURL)
}

// ffmpegSender runs FFmpeg once per run call.
type ffmpegSender struct {
	bin  string
	args []string
	log  *slog.Logger
}

// run starts FFmpeg and waits for it. Ending ctx sends SIGINT, which lets
// FFmpeg's WHIP muxer write its trailer (the DELETE, a stop: close 4014),
// and kills it if it is still running 5 s later.
func (s ffmpegSender) run(ctx context.Context) error {
	cmd := exec.Command(s.bin, s.args...)
	cmd.Stdout = logWriter{s.log, "ffmpeg"}
	cmd.Stderr = logWriter{s.log, "ffmpeg"}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("%w: %v", errNoFFmpeg, err)
	}
	s.log.Info("ffmpeg started", "pid", cmd.Process.Pid)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = cmd.Process.Signal(syscall.SIGINT)
		select {
		case err := <-done:
			return err
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			return <-done
		}
	}
}

// logWriter turns FFmpeg's output into log lines.
type logWriter struct {
	log  *slog.Logger
	from string
}

func (w logWriter) Write(p []byte) (int, error) {
	for _, line := range splitLines(p) {
		w.log.Info(line, "from", w.from)
	}
	return len(p), nil
}

func splitLines(p []byte) []string {
	var out []string
	start := 0
	for i, b := range p {
		if b == '\n' || b == '\r' {
			if i > start {
				out = append(out, string(p[start:i]))
			}
			start = i + 1
		}
	}
	if start < len(p) {
		out = append(out, string(p[start:]))
	}
	return out
}
