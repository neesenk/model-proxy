package main

import (
	"os/exec"
	"testing"
)

// exec_test.go covers runCmd (util.go) and sysProcAttrDetach (daemon_unix.go)
// — small wrappers around os/exec and syscall that are otherwise 0%.

func TestRunCmd_StartsProcess(t *testing.T) {
	// `true` exits 0 immediately. runCmd uses Start (non-blocking), so the
	// process may still be starting when we check — but Start itself either
	// errors or returns nil with the process created.
	if err := runCmd("true"); err != nil {
		// `true` may not be on PATH in some environments; skip then.
		if _, lerr := exec.LookPath("true"); lerr != nil {
			t.Skip("`true` not on PATH")
		}
		t.Errorf("runCmd(true): %v", err)
	}
}

func TestRunCmd_MissingBinary(t *testing.T) {
	if err := runCmd("no-such-binary-xyz"); err == nil {
		t.Error("runCmd(missing binary): want error, got nil")
	}
}

func TestSysProcAttrDetach_NonNil(t *testing.T) {
	if got := sysProcAttrDetach(); got == nil {
		t.Error("sysProcAttrDetach() = nil, want non-nil SysProcAttr")
	}
}
