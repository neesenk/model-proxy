// Package restartserve is a test-only package: the real end-to-end contract
// for scripts/restart_serve.sh. It builds the actual model-proxy binary, boots
// a real `serve` process in a sandbox (temp HOME, loopback port, static
// provider), then runs the script and asserts the observable restart
// semantics: old listener SIGINTed and gone, port re-listening under a new
// pid, fresh-start path, and listen-port parsing from config.yaml.
//
// External precondition: lsof + nohup (unix); the suite skips on windows.
package restartserve

import (
	"fmt"
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
	// Minimal env: isolated HOME (never the real one) + PATH for lsof/nohup.
	return []string{"HOME=" + filepath.Join(dir, "home"), "PATH=" + os.Getenv("PATH")}
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
		_ = syscall.Kill(pid, syscall.SIGINT)
		waitFor(t, "port listener exit", 15*time.Second, func() bool { return processGone(pid) })
	})
}

func processGone(pid int) bool {
	return syscall.Kill(pid, 0) == syscall.ESRCH
}

// isTestBinary verifies pid belongs to the model-proxy binary we built for
// this suite. On Linux /proc/<pid>/exe is authoritative; on Darwin we fall back
// to the process command name from ps.
func isTestBinary(pid int, binary string) bool {
	if exe, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil {
		return exe == binary
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=").Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == filepath.Base(binary)
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
