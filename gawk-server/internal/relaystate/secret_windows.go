//go:build windows

package relaystate

// On Windows a secret is additionally sealed with DPAPI for the current
// user (CryptProtectData), so a copy of the state directory is useless on
// another account or machine. Called through syscall directly so the module
// takes no new dependency.

import (
	"fmt"
	"syscall"
	"unsafe"
)

var (
	crypt32                = syscall.NewLazyDLL("crypt32.dll")
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procCryptProtectData   = crypt32.NewProc("CryptProtectData")
	procCryptUnprotectData = crypt32.NewProc("CryptUnprotectData")
	procLocalFree          = kernel32.NewProc("LocalFree")
)

// dataBlob is DATA_BLOB.
type dataBlob struct {
	cbData uint32
	pbData *byte
}

func newBlob(b []byte) *dataBlob {
	if len(b) == 0 {
		return &dataBlob{}
	}
	return &dataBlob{cbData: uint32(len(b)), pbData: &b[0]}
}

func (d *dataBlob) bytes() []byte {
	if d.pbData == nil || d.cbData == 0 {
		return nil
	}
	return append([]byte(nil), unsafe.Slice(d.pbData, d.cbData)...)
}

// cryptProtectUIForbidden is CRYPTPROTECT_UI_FORBIDDEN: never prompt.
const cryptProtectUIForbidden = 0x1

func protect(b []byte) ([]byte, error) {
	var out dataBlob
	r, _, err := procCryptProtectData.Call(
		uintptr(unsafe.Pointer(newBlob(b))), 0, 0, 0, 0,
		cryptProtectUIForbidden, uintptr(unsafe.Pointer(&out)))
	if r == 0 {
		return nil, fmt.Errorf("CryptProtectData: %w", err)
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	return out.bytes(), nil
}

func unprotect(b []byte) ([]byte, error) {
	var out dataBlob
	r, _, err := procCryptUnprotectData.Call(
		uintptr(unsafe.Pointer(newBlob(b))), 0, 0, 0, 0,
		cryptProtectUIForbidden, uintptr(unsafe.Pointer(&out)))
	if r == 0 {
		return nil, fmt.Errorf("CryptUnprotectData: %w", err)
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	return out.bytes(), nil
}
