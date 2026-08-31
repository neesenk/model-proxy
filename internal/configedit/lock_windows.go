//go:build windows

package configedit

import (
	"os"
	"syscall"
	"unsafe"
)

// stdlib-only LockFileEx/UnlockFileEx (no golang.org/x/sys dependency, same
// convention as internal/accounts' portable file handling).
var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

const lockfileExclusiveLock = 0x2 // blocking: no LOCKFILE_FAIL_IMMEDIATELY

func lockFile(f *os.File) error {
	var overlapped syscall.Overlapped
	r1, _, err := procLockFileEx.Call(
		uintptr(f.Fd()), lockfileExclusiveLock, 0, 1, 0,
		uintptr(unsafe.Pointer(&overlapped)))
	if r1 == 0 {
		return err
	}
	return nil
}

func unlockFile(f *os.File) error {
	var overlapped syscall.Overlapped
	r1, _, err := procUnlockFileEx.Call(
		uintptr(f.Fd()), 0, 1, 0,
		uintptr(unsafe.Pointer(&overlapped)))
	if r1 == 0 {
		return err
	}
	return nil
}
