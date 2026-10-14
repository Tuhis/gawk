package detect

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/Tuhis/gawk/gawk-sampler/internal/nativetest"
)

func loadTestModel(t *testing.T, size int) *Detector {
	t.Helper()
	if err := Init(nativetest.ONNXRuntime(t)); err != nil {
		t.Fatal(err)
	}
	d, err := Load(filepath.Join(nativetest.Testdata(t), "models", "test-"+strconv.Itoa(size)+".onnx"), size)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// solid is a size×size RGB24 frame whose top rows (a 16:9 picture) are c and
// whose rest is the black letterbox.
func solid(size int, c [3]byte) []byte {
	out := make([]byte, size*size*3)
	_, h := Fit(16, 9, size)
	for i := 0; i < size*h; i++ {
		copy(out[3*i:], c[:])
	}
	return out
}

// CF5: both input sizes load and score, through ONNX Runtime and the real
// post-processing: the test model scores a letterboxed magenta frame ≈1 on
// FEMALE_GENITALIA_EXPOSED and black 0.
func TestDetectBothInputSizes(t *testing.T) {
	for _, size := range []int{320, 640} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			d := loadTestModel(t, size)
			if d.Size() != size {
				t.Fatalf("Size() = %d", d.Size())
			}
			full := Box{W: float64(size), H: float64(size)}
			dets, err := d.Detect(solid(size, [3]byte{255, 0, 255}), full)
			if err != nil {
				t.Fatal(err)
			}
			scores := LabelScores(dets, []string{"FEMALE_GENITALIA_EXPOSED", "ANUS_EXPOSED"})
			if scores["FEMALE_GENITALIA_EXPOSED"] < 0.99 || scores["ANUS_EXPOSED"] != 0 {
				t.Fatalf("magenta scored %v", scores)
			}
			dets, err = d.Detect(make([]byte, size*size*3), full)
			if err != nil {
				t.Fatal(err)
			}
			if len(dets) != 0 {
				t.Fatalf("black frame detected %+v", dets)
			}
			if _, err := d.Detect(make([]byte, 10), full); err == nil {
				t.Fatal("a wrong-size frame was scored")
			}
		})
	}
}

// ONNX Runtime's official Linux builds report telemetry to Microsoft unless
// ORT_DISABLE_TELEMETRY=1 is in the environment when the library
// initializes, and 1.29.0 segfaults starting that telemetry on a shell-less
// image — the sampler's distroless runtime (microsoft/onnxruntime#32173).
// Init sets it; deploy/Dockerfile's runtime-check stage is where the crash
// itself would show.
func TestInitTurnsONNXRuntimeTelemetryOff(t *testing.T) {
	loadTestModel(t, 320)
	if got := os.Getenv("ORT_DISABLE_TELEMETRY"); got != "1" {
		t.Fatalf("ORT_DISABLE_TELEMETRY = %q after Init, want \"1\"", got)
	}
}

// A model is checked against the input size it is loaded for: the 640 test
// model is not a 320 model.
func TestLoadRefusesTheWrongInputSize(t *testing.T) {
	if err := Init(nativetest.ONNXRuntime(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(filepath.Join(nativetest.Testdata(t), "models", "test-640.onnx"), 320); err == nil {
		t.Fatal("the 640 model loaded at 320")
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.onnx"), 320); err == nil {
		t.Fatal("a missing model loaded")
	}
}

// Detect is called from every worker at once.
func TestDetectConcurrently(t *testing.T) {
	d := loadTestModel(t, 320)
	frame := solid(320, [3]byte{255, 0, 255})
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		go func() {
			for j := 0; j < 10; j++ {
				dets, err := d.Detect(frame, Box{W: 320, H: 320})
				if err == nil && len(dets) != 1 {
					err = errUnexpected(len(dets))
				}
				if err != nil {
					errs <- err
					return
				}
			}
			errs <- nil
		}()
	}
	for i := 0; i < 8; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

type errUnexpected int

func (e errUnexpected) Error() string { return strconv.Itoa(int(e)) + " detections, want 1" }
