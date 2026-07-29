//go:build !windows

package main

import "syscall"

// sysProcAttrDetach returns a SysProcAttr that starts the child in a new session
// (setsid), detaching it from the controlling terminal — the standard Unix daemon
// detach. Used when launching the supervisor.
func sysProcAttrDetach() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
