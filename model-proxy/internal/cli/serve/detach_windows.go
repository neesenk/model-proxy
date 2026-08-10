//go:build windows

package serve

import "syscall"

// SysProcAttrDetach is a no-op on Windows (setsid is Unix-only). Daemon mode is
// best supported on Unix; on Windows the supervisor stays attached to the
// console.
func SysProcAttrDetach() *syscall.SysProcAttr {
	return nil
}
