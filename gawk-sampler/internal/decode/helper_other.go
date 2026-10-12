//go:build !linux

package decode

import "errors"

// ExecHelper needs Linux's setrlimit and exec; the sampler ships only for
// Linux.
func ExecHelper([]string) error {
	return errors.New("ffmpeg-exec: only supported on Linux")
}
