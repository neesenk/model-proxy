//go:build windows

package main

import "syscall"

// sysProcAttrDetach is a no-op on Windows (setsid is Unix-only). Daemon mode is
// best supported on Unix; on Windows the supervisor stays attached to the console.
func sysProcAttrDetach() *syscall.SysProcAttr {
	return nil
}
