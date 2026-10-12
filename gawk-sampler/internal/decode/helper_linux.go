//go:build linux

package decode

import (
	"errors"
	"fmt"
	"strconv"
	"syscall"
)

// ExecHelper is the HelperCommand: args are `-as <bytes> -- <program>
// <args…>`. It sets RLIMIT_CORE to 0 and RLIMIT_AS to <bytes> on this
// process and then replaces it with program, which inherits both. It returns
// only on failure; the caller exits non-zero with the error.
//
// Nothing may allocate much between the second setrlimit and the exec: the
// limit applies to this Go process first, and the runtime has already
// reserved what it needs.
func ExecHelper(args []string) error {
	if len(args) < 4 || args[0] != "-as" || args[2] != "--" {
		return errors.New("usage: ffmpeg-exec -as <bytes> -- <program> [args…]")
	}
	limit, err := strconv.ParseUint(args[1], 10, 64)
	if err != nil || limit == 0 {
		return fmt.Errorf("ffmpeg-exec: invalid -as %q", args[1])
	}
	prog, argv := args[3], args[3:]
	if err := syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{Cur: 0, Max: 0}); err != nil {
		return fmt.Errorf("ffmpeg-exec: RLIMIT_CORE: %w", err)
	}
	if err := syscall.Setrlimit(syscall.RLIMIT_AS, &syscall.Rlimit{Cur: limit, Max: limit}); err != nil {
		return fmt.Errorf("ffmpeg-exec: RLIMIT_AS: %w", err)
	}
	return syscall.Exec(prog, argv, []string{})
}
