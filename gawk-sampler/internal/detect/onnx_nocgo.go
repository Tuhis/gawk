//go:build !cgo

package detect

import "errors"

// ErrNoCgo is what every inference entry point returns in a build without
// cgo: ONNX Runtime is reached through cgo, so such a binary can run the
// pure-Go parts (and their tests) but never score a frame. The shipped image
// is built with CGO_ENABLED=1.
var ErrNoCgo = errors.New("detect: built without cgo; ONNX Runtime is unavailable")

// Init always fails without cgo.
func Init(string) error { return ErrNoCgo }

// Detector is uninhabited without cgo.
type Detector struct{ size int }

// Load always fails without cgo.
func Load(string, int) (*Detector, error) { return nil, ErrNoCgo }

// Size is the square input size the detector was loaded for.
func (d *Detector) Size() int { return d.size }

// Detect always fails without cgo.
func (d *Detector) Detect([]byte, Box) ([]Detection, error) { return nil, ErrNoCgo }

// Close does nothing without cgo.
func (d *Detector) Close() error { return nil }
