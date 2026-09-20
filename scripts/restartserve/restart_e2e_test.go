// Package restartserve is a test-only package: the real end-to-end contract
// for scripts/restart_serve.sh. It builds the actual model-proxy binary, boots
// a real `serve` process in a sandbox (temp HOME+TMPDIR, loopback port, static
// provider), then runs the script and asserts the observable restart
// semantics: old listener SIGINTed and gone, port re-listening under a new
// pid, fresh-start path, listen-port parsing from config.yaml, and the
// pidfile-owner stop (a daemon's supervisor must be stopped, never left to
// respawn its killed worker).
//
// External precondition: lsof + nohup (unix); the suite skips on windows.
package restartserve

import (
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var (
	binary    string
	binaryDir string
	buildOnce sync.Once
	buildErr  error
)

// TestMain cleans up the shared build directory that requireBinary creates
// with os.MkdirTemp (t.TempDir is unusable there: buildOnce runs inside the
// first test but the binary must outlive it for the rest of the suite).
func TestMain(m *testing.M) {
	code := m.Run()
	if binaryDir != "" {
		_ = os.RemoveAll(binaryDir)
	}
	os.Exit(code)
}

func rootDir(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// scripts/restartserve/ -> repo root is two levels up.
	return filepath.Dir(filepath.Dir(filepath.Dir(self)))
}

func scriptPath(t *testing.T) string {
	t.Helper()
	p := filepath.Join(rootDir(t), "scripts", "restart_serve.sh")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("script missing: %v", err)
	}
	return p
}

func requireBinary(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("restart_serve.sh requires lsof/nohup (unix-only script)")
	}
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof not available (external precondition for the restart script)")
	}
	buildOnce.Do(func() {
		tmp, err := os.MkdirTemp("", "restartserve-e2e-bin-*")
		if err != nil {
			buildErr = err
			return
		}
		binaryDir = tmp
		binary = filepath.Join(tmp, "model-proxy")
		cmd := exec.Command("go", "build", "-o", binary, ".")
		cmd.Dir = rootDir(t)
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatalf("build real binary: %v", buildErr)
	}
	return binary
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// sandbox creates an isolated serve workdir: own config.yaml (loopback listen,
// static provider, no real upstream), own HOME, and the real binary linked in
// as ./model-proxy (the script runs `./model-proxy serve` from --dir).
func sandbox(t *testing.T) (dir string, port int) {
	t.Helper()
	bin := requireBinary(t)
	dir = t.TempDir()
	port = freePort(t)
	if err := os.Mkdir(filepath.Join(dir, "home"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Sandboxed TMPDIR: the default log/pid file live under os.TempDir, so
	// without it a sandbox serve (or the script's pidfile probe) could claim
	// or stop the developer's REAL gateway via $TMPDIR/model-proxy.pid.
	if err := os.Mkdir(filepath.Join(dir, "tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(bin, filepath.Join(dir, "model-proxy")); err != nil {
		t.Fatalf("link binary: %v", err)
	}
	cfg := fmt.Sprintf(`listen: 127.0.0.1:%d
providers:
  dummy:
    provider_id: static
    openai_base_url: http://127.0.0.1:1
    models: [m1]
request_log:
  enabled: false
`, port)
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, port
}

func envFor(dir string) []string {
	// Minimal env: isolated HOME + TMPDIR (never the real ones) + PATH for
	// lsof/nohup.
	return []string{
		"HOME=" + filepath.Join(dir, "home"),
		"TMPDIR=" + filepath.Join(dir, "tmp"),
		"PATH=" + os.Getenv("PATH"),
	}
}

func pidOnPort(port int) (int, error) {
	out, err := exec.Command("lsof", "-tiTCP:"+strconv.Itoa(port), "-sTCP:LISTEN").Output()
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0]))
	if err != nil {
		return 0, fmt.Errorf("parse lsof pid from %q: %v", out, err)
	}
	return pid, nil
}

func waitFor(t *testing.T, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func listening(port int) bool {
	_, err := pidOnPort(port)
	return err == nil
}

// startServe boots a real `model-proxy serve` in the sandbox and returns its
// pid plus a channel closed when the process exits (the Wait goroutine reaps
// it immediately, so a dead child never lingers as a zombie that kill(2)
// would still report as alive). Cleanup SIGINTs it (graceful drain path).
func startServe(t *testing.T, dir string, port int) (int, <-chan error) {
	t.Helper()
	logPath := filepath.Join(dir, "serve.boot.log")
	f, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(dir, "model-proxy"), "serve")
	cmd.Dir = dir
	cmd.Env = envFor(dir)
	cmd.Stdout, cmd.Stderr = f, f
	if err := cmd.Start(); err != nil {
		t.Fatalf("start serve: %v", err)
	}
	pid := cmd.Process.Pid
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait(); close(exited) }()
	waitFor(t, "serve to listen", 15*time.Second, func() bool { return listening(port) })
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGINT)
		select {
		case <-exited:
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
		}
		_ = f.Close()
	})
	return pid, exited
}

func runScript(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{scriptPath(t), "--dir", dir, "--log", filepath.Join(dir, "serve.log")}, args...)
	cmd := exec.Command("sh", full...)
	cmd.Env = envFor(dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("restart_serve.sh %v failed: %v\n%s", args, err, out)
	}
	return string(out)
}

// killPortOnCleanup SIGINTs the expected listener on test teardown. When
// expectedPID is non-zero it is killed directly; otherwise the port is used as
// a fallback but the process is verified to be the model-proxy test binary
// before any signal is sent, so an unrelated listener cannot be killed.
func killPortOnCleanup(t *testing.T, port int, binary string, expectedPID int) {
	t.Helper()
	t.Cleanup(func() {
		pid := expectedPID
		if pid == 0 {
			found, err := pidOnPort(port)
			if err != nil {
				return
			}
			pid = found
		}
		if !isTestBinary(pid, binary) {
			return
		}
		if err := stopServeProcess(pid); err != nil {
			t.Errorf("stop serve pid %d: %v", pid, err)
		}
	})
}

// stopServeProcess SIGINTs pid (graceful drain path) and escalates to SIGKILL
// when it does not exit. The escalation is mandatory: a serve wedged in
// teardown that is only SIGINTed outlives the test run as an orphaned,
// still-listening process (reparented to init) and accumulates on dev
// machines. Errors only when even SIGKILL leaves the process alive.
func stopServeProcess(pid int) error {
	_ = syscall.Kill(pid, syscall.SIGINT)
	if waitGone(pid, 15*time.Second) {
		return nil
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	if !waitGone(pid, 5*time.Second) {
		return fmt.Errorf("still alive after SIGKILL")
	}
	return nil
}

// waitGone polls until kill(2) reports pid gone. Unlike waitFor it never
// fails the test, so callers can act on a timeout (e.g. escalate the signal)
// instead of aborting the cleanup with the process left running.
func waitGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if processGone(pid) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return processGone(pid)
}

func processGone(pid int) bool {
	return syscall.Kill(pid, 0) == syscall.ESRCH
}

// isTestBinary verifies pid belongs to the model-proxy binary we built for
// this suite. On Linux /proc/<pid>/exe is authoritative; on Darwin we fall back
// to the process command name from ps. Darwin reports comm as argv[0]
// VERBATIM — "./model-proxy" for the script-started serve, an absolute path
// for startServe's child — so both sides are compared by base name; an exact
// string match would silently skip the kill and leak the orphaned serve.
func isTestBinary(pid int, binary string) bool {
	if exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil {
		return exe == binary
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err != nil {
		return false
	}
	return filepath.Base(strings.TrimSpace(string(out))) == filepath.Base(binary)
}

func exitedYet(exited <-chan error) bool {
	select {
	case <-exited:
		return true
	default:
		return false
	}
}

// TestHotRestartReplacesListener is the核心契约: with a live serve on the
// port, one script invocation SIGINTs it, waits for port release, starts the
// new process and waits for it to listen — no cross-call downtime window.
func TestHotRestartReplacesListener(t *testing.T) {
	dir, port := sandbox(t)
	pid1, exited1 := startServe(t, dir, port)
	// 覆盖脚本拉起的新实例；pid1 由 startServe 自己清理。binary 已由 sandbox 构建。
	killPortOnCleanup(t, port, binary, 0)

	out := runScript(t, dir, "--port", strconv.Itoa(port))

	if !strings.Contains(out, fmt.Sprintf("restarted on port %d", port)) {
		t.Errorf("script output %q missing success line for port %d", out, port)
	}
	waitFor(t, "port re-listening", 15*time.Second, func() bool { return listening(port) })
	pid2, err := pidOnPort(port)
	if err != nil {
		t.Fatalf("no listener after restart: %v", err)
	}
	if pid2 == pid1 {
		t.Errorf("listener pid unchanged (%d) — old process was not replaced", pid1)
	}
	// The old process may briefly outlive port release (drain + final flush);
	// it must still exit promptly after SIGINT.
	waitFor(t, "old process exit", 15*time.Second, func() bool { return exitedYet(exited1) })

	// New instance actually serves, not just holds the socket.
	logBytes, err := os.ReadFile(filepath.Join(dir, "serve.log"))
	if err != nil {
		t.Fatalf("read script-managed log: %v", err)
	}
	if !strings.Contains(string(logBytes), fmt.Sprintf("listening on 127.0.0.1:%d", port)) {
		t.Errorf("serve.log missing listening line for new instance:\n%s", logBytes)
	}
}

// TestColdStartParsesPortFromConfig: with nothing listening, the script must
// derive the port from config.yaml's listen: (no --port) and start fresh.
func TestColdStartParsesPortFromConfig(t *testing.T) {
	dir, port := sandbox(t)

	// Register BEFORE runScript: if the script fails (t.Fatalf inside
	// runScript), a cleanup registered after it would never run and the
	// nohup'd serve would leak. binary 已由 sandbox 构建。
	killPortOnCleanup(t, port, binary, 0)
	out := runScript(t, dir) // no --port: exercise the config.yaml parser

	if !strings.Contains(out, fmt.Sprintf("restarted on port %d", port)) {
		t.Errorf("script output %q missing parsed port %d", out, port)
	}
	waitFor(t, "port listening", 15*time.Second, func() bool { return listening(port) })
	if _, err := pidOnPort(port); err != nil {
		t.Fatalf("no listener after cold start: %v", err)
	}
}

// TestStopServeProcessEscalatesToSigkill guards the cleanup leak fix: a
// process that ignores SIGINT (a wedged serve) must still be reaped via the
// SIGKILL fallback, not left orphaned.
func TestStopServeProcessEscalatesToSigkill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("signal escalation test requires unix signals")
	}
	cmd := exec.Command("sh", "-c", "trap '' INT; echo ready; sleep 60")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("helper stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start SIGINT-ignoring helper: %v", err)
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }() // reap on exit so kill(2) can report ESRCH
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	// Synchronize on the trap actually being installed: signaling before the
	// helper runs `trap '' INT` would kill it with the DEFAULT SIGINT action
	// and the SIGKILL escalation path under test would never execute.
	var line [5]byte
	if _, err := io.ReadFull(out, line[:]); err != nil || string(line[:]) != "ready" {
		t.Fatalf("helper did not report ready: %q, %v", line, err)
	}

	// The SIGINT grace wait is the escalation price; keep it observable so a
	// future change that skips SIGKILL (or kills upfront) fails this test.
	start := time.Now()
	if err := stopServeProcess(pid); err != nil {
		t.Fatalf("stopServeProcess: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 15*time.Second {
		t.Fatalf("stopServeProcess returned after %s — SIGINT killed a SIGINT-ignoring process?", elapsed)
	}
	if !processGone(pid) {
		t.Fatal("SIGINT-ignoring process still alive after stopServeProcess")
	}
}

// TestIsTestBinaryMatchesArgv0Path guards the Darwin leak: ps reports comm as
// argv[0] verbatim ("./model-proxy", absolute paths), so the check must
// compare base names — an exact match silently skips every cleanup kill and
// leaves orphaned serves behind.
func TestIsTestBinaryMatchesArgv0Path(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ps comm probe is unix-only")
	}
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not on PATH (external precondition)")
	}
	cmd := exec.Command(sleepPath, "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	if !isTestBinary(cmd.Process.Pid, sleepPath) {
		t.Errorf("isTestBinary(%d, %q) = false, want true (base-name match)", cmd.Process.Pid, sleepPath)
	}
	other := filepath.Join(filepath.Dir(sleepPath), "model-proxy")
	if isTestBinary(cmd.Process.Pid, other) {
		t.Errorf("isTestBinary(%d, %q) = true for a sleep process", cmd.Process.Pid, other)
	}
}

// TestRestartStopsDaemonSupervisor: with a daemon (supervisor+worker) on the
// port, one script invocation must stop the SUPERVISOR via the pidfile before
// starting the new instance. SIGINTing only the port listener kills just the
// worker, and the supervisor respawns it after ~1s backoff — either winning
// the port back (the script's port-listen wait then "succeeds" on the OLD
// binary: a silent stale restart) or losing it and later resurrecting the old
// binary whenever the script-started serve exits.
func TestRestartStopsDaemonSupervisor(t *testing.T) {
	dir, port := sandbox(t)

	// Point log_file into the sandbox so the daemon's pidfile is sandbox-local
	// and the script exercises its config-derived pidfile branch.
	logFile := filepath.Join(dir, "daemon.log")
	cfgPath := filepath.Join(dir, "config.yaml")
	cfgBytes, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, append(cfgBytes, []byte("log_file: "+logFile+"\n")...), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(filepath.Join(dir, "model-proxy"), "serve", "daemon")
	cmd.Dir = dir
	cmd.Env = envFor(dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("serve daemon: %v\n%s", err, out)
	}
	waitFor(t, "daemon worker to listen", 15*time.Second, func() bool { return listening(port) })
	workerPID, err := pidOnPort(port)
	if err != nil {
		t.Fatalf("no daemon worker listener: %v", err)
	}
	pidBytes, err := os.ReadFile(filepath.Join(dir, "daemon.pid"))
	if err != nil {
		t.Fatalf("daemon pidfile: %v", err)
	}
	supervisorPID, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatalf("parse daemon pidfile %q: %v", pidBytes, err)
	}

	// Safety nets (LIFO: port cleanup first, supervisor SIGKILL as last
	// resort) so a failed assertion can never orphan the daemon.
	t.Cleanup(func() { _ = syscall.Kill(supervisorPID, syscall.SIGKILL) })
	killPortOnCleanup(t, port, binary, 0)

	out := runScript(t, dir, "--port", strconv.Itoa(port))
	if !strings.Contains(out, fmt.Sprintf("restarted on port %d", port)) {
		t.Errorf("script output %q missing success line for port %d", out, port)
	}

	// The supervisor and its old worker must be STOPPED, and the port must be
	// re-listened by a new pid — never by a supervisor-respawned worker.
	waitFor(t, "supervisor exit", 15*time.Second, func() bool { return processGone(supervisorPID) })
	waitFor(t, "old worker exit", 15*time.Second, func() bool { return processGone(workerPID) })
	waitFor(t, "port re-listening", 15*time.Second, func() bool { return listening(port) })
	newPID, err := pidOnPort(port)
	if err != nil {
		t.Fatalf("no listener after restart: %v", err)
	}
	if newPID == supervisorPID || newPID == workerPID {
		t.Errorf("listener pid %d was not replaced (supervisor=%d worker=%d)", newPID, supervisorPID, workerPID)
	}
}
