package decode

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Tuhis/gawk/gawk-sampler/internal/bitstream"
	"github.com/Tuhis/gawk/gawk-sampler/internal/bitstream/bitstreamtest"
	"github.com/Tuhis/gawk/gawk-sampler/internal/nativetest"
)

// helperEnv makes the test binary answer HelperCommand the way the sampler
// binary does (cmd/gawk-sampler dispatches it before anything else).
const helperEnv = "GAWK_SAMPLER_TEST_EXEC_HELPER"

func TestMain(m *testing.M) {
	if os.Getenv(helperEnv) == "1" && len(os.Args) > 1 && os.Args[1] == HelperCommand {
		err := ExecHelper(os.Args[2:])
		os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(2)
	}
	os.Exit(m.Run())
}

func testDecoder(t *testing.T, ffmpeg string) *Decoder {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return &Decoder{
		FFmpeg:      ffmpeg,
		Size:        320,
		Timeout:     5 * time.Second,
		MemoryLimit: 1 << 30,
		Helper:      exe,
		HelperEnv:   []string{helperEnv + "=1"},
	}
}

// D4: the subprocess runs with RLIMIT_CORE = 0 and the configured RLIMIT_AS.
// The helper execs a shell that reports its own limits instead of ffmpeg.
func TestHelperSetsCoreAndAddressSpaceLimits(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	exe, _ := os.Executable()
	cmd := exec.Command(exe, HelperCommand, "-as", "536870912", "--", sh, "-c", "ulimit -c; ulimit -v")
	cmd.Env = []string{helperEnv + "=1"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper: %v: %s", err, out)
	}
	if got := strings.Fields(string(out)); len(got) != 2 || got[0] != "0" || got[1] != "524288" {
		t.Fatalf("limits %q, want core 0 and 524288 KiB of address space", out)
	}
}

func TestHelperRejectsBadArgs(t *testing.T) {
	for _, args := range [][]string{nil, {"-as", "x", "--", "sh"}, {"-as", "0", "--", "sh"}, {"-as", "1", "sh"}} {
		if err := ExecHelper(args); err == nil {
			t.Fatalf("ExecHelper(%q) succeeded", args)
		}
	}
}

// CF5: a decoder that hangs is killed at -decode-timeout.
func TestDecodeKillsAHangingDecoder(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := testDecoder(t, fake)
	d.Timeout = 300 * time.Millisecond
	start := time.Now()
	_, err := d.Decode(context.Background(), bitstream.Input{Demuxer: "h264", Data: []byte{0, 0, 0, 1}})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Fatalf("a hung decoder held the sample for %s", took)
	}
}

func TestDecodeReportsAFailingDecoder(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho 'Invalid data found when processing input' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := testDecoder(t, fake).Decode(context.Background(), bitstream.Input{Demuxer: "h264", Data: []byte{1}})
	if !errors.Is(err, ErrFailed) || !strings.Contains(err.Error(), "Invalid data") {
		t.Fatalf("err = %v, want ErrFailed with ffmpeg's message", err)
	}
	// A decoder that exits cleanly but writes a short frame is a failure too.
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf abc\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := testDecoder(t, fake).Decode(context.Background(), bitstream.Input{Demuxer: "h264", Data: []byte{1}}); !errors.Is(err, ErrFailed) {
		t.Fatalf("short frame: err = %v, want ErrFailed", err)
	}
}

// The environment — which holds the screening token and the OIDC secret —
// never reaches the decoder.
func TestDecodeGivesTheDecoderNoEnvironment(t *testing.T) {
	t.Setenv("GAWK_SAMPLER_SCREENING_TOKEN", "secret-token")
	dir := t.TempDir()
	fake := filepath.Join(dir, "ffmpeg")
	envOut := filepath.Join(dir, "env")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nenv > "+envOut+"\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, _ = testDecoder(t, fake).Decode(context.Background(), bitstream.Input{Demuxer: "h264", Data: []byte{1}})
	env, err := os.ReadFile(envOut)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(env), "secret-token") || strings.Contains(string(env), helperEnv) {
		t.Fatalf("decoder saw the sampler's environment: %s", env)
	}
}

type rgb [3]int

func pixel(frame []byte, size, x, y int) rgb {
	o := (y*size + x) * 3
	return rgb{int(frame[o]), int(frame[o+1]), int(frame[o+2])}
}

func near(a, b rgb, tol int) bool {
	for i := range a {
		if d := a[i] - b[i]; d > tol || d < -tol {
			return false
		}
	}
	return true
}

// CF5: AVCC → Annex B and the IVF wrapping decode to the expected frame with
// the real ffmpeg — every broadcaster shape, letterboxed per §9.5.
func TestDecodeEveryBroadcasterShape(t *testing.T) {
	d := testDecoder(t, nativetest.FFmpeg(t))
	td := nativetest.Testdata(t)
	for _, s := range bitstreamtest.Shapes(td) {
		t.Run(s.Name, func(t *testing.T) {
			in, err := bitstream.FromStreamFrame(s.Msg, s.Codec)
			if err != nil {
				t.Fatal(err)
			}
			frame, err := d.Decode(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			// 160×96 fits 320×320 as 320×192: four quadrants, then black.
			for _, c := range []struct {
				x, y int
				want rgb
				name string
			}{
				{80, 48, rgb{255, 0, 0}, "red top-left"},
				{240, 48, rgb{0, 255, 0}, "green top-right"},
				{80, 144, rgb{0, 0, 255}, "blue bottom-left"},
				{240, 144, rgb{255, 255, 255}, "white bottom-right"},
				{160, 260, rgb{0, 0, 0}, "padding"},
				{319, 319, rgb{0, 0, 0}, "padding corner"},
			} {
				if got := pixel(frame, 320, c.x, c.y); !near(got, c.want, 40) {
					t.Errorf("%s at (%d,%d) = %v, want ≈%v", c.name, c.x, c.y, got, c.want)
				}
			}
		})
	}
}

func TestDecodeMagentaAt640(t *testing.T) {
	d := testDecoder(t, nativetest.FFmpeg(t))
	d.Size = 640
	in, err := bitstream.FromStreamFrame(bitstreamtest.StreamFrame(1, "", nil, bitstreamtest.Fixture(nativetest.Testdata(t), "magenta.h264")), "h264")
	if err != nil {
		t.Fatal(err)
	}
	frame, err := d.Decode(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(frame) != 640*640*3 {
		t.Fatalf("%d bytes", len(frame))
	}
	// 160×90 fits 640×640 as 640×360.
	if got := pixel(frame, 640, 320, 180); !near(got, rgb{255, 0, 255}, 40) {
		t.Fatalf("content %v, want magenta", got)
	}
	if got := pixel(frame, 640, 320, 500); got != (rgb{0, 0, 0}) {
		t.Fatalf("padding %v, want black", got)
	}
}

// CF5: a truncated or garbage bitstream fails that one sample — ffmpeg's
// failure, not the sampler's — and the next sample decodes.
func TestDecodeSurvivesGarbage(t *testing.T) {
	d := testDecoder(t, nativetest.FFmpeg(t))
	td := nativetest.Testdata(t)
	h264 := bitstreamtest.Fixture(td, "quadrants.h264")
	bad := []bitstream.Input{
		{Codec: "h264", Demuxer: "h264", Data: append([]byte{0, 0, 0, 1}, []byte("this is not h264 at all, not even close")...)},
		{Codec: "h264", Demuxer: "h264", Data: h264[:len(h264)/2]},
		{Codec: "vp9", Demuxer: "ivf", Data: bitstream.IVF("vp9", []byte{0x82, 0x49, 0x83, 0x42, 0x00})},
		{Codec: "vp8", Demuxer: "ivf", Data: bitstream.IVF("vp8", nil)},
	}
	for i, in := range bad {
		if _, err := d.Decode(context.Background(), in); !errors.Is(err, ErrFailed) {
			t.Errorf("bad input %d: err = %v, want ErrFailed", i, err)
		}
	}
	if _, err := d.Decode(context.Background(), bitstream.Input{Codec: "h264", Demuxer: "h264", Data: h264}); err != nil {
		t.Fatalf("the next sample failed: %v", err)
	}
}

// RLIMIT_AS reaches ffmpeg: under a few megabytes it cannot even start.
func TestDecodeMemoryLimitApplies(t *testing.T) {
	d := testDecoder(t, nativetest.FFmpeg(t))
	d.MemoryLimit = 4 << 20
	in := bitstream.Input{Codec: "h264", Demuxer: "h264", Data: bitstreamtest.Fixture(nativetest.Testdata(t), "quadrants.h264")}
	if _, err := d.Decode(context.Background(), in); !errors.Is(err, ErrFailed) {
		t.Fatalf("err = %v, want ErrFailed under a 4 MiB address-space limit", err)
	}
}
