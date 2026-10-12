// Package nativetest finds the native dependencies the sampler's real-binary
// tests need — the minimal ffmpeg (deploy/build-ffmpeg.sh) and the ONNX
// Runtime shared library — through the same environment variables the
// service reads, GAWK_SAMPLER_FFMPEG and GAWK_SAMPLER_ONNXRUNTIME_LIB.
//
// A machine without them skips those tests, unless GAWK_SAMPLER_REQUIRE_NATIVE
// is set, which turns the skip into a failure: CI sets it, so a broken
// toolchain fails red instead of passing on skips (the GAWK_REQUIRE_GST
// pattern of the desktop workspace).
package nativetest

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// Required reports whether the native tests must run.
func Required() bool { return os.Getenv("GAWK_SAMPLER_REQUIRE_NATIVE") != "" }

func skipOrFail(t testing.TB, msg string) {
	t.Helper()
	if Required() {
		t.Fatalf("GAWK_SAMPLER_REQUIRE_NATIVE is set but %s", msg)
	}
	t.Skip(msg)
}

// FFmpeg returns the absolute path of the ffmpeg to test with.
func FFmpeg(t testing.TB) string {
	t.Helper()
	p := os.Getenv("GAWK_SAMPLER_FFMPEG")
	if p == "" {
		skipOrFail(t, "GAWK_SAMPLER_FFMPEG is not set")
	}
	abs, err := exec.LookPath(p)
	if err != nil {
		skipOrFail(t, "GAWK_SAMPLER_FFMPEG="+p+" is not runnable: "+err.Error())
	}
	abs, _ = filepath.Abs(abs)
	return abs
}

// ONNXRuntime returns the ONNX Runtime shared library to test with. It also
// skips (or fails) in a build without cgo, where the library cannot be used.
func ONNXRuntime(t testing.TB) string {
	t.Helper()
	if !cgoEnabled {
		skipOrFail(t, "the test binary was built without cgo")
	}
	p := os.Getenv("GAWK_SAMPLER_ONNXRUNTIME_LIB")
	if p == "" {
		skipOrFail(t, "GAWK_SAMPLER_ONNXRUNTIME_LIB is not set")
	}
	if _, err := os.Stat(p); err != nil {
		skipOrFail(t, "GAWK_SAMPLER_ONNXRUNTIME_LIB="+p+": "+err.Error())
	}
	return p
}

// Testdata is the module's testdata directory, from any package under it.
func Testdata(t testing.TB) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the module")
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "testdata")
}
