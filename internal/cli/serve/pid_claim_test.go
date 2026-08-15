package serve

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Regression: a foreground `serve` used to overwrite (and on exit DELETE) the
// pid file unconditionally — if a daemon was already running, its pid file was
// destroyed and `serve stop`/`reload` could never reach it again. Claiming
// must respect a live owner, and release must only delete a file that still
// names this process.

func pidFileFor(t *testing.T, dir string) string {
	t.Helper()
	return filepath.Join(dir, "proxy.log")
}

func writePid(t *testing.T, path, pid string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(pid+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readPid(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

func TestClaimForegroundPidFile_FreeFileIsClaimedAndReleased(t *testing.T) {
	logFile := pidFileFor(t, t.TempDir())

	path := ClaimForegroundPidFile(logFile)
	if path == "" {
		t.Fatal("claim of a free pid file returned \"\"")
	}
	if got := readPid(t, path); got != strconv.Itoa(os.Getpid()) {
		t.Fatalf("pid file = %q, want our pid %d", got, os.Getpid())
	}

	ReleasePidFileIfOwned(path)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("owned pid file not removed: %v", err)
	}
}

func TestClaimForegroundPidFile_LiveOwnerIsNotClobbered(t *testing.T) {
	logFile := pidFileFor(t, t.TempDir())
	pidPath := PidFilePath(logFile)
	// pid 1 is always alive and never this process.
	writePid(t, pidPath, "1")

	if path := ClaimForegroundPidFile(logFile); path != "" {
		t.Fatalf("claim returned %q for a live-owned pid file, want \"\"", path)
	}
	if got := readPid(t, pidPath); got != "1" {
		t.Fatalf("live owner's pid file was overwritten: %q", got)
	}
	// The foreground serve never owned it — release must leave it alone.
	ReleasePidFileIfOwned("")
	ReleasePidFileIfOwned(pidPath)
	if got := readPid(t, pidPath); got != "1" {
		t.Fatalf("foreign pid file must survive release: %q", got)
	}
}

func TestClaimForegroundPidFile_StaleOwnerIsReplaced(t *testing.T) {
	logFile := pidFileFor(t, t.TempDir())
	// Spawn and reap a child so its pid is definitively dead.
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	writePid(t, PidFilePath(logFile), strconv.Itoa(dead.ProcessState.Pid()))

	path := ClaimForegroundPidFile(logFile)
	if path == "" {
		t.Fatal("claim must take over a stale pid file")
	}
	if got := readPid(t, path); got != strconv.Itoa(os.Getpid()) {
		t.Fatalf("pid file = %q, want our pid", got)
	}
}

func TestReleasePidFileIfOwned_ReplacedFileSurvives(t *testing.T) {
	logFile := pidFileFor(t, t.TempDir())
	path := ClaimForegroundPidFile(logFile)
	if path == "" {
		t.Fatal("claim failed")
	}
	// A daemon restarted while the foreground serve ran and took the file over.
	writePid(t, path, "1")

	ReleasePidFileIfOwned(path)
	if got := readPid(t, path); got != "1" {
		t.Fatalf("replaced pid file must survive release: %q", got)
	}
}
