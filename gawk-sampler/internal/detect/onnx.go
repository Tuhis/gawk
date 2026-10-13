//go:build cgo

package detect

import (
	"errors"
	"fmt"
	"os"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
)

// Inference runs in-process (docs/71 D4): ONNX Runtime only ever sees a
// fixed-shape float tensor this package built from an RGB frame ffmpeg
// already decoded, so a malformed bitstream cannot reach it.

var (
	envOnce sync.Once
	envErr  error
	envLib  string
)

// Init loads the ONNX Runtime shared library at lib and initializes its
// environment. It may be called more than once with the same path; the first
// call does the work.
//
// It turns ONNX Runtime's own telemetry off first, whatever the environment
// said: the official Linux builds report to Microsoft by default unless
// ORT_DISABLE_TELEMETRY=1 is set when the library initializes, and a
// moderation sidecar does not phone home. It is not only a privacy setting:
// 1.29.0 segfaults starting that telemetry in a shell-less image with no
// /etc/machine-id — the distroless runtime (microsoft/onnxruntime#32173).
// os.Setenv reaches the C environment the library's getenv reads, since
// this file is cgo.
func Init(lib string) error {
	envOnce.Do(func() {
		envLib = lib
		if envErr = os.Setenv("ORT_DISABLE_TELEMETRY", "1"); envErr != nil {
			return
		}
		ort.SetSharedLibraryPath(lib)
		envErr = ort.InitializeEnvironment(ort.WithLogLevelError())
	})
	if envErr != nil {
		return fmt.Errorf("detect: onnxruntime %s: %w", envLib, envErr)
	}
	if lib != envLib {
		return fmt.Errorf("detect: onnxruntime already loaded from %s", envLib)
	}
	return nil
}

// Detector is one loaded model at one input size. Detect is safe for
// concurrent use: ONNX Runtime's Run is, and every call brings its own
// tensors.
type Detector struct {
	session *ort.DynamicAdvancedSession
	size    int
	anchors int
}

// Load opens the model at path for size×size input. It refuses a model whose
// input is not [1,3,size,size] (a dynamic dimension is fine) or whose output
// is not [1, 4+NumLabels, anchors]: loading NudeNet's 640m model with a
// 320 input, or a model of some other vocabulary, fails here rather than
// scoring garbage.
func Load(path string, size int) (*Detector, error) {
	if !ort.IsInitialized() {
		return nil, errors.New("detect: Init was not called")
	}
	inputs, outputs, err := ort.GetInputOutputInfo(path)
	if err != nil {
		return nil, fmt.Errorf("detect: read model %s: %w", path, err)
	}
	if len(inputs) != 1 || len(outputs) != 1 {
		return nil, fmt.Errorf("detect: model has %d inputs and %d outputs, want 1 and 1", len(inputs), len(outputs))
	}
	in, out := inputs[0], outputs[0]
	if in.DataType != ort.TensorElementDataTypeFloat || out.DataType != ort.TensorElementDataTypeFloat {
		return nil, errors.New("detect: model input and output must be float32 tensors")
	}
	if !dimsMatch(in.Dimensions, []int64{1, 3, int64(size), int64(size)}) {
		return nil, fmt.Errorf("detect: model input %v does not take [1 3 %d %d]", in.Dimensions, size, size)
	}
	anchors := Anchors(size)
	if !dimsMatch(out.Dimensions, []int64{1, int64(4 + NumLabels), int64(anchors)}) {
		return nil, fmt.Errorf("detect: model output %v is not [1 %d %d] at input %d", out.Dimensions, 4+NumLabels, anchors, size)
	}

	opts, err := ort.NewSessionOptions()
	if err != nil {
		return nil, fmt.Errorf("detect: session options: %w", err)
	}
	defer opts.Destroy()
	// One thread per inference: the sampler runs -workers inferences side by
	// side, and letting each one fan out across every core would oversubscribe
	// the CPU the worker pool is already sized to.
	if err := opts.SetIntraOpNumThreads(1); err != nil {
		return nil, err
	}
	if err := opts.SetInterOpNumThreads(1); err != nil {
		return nil, err
	}
	s, err := ort.NewDynamicAdvancedSession(path, []string{in.Name}, []string{out.Name}, opts)
	if err != nil {
		return nil, fmt.Errorf("detect: load model %s: %w", path, err)
	}
	return &Detector{session: s, size: size, anchors: anchors}, nil
}

// dimsMatch compares a model's declared dimensions with want; a negative
// (symbolic) dimension matches anything.
func dimsMatch(got ort.Shape, want []int64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range want {
		if got[i] >= 0 && got[i] != want[i] {
			return false
		}
	}
	return true
}

// Size is the square input size the detector was loaded for.
func (d *Detector) Size() int { return d.size }

// Detect runs the model on a size×size RGB24 frame and post-processes the
// output, clipping boxes to clip.
func (d *Detector) Detect(rgb []byte, clip Box) ([]Detection, error) {
	data, err := Tensor(rgb, d.size)
	if err != nil {
		return nil, err
	}
	in, err := ort.NewTensor(ort.NewShape(1, 3, int64(d.size), int64(d.size)), data)
	if err != nil {
		return nil, fmt.Errorf("detect: input tensor: %w", err)
	}
	defer in.Destroy()
	out, err := ort.NewEmptyTensor[float32](ort.NewShape(1, int64(4+NumLabels), int64(d.anchors)))
	if err != nil {
		return nil, fmt.Errorf("detect: output tensor: %w", err)
	}
	defer out.Destroy()
	if err := d.session.Run([]ort.Value{in}, []ort.Value{out}); err != nil {
		return nil, fmt.Errorf("detect: inference: %w", err)
	}
	return Decode(out.GetData(), d.anchors, clip)
}

// Close releases the session.
func (d *Detector) Close() error {
	return d.session.Destroy()
}
