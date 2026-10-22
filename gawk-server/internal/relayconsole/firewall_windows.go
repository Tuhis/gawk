//go:build windows

package relayconsole

// The Windows half of the firewall helper: elevation through UAC and the
// rule check. Through syscall rather than golang.org/x/sys, which
// gawk-server does not require directly (relaystate's DPAPI does the same).
//
// Never run on Windows by an agent: PENDING the PP15 party pass
// (docs/76 §12), as is the Public-profile behaviour itself (PP0).

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// FirewallSupported reports that this build can add the rule.
const FirewallSupported = true

var (
	shell32              = syscall.NewLazyDLL("shell32.dll")
	procShellExecuteExW  = shell32.NewProc("ShellExecuteExW")
	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	procGetExitCodeProc  = kernel32.NewProc("GetExitCodeProcess")
	errElevationDeclined = errors.New("relayconsole: the administrator prompt was declined")
)

// shellExecuteInfo is SHELLEXECUTEINFOW; Go's natural alignment matches
// the C layout on both 386 and amd64/arm64.
type shellExecuteInfo struct {
	cbSize         uint32
	fMask          uint32
	hwnd           uintptr
	lpVerb         *uint16
	lpFile         *uint16
	lpParameters   *uint16
	lpDirectory    *uint16
	nShow          int32
	hInstApp       uintptr
	lpIDList       uintptr
	lpClass        *uint16
	hkeyClass      uintptr
	dwHotKey       uint32
	hIconOrMonitor uintptr
	hProcess       syscall.Handle
}

const (
	seeMaskNoCloseProcess = 0x00000040
	seeMaskNoAsync        = 0x00000100
	swHide                = 0
	errorCancelled        = 1223
	stillActive           = 259
)

// AllowFirewallRule runs argv (FirewallCommand's) elevated — one UAC
// prompt — and waits for it.
func AllowFirewallRule(ctx context.Context, argv []string) error {
	if len(argv) < 2 {
		return errors.New("relayconsole: empty firewall command")
	}
	verb, _ := syscall.UTF16PtrFromString("runas")
	file, err := syscall.UTF16PtrFromString(argv[0])
	if err != nil {
		return err
	}
	params, err := syscall.UTF16PtrFromString(strings.Join(argv[1:], " "))
	if err != nil {
		return err
	}
	info := shellExecuteInfo{
		fMask:        seeMaskNoCloseProcess | seeMaskNoAsync,
		lpVerb:       verb,
		lpFile:       file,
		lpParameters: params,
		nShow:        swHide,
	}
	info.cbSize = uint32(unsafe.Sizeof(info))
	if r, _, e := procShellExecuteExW.Call(uintptr(unsafe.Pointer(&info))); r == 0 {
		if errno, ok := e.(syscall.Errno); ok && errno == errorCancelled {
			return errElevationDeclined
		}
		return fmt.Errorf("relayconsole: ShellExecuteEx: %w", e)
	}
	if info.hProcess == 0 {
		return nil
	}
	defer syscall.CloseHandle(info.hProcess)
	for {
		ev, err := syscall.WaitForSingleObject(info.hProcess, 200)
		if err != nil {
			return err
		}
		if ev == syscall.WAIT_OBJECT_0 {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	var code uint32
	if r, _, e := procGetExitCodeProc.Call(uintptr(info.hProcess), uintptr(unsafe.Pointer(&code))); r == 0 {
		return fmt.Errorf("relayconsole: GetExitCodeProcess: %w", e)
	}
	if code != 0 && code != stillActive {
		return fmt.Errorf("relayconsole: netsh exited with %d", code)
	}
	return nil
}

// FirewallRulePresent reports whether a rule named FirewallRuleName exists.
// netsh answers this without elevation; "No rules match" exits non-zero.
func FirewallRulePresent(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "netsh", "advfirewall", "firewall", "show", "rule", "name="+FirewallRuleName)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return true, nil
	case errors.As(err, &exit):
		return false, nil
	default:
		return false, err
	}
}
