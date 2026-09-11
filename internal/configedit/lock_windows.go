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

const lockfileExclusiveLock = 0x2

func tryLockFile(f *os.File) (bool, error) {
	var overlapped syscall.Overlapped
	r1, _, err := procLockFileEx.Call(
		uintptr(f.Fd()), lockfileExclusiveLock|0x1, 0, 1, 0,
		uintptr(unsafe.Pointer(&overlapped)))
	if r1 == 0 {
		if err == syscall.Errno(33) {
			return false, nil
		} // ERROR_LOCK_VIOLATION
		return false, err
	}
	return true, nil
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
