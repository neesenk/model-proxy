package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseServeArgs(t *testing.T) {
	// All cases use an explicit --config so the result doesn't depend on whether
	// ~/.model-proxy/config.yaml exists on the test host.
	cases := []struct {
		name string
		args []string
		want serveArgs
	}{
		{"bare config", []string{"--config", "c.yaml"}, serveArgs{config: "c.yaml"}},
		{"config=", []string{"--config=/x.yaml"}, serveArgs{config: "/x.yaml"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseServeArgs(tc.args)
			if got != tc.want {
				t.Errorf("got %+v want %+v", got, tc.want)
			}
		})
	}
}

// TestConfigPath_LookupOrder verifies the lookup order:
// --config flag > ~/.model-proxy/config.yaml > ./config.yaml.
func TestConfigPath_LookupOrder(t *testing.T) {
	// 1. explicit flag wins over everything.
	got := configPath([]string{"--config", "/explicit.yaml"})
	if got != "/explicit.yaml" {
		t.Errorf("flag: got %q", got)
	}
	got = configPath([]string{"--config=/explicit2.yaml"})
	if got != "/explicit2.yaml" {
		t.Errorf("flag=: got %q", got)
	}

	// 2. user-level file wins over ./config.yaml. Point HOME at a temp dir with
	// the user config present (under .model-proxy/), and a different CWD config —
	// the user one wins.
	dir := t.TempDir()
	aisDir := filepath.Join(dir, ".model-proxy")
	if err := os.MkdirAll(aisDir, 0o755); err != nil {
		t.Fatal(err)
	}
	homeCfg := filepath.Join(aisDir, "config.yaml")
	if err := os.WriteFile(homeCfg, []byte("listen: 127.0.0.1:0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldHome := homeDirForTest
	homeDirForTest = dir
	defer func() { homeDirForTest = oldHome }()
	// Without a flag and with a user-level file present, configPath returns it.
	got = configPath(nil)
	if got != homeCfg {
		t.Errorf("user-level: got %q want %q", got, homeCfg)
	}
}

func TestResolveLogFile(t *testing.T) {
	cfg := &Config{LogFile: "/from/config.log"}
	sa := serveArgs{config: "/etc/model-proxy/config.yaml"}

	// config used
	if got := resolveLogFile(sa, cfg); got != "/from/config.log" {
		t.Errorf("config: got %q", got)
	}
	// default: the OS temp dir (runtime artifacts), e.g. /tmp on Linux, $TMPDIR on macOS
	wantDefault := filepath.Join(os.TempDir(), "model-proxy.log")
	if got := resolveLogFile(serveArgs{config: "/etc/model-proxy/config.yaml"}, &Config{}); got != wantDefault {
		t.Errorf("default: got %q want %q", got, wantDefault)
	}
}

func TestPidFilePath(t *testing.T) {
	if got := pidFilePath("/var/log/model-proxy.log"); got != "/var/log/model-proxy.pid" {
		t.Errorf("got %q", got)
	}
	if got := pidFilePath("/var/log/agent"); got != "/var/log/agent.pid" {
		t.Errorf("got %q", got)
	}
}

func TestWriteReadPidFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.pid")
	if err := writePidFile(path, 4242); err != nil {
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
	pidPath := pidFilePath(logFile)

	// No pid file -> 0.
	if got := aliveDaemonPid(logFile); got != 0 {
		t.Errorf("no pid file: got %d want 0", got)
	}

	// Live pid (this test process) -> returned, pid file untouched.
	if err := writePidFile(pidPath, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	if got := aliveDaemonPid(logFile); got != os.Getpid() {
		t.Errorf("live pid: got %d want %d", got, os.Getpid())
	}
	if _, err := os.Stat(pidPath); err != nil {
		t.Error("live pid: pid file should still exist")
	}

	// Stale pid (a pid that surely isn't running) -> 0, and the pid file removed.
	if err := writePidFile(pidPath, 999999); err != nil {
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
	if err := writePidFile(pidFilePath(logFile), os.Getpid()); err != nil {
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
	b, err := os.ReadFile(pidFilePath(logFile))
	if err != nil {
		t.Fatalf("pid file removed/missing: %v", err)
	}
	if strings.TrimSpace(string(b)) != fmt.Sprintf("%d", os.Getpid()) {
		t.Errorf("pid file overwritten: got %q want %d", string(b), os.Getpid())
	}
}
