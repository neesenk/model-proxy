//go:build !windows

package serve

import "syscall"

// SysProcAttrDetach returns a SysProcAttr that starts the child in a new
// session (setsid), detaching it from the controlling terminal — the standard
// Unix daemon detach. Used when launching the supervisor.
func SysProcAttrDetach() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
