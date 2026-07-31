package main

import (
	"fmt"
	cliserve "model-proxy/internal/cli/serve"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// -- T10: `stop` with no pid file prints "No daemon running" (exit 0) ---

func TestCLI_StopNoDaemon(t *testing.T) {
	cfgPath := writeDaemonConfig(t)
	stdout, _, code := runCLI(t, "stop", cfgPath)
	if code != 0 {
		t.Fatalf("stop (no daemon) exit=%d want 0\n--- stdout ---\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "No daemon running") {
		t.Errorf("stop stdout missing 'No daemon running':\n%s", stdout)
	}
}

// -- T11: `reload` with no pid file prints "No daemon running" (exit 0) ---

func TestCLI_ReloadNoDaemon(t *testing.T) {
	cfgPath := writeDaemonConfig(t)
	stdout, _, code := runCLI(t, "reload", cfgPath)
	if code != 0 {
		t.Fatalf("reload (no daemon) exit=%d want 0\n--- stdout ---\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "No daemon running") {
		t.Errorf("reload stdout missing 'No daemon running':\n%s", stdout)
	}
}

// writeDaemonConfig writes a valid config whose log_file (and thus pid file)
// lives in a temp dir with no pre-existing .pid, so stop/reload hit the
// "pid file not found" branch.
func writeDaemonConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	body := fmt.Sprintf(`listen: 127.0.0.1:15721
log_file: %s/mp.log
providers:
  aqp:
    openai_base_url: https://example.invalid/compass-api/v1
    provider_id: aqp
    models:
      - glm-5.2
`, dir)
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestResolveLogFile(t *testing.T) {
	cfg := &Config{LogFile: "/from/config.log"}
	sa := serveArgs{Config: "/etc/model-proxy/config.yaml"}

	// config used
	if got := cliserve.ResolveLogFile(sa, cfg); got != "/from/config.log" {
		t.Errorf("config: got %q", got)
	}
	// default: the OS temp dir (runtime artifacts), e.g. /tmp on Linux, $TMPDIR on macOS
	wantDefault := filepath.Join(os.TempDir(), "model-proxy.log")
	if got := cliserve.ResolveLogFile(serveArgs{Config: "/etc/model-proxy/config.yaml"}, &Config{}); got != wantDefault {
		t.Errorf("default: got %q want %q", got, wantDefault)
	}
}

func TestPidFilePath(t *testing.T) {
	if got := cliserve.PidFilePath("/var/log/model-proxy.log"); got != "/var/log/model-proxy.pid" {
		t.Errorf("got %q", got)
	}
	if got := cliserve.PidFilePath("/var/log/agent"); got != "/var/log/agent.pid" {
		t.Errorf("got %q", got)
	}
}

func TestWriteReadPidFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.pid")
	if err := cliserve.WritePidFile(path, 4242); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "4242\n" {
		t.Errorf("got %q", string(b))
	}
}

// TestAliveDaemonPid covers the three branches of the pre-start guard's helper:
// no pid file -> 0; a live pid (this test process) -> that pid; a stale pid
// (process gone) -> 0 AND the pid file is removed so the next start isn't
// confused. This is the check daemonize uses to refuse a second `serve daemon`.
func TestAliveDaemonPid(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "mp.log")
	pidPath := cliserve.PidFilePath(logFile)

	// No pid file -> 0.
	if got := aliveDaemonPid(logFile); got != 0 {
		t.Errorf("no pid file: got %d want 0", got)
	}

	// Live pid (this test process) -> returned, pid file untouched.
	if err := cliserve.WritePidFile(pidPath, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if got := aliveDaemonPid(logFile); got != os.Getpid() {
		t.Errorf("live pid: got %d want %d", got, os.Getpid())
	}
	if _, err := os.Stat(pidPath); err != nil {
		t.Error("live pid: pid file should still exist")
	}

	// Stale pid (a pid that surely isn't running) -> 0, and the pid file removed.
	if err := cliserve.WritePidFile(pidPath, 999999); err != nil {
		t.Fatal(err)
	}
	if got := aliveDaemonPid(logFile); got != 0 {
		t.Errorf("stale pid: got %d want 0", got)
	}
	if _, err := os.Stat(pidPath); err == nil {
		t.Error("stale pid: pid file should have been removed")
	}
}

// TestDaemonizeRefusesSecondDaemon asserts the pre-start guard: when a pid file
// names a live process, `serve daemon` must refuse (non-zero exit + "already
// running" message) rather than launching a second supervisor that overwrites
// the pid file and orphans the first. The "live" pid is this test process, so
// no real daemon is spawned.
func TestDaemonizeRefusesSecondDaemon(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "mp.log")
	// Pretend a daemon is running: pid file names THIS test process (alive).
	if err := cliserve.WritePidFile(cliserve.PidFilePath(logFile), os.Getpid()); err != nil {
		t.Fatal(err)
	}
	cfgBody := fmt.Sprintf("listen: 127.0.0.1:0\nlog_file: %s\nproviders:\n  zhipu:\n    openai_base_url: https://x\n    provider_id: zhipu\n    models:\n      - m\nroutes:\n  m:\n    - {provider: zhipu, model: m}\n", logFile)
	cfgPath := writeTempConfig(t, cfgBody)

	_, stderr, code := runCLI(t, "serve", cfgPath, "daemon")
	if code == 0 {
		t.Error("second daemon: exit=0 want non-zero (should refuse)")
	}
	if !strings.Contains(stderr, "already running") {
		t.Errorf("second daemon stderr missing 'already running':\n%s", stderr)
	}
	// The pid file must be intact (not overwritten) so the real daemon is still
	// reachable by `serve stop`.
	b, err := os.ReadFile(cliserve.PidFilePath(logFile))
	if err != nil {
		t.Fatalf("pid file removed/missing: %v", err)
	}
	if strings.TrimSpace(string(b)) != fmt.Sprintf("%d", os.Getpid()) {
		t.Errorf("pid file overwritten: got %q want %d", string(b), os.Getpid())
	}
}

// --- openLogFile ---

func TestOpenLogFile_CreatesDirAndFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "model-proxy.log")
	f, err := cliserve.OpenLogFile(p)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := os.Stat(p); err != nil {
		t.Errorf("log file not created: %v", err)
	}
}

func TestOpenLogFile_Appends(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.log")
	f1, _ := cliserve.OpenLogFile(p)
	f1.Write([]byte("first\n"))
	f1.Close()
	f2, _ := cliserve.OpenLogFile(p)
	f2.Write([]byte("second\n"))
	f2.Close()
	data, _ := os.ReadFile(p)
	if !strings.Contains(string(data), "first") || !strings.Contains(string(data), "second") {
		t.Errorf("append lost content: %s", data)
	}
}

// --- pidFilePath (daemon_test.go already covers it; skipped here) ---

// --- writePidFile ---

func TestWritePidFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.pid")
	if err := cliserve.WritePidFile(p, 12345); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p)
	if !strings.Contains(string(data), "12345") {
		t.Errorf("pid file content=%q want 12345", data)
	}
}
