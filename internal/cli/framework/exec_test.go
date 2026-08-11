package framework

import (
	"os/exec"
	"testing"
)

// exec_test.go covers RunCmd — a small wrapper around os/exec.

func TestRunCmd_StartsProcess(t *testing.T) {
	// `true` exits 0 immediately. runCmd uses Start (non-blocking), so the
	// process may still be starting when we check — but Start itself either
	// errors or returns nil with the process created.
	if err := RunCmd("true"); err != nil {
		// `true` may not be on PATH in some environments; skip then.
		if _, lerr := exec.LookPath("true"); lerr != nil {
			t.Skip("`true` not on PATH")
		}
		t.Errorf("RunCmd(true): %v", err)
	}
}

func TestRunCmd_MissingBinary(t *testing.T) {
	if err := RunCmd("no-such-binary-xyz"); err == nil {
		t.Error("RunCmd(missing binary): want error, got nil")
	}
}
