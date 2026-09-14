//go:build !windows

package serve

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// supervisor_lifecycle_test.go covers the production thin shells the
// deps-seamed supervisor tests cannot reach in-package: resolveDaemonPid /
// CmdStop / CmdReload against a test-owned child process, and SpawnWorker /
// productionSupervisorDeps. Children are always spawned and reaped by the
// test itself — no signals ever target processes the test does not own.
// Unix-only: the child fixture is `sleep` and liveness probes use
// syscall.Kill(pid, 0).

// spawnStoppableChild starts a child process the test owns (a long sleep) and
// returns it plus a channel closed once the child is reaped. The reaper
// goroutine keeps the child from lingering as a zombie, which would still
// answer Signal(0) on Unix.
func spawnStoppableChild(t *testing.T) (*exec.Cmd, <-chan struct{}) {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot spawn sleep child: %v", err)
	}
	reaped := make(chan struct{})
	go func() { _ = cmd.Wait(); close(reaped) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		select {
		case <-reaped:
		case <-time.After(2 * time.Second):
			t.Error("child was not reaped after kill")
		}
	})
	return cmd, reaped
}

func writeTestPidFile(t *testing.T, pidPath string, pid int) {
	t.Helper()
	if err := WritePidFile(pidPath, pid); err != nil {
		t.Fatalf("write pid file: %v", err)
	}
}

func plainColor(s string) string { return s }

func TestResolveDaemonPidNoPidFile(t *testing.T) {
	env, sa, pidPath := supervisorTestEnv(t)
	proc, pid, _, ok := resolveDaemonPid(env, sa, plainColor, plainColor)
	if ok || proc != nil || pid != 0 {
		t.Errorf("resolveDaemonPid = (proc=%v pid=%d ok=%v), want all zero with no pid file", proc, pid, ok)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Errorf("pid file unexpectedly exists: %v", err)
	}
}

func TestResolveDaemonPidRemovesStalePidFile(t *testing.T) {
	env, sa, pidPath := supervisorTestEnv(t)
	// A child that has exited AND been reaped leaves a pid that fails Signal(0).
	cmd, reaped := spawnStoppableChild(t)
	deadPid := cmd.Process.Pid
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill child: %v", err)
	}
	<-reaped
	if err := syscall.Kill(deadPid, 0); err == nil {
		t.Skipf("pid %d still signalable after kill+reap; skipping stale-pid test", deadPid)
	}

	writeTestPidFile(t, pidPath, deadPid)
	proc, _, _, ok := resolveDaemonPid(env, sa, plainColor, plainColor)
	if ok || proc != nil {
		t.Errorf("resolveDaemonPid = (proc=%v ok=%v), want stale pid rejected", proc, ok)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Errorf("stale pid file was not removed: %v", err)
	}
}

func TestResolveDaemonPidParsesWrittenPidFile(t *testing.T) {
	env, sa, pidPath := supervisorTestEnv(t)
	cmd, _ := spawnStoppableChild(t)
	writeTestPidFile(t, pidPath, cmd.Process.Pid)

	proc, pid, gotPath, ok := resolveDaemonPid(env, sa, plainColor, plainColor)
	if !ok || proc == nil {
		t.Fatal("resolveDaemonPid rejected a live pid")
	}
	if pid != cmd.Process.Pid {
		t.Errorf("pid = %d, want %d", pid, cmd.Process.Pid)
	}
	if gotPath != pidPath {
		t.Errorf("pidPath = %q, want %q", gotPath, pidPath)
	}
	// The pid file content round-trips through parsePidFileContents.
	raw, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read pid file: %v", err)
	}
	if strings.TrimSpace(string(raw)) != strconv.Itoa(cmd.Process.Pid) {
		t.Errorf("pid file content = %q, want %d", string(raw), cmd.Process.Pid)
	}
}

func TestCmdStopTerminatesOwnedDaemon(t *testing.T) {
	env, sa, pidPath := supervisorTestEnv(t)
	cmd, reaped := spawnStoppableChild(t)
	writeTestPidFile(t, pidPath, cmd.Process.Pid)

	done := make(chan struct{})
	go func() {
		CmdStop(env, sa, plainColor, plainColor, plainColor)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("CmdStop did not finish within the graceful window")
	}

	select {
	case <-reaped:
	default:
		t.Error("child still alive after CmdStop")
	}
	// The pid file is removed by the real supervisor on exit (runSupervisor's
	// defer), not by CmdStop on the graceful path — our sleep child cannot
	// remove it, so there is nothing to assert about it here.
}

func TestCmdReloadSignalsOwnedDaemon(t *testing.T) {
	env, sa, pidPath := supervisorTestEnv(t)
	cmd, reaped := spawnStoppableChild(t)
	writeTestPidFile(t, pidPath, cmd.Process.Pid)

	// SIGHUP's default action terminates the child; CmdReload returns once the
	// signal is sent (delivery itself is what we pin, not the child's exit).
	CmdReload(env, sa, plainColor, plainColor, plainColor)

	select {
	case <-reaped:
	case <-time.After(2 * time.Second):
		t.Error("child did not exit after SIGHUP from CmdReload")
	}
	// Reload never removes the pid file — the daemon keeps running.
	if _, err := os.Stat(pidPath); err != nil {
		t.Errorf("pid file removed by reload: %v", err)
	}
}

func TestSpawnWorkerStartsProcess(t *testing.T) {
	env, sa, _ := supervisorTestEnv(t)
	// Use the test binary itself as the executable: it starts (unknown
	// positional args make it exit on its own) and is a process we own.
	env.Executable = os.Args[0]
	cmd := SpawnWorker(env, sa)
	if cmd == nil {
		t.Fatal("SpawnWorker returned nil")
	}
	if cmd.Process.Pid <= 0 {
		t.Errorf("worker pid = %d, want > 0", cmd.Process.Pid)
	}
	w := execSupervisorWorker{cmd: cmd}
	if w.PID() != cmd.Process.Pid {
		t.Errorf("PID() = %d, want %d", w.PID(), cmd.Process.Pid)
	}
	_ = w.Kill()
	if err := w.Wait(); err == nil {
		t.Error("killed worker should exit non-zero")
	}
	if err := w.Signal(syscall.Signal(0)); err == nil {
		t.Error("Signal(0) on a reaped worker should fail")
	}
}

func TestSpawnWorkerBadExecutableReturnsNil(t *testing.T) {
	env, sa, _ := supervisorTestEnv(t)
	env.Executable = filepath.Join(os.TempDir(), "model-proxy-no-such-binary")
	if cmd := SpawnWorker(env, sa); cmd != nil {
		_ = cmd.Process.Kill()
		t.Fatal("SpawnWorker with a bad executable must return nil")
	}
}

func TestProductionSupervisorDepsShape(t *testing.T) {
	sigs := make(chan os.Signal, 1)
	deps := productionSupervisorDeps(sigs)
	if deps.signals == nil || deps.after == nil || deps.now == nil || deps.pid == nil || deps.spawn == nil {
		t.Fatal("productionSupervisorDeps must fill every seam")
	}
	if deps.pid() != os.Getpid() {
		t.Errorf("deps.pid() = %d, want %d", deps.pid(), os.Getpid())
	}
	// The spawn adapter converts a nil *exec.Cmd (failed start) into a nil
	// worker so the supervisor treats it as an immediate-exit restart.
	env, sa, _ := supervisorTestEnv(t)
	env.Executable = filepath.Join(os.TempDir(), "model-proxy-no-such-binary")
	if w := deps.spawn(env, sa); w != nil {
		t.Fatalf("spawn with a bad executable = %v, want nil", w)
	}
}
