//go:build linux

package decode

import (
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"
	"strconv"
	"syscall"
	"unsafe"
)

// ExecHelper is the HelperCommand: args are `-as <bytes> -- <program>
// <args…>`. It sets RLIMIT_CORE to 0 and RLIMIT_AS to <bytes> on this
// process and then replaces it with program, which inherits both. It returns
// only on failure; the caller exits non-zero with the error.
//
// RLIMIT_AS applies to this Go process before the exec, and the runtime is
// usually already over it, so any mmap between the limit and the exec is
// fatal ("fatal error: runtime: cannot allocate memory"). Not allocating in
// this function is not enough: other goroutines run on other threads (CI
// caught the finalizer goroutine growing its stack in that window), a
// blocking syscall.Syscall lets sysmon hand the P to a new thread whose
// stack is an mmap, and a GC cycle grows its own structures. So everything
// execve needs is built first, then GOMAXPROCS(1) and GC off leave this
// goroutine the only one that can run, and the limit and the exec are raw
// syscalls, which keep the P. (syscall.Exec would also restore Go's
// original soft RLIMIT_NOFILE for the program; the raw execve passes on the
// raised soft limit, which ffmpeg reading one pipe does not care about.)
func ExecHelper(args []string) error {
	if len(args) < 4 || args[0] != "-as" || args[2] != "--" {
		return errors.New("usage: ffmpeg-exec -as <bytes> -- <program> [args…]")
	}
	limit, err := strconv.ParseUint(args[1], 10, 64)
	if err != nil || limit == 0 {
		return fmt.Errorf("ffmpeg-exec: invalid -as %q", args[1])
	}
	prog, err := syscall.BytePtrFromString(args[3])
	if err != nil {
		return fmt.Errorf("ffmpeg-exec: %w", err)
	}
	argv, err := syscall.SlicePtrFromStrings(args[3:])
	if err != nil {
		return fmt.Errorf("ffmpeg-exec: %w", err)
	}
	envv := []*byte{nil}
	if err := syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{Cur: 0, Max: 0}); err != nil {
		return fmt.Errorf("ffmpeg-exec: RLIMIT_CORE: %w", err)
	}
	as := syscall.Rlimit{Cur: limit, Max: limit}

	runtime.LockOSThread()
	runtime.GOMAXPROCS(1)
	debug.SetGCPercent(-1) // waits out a running cycle and starts no new one
	if _, _, e := syscall.RawSyscall6(syscall.SYS_PRLIMIT64, 0, syscall.RLIMIT_AS, uintptr(unsafe.Pointer(&as)), 0, 0, 0); e != 0 {
		return fmt.Errorf("ffmpeg-exec: RLIMIT_AS: %w", e)
	}
	_, _, e := syscall.RawSyscall(syscall.SYS_EXECVE,
		uintptr(unsafe.Pointer(prog)),
		uintptr(unsafe.Pointer(&argv[0])),
		uintptr(unsafe.Pointer(&envv[0])))
	runtime.KeepAlive(prog)
	runtime.KeepAlive(argv)
	runtime.KeepAlive(envv)
	return fmt.Errorf("ffmpeg-exec: exec %s: %w", args[3], e)
}
