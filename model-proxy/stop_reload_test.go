package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stop_reload_test.go covers cmdStop / cmdReload's stale-pid and invalid-pid
// branches via the subprocess CLI harness. The happy path (signaling a real
// daemon) is not tested end-to-end (needs a long-running worker).

// --- cmdStop: stale pid file (process gone) → removed + "Daemon not running" ---

func TestCLI_StopStalePidFile(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "mp.log")
	pidPath := filepath.Join(dir, "mp.pid")
	// Write a pid that definitely isn't running (999999 is unlikely to exist).
	if err := os.WriteFile(pidPath, []byte("999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfgBody := fmt.Sprintf("listen: 127.0.0.1:15721\nlog_file: %s\nproviders:\n  zhipu:\n    openai_base_url: https://x\n    provider_id: zhipu\n    models:\n      - m\nroutes:\n  m:\n    - {provider: zhipu, model: m}\n", logFile)
	cfgPath := writeTempConfig(t, cfgBody)
	// Use a HOME whose .model-proxy won't be touched; pass the config via --config.
	stdout, _, code := runCLI(t, "stop", cfgPath)
	if code != 0 {
		t.Fatalf("stop stale pid exit=%d want 0\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "Daemon not running.") || !strings.Contains(stdout, "removed stale pid file") {
		t.Errorf("stop stale pid: stdout missing exact stale-process result:\n%s", stdout)
	}
	// The stale pid file should have been removed.
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Errorf("stale pid file should have been removed, stat err=%v", err)
	}
}

// --- cmdStop: invalid pid (0 / non-numeric) → non-zero ---

func TestCLI_StopInvalidPid(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "mp.log")
	pidPath := filepath.Join(dir, "mp.pid")
	if err := os.WriteFile(pidPath, []byte("not-a-pid\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfgBody := fmt.Sprintf("listen: 127.0.0.1:15721\nlog_file: %s\nproviders:\n  zhipu:\n    openai_base_url: https://x\n    provider_id: zhipu\n    models:\n      - m\nroutes:\n  m:\n    - {provider: zhipu, model: m}\n", logFile)
	cfgPath := writeTempConfig(t, cfgBody)
	_, stderr, code := runCLI(t, "stop", cfgPath)
	if code != 1 {
		t.Errorf("stop invalid pid: exit=%d want 1", code)
	}
	if !strings.Contains(stderr, "invalid pid") {
		t.Errorf("stop invalid pid stderr missing 'invalid pid':\n%s", stderr)
	}
}

// --- cmdReload: stale pid file → "not running" message ---

func TestCLI_ReloadStalePidFile(t *testing.T) {
	dir := t.TempDir()
	logFile := filepath.Join(dir, "mp.log")
	pidPath := filepath.Join(dir, "mp.pid")
	if err := os.WriteFile(pidPath, []byte("999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfgBody := fmt.Sprintf("listen: 127.0.0.1:15721\nlog_file: %s\nproviders:\n  zhipu:\n    openai_base_url: https://x\n    provider_id: zhipu\n    models:\n      - m\nroutes:\n  m:\n    - {provider: zhipu, model: m}\n", logFile)
	cfgPath := writeTempConfig(t, cfgBody)
	stdout, _, code := runCLI(t, "reload", cfgPath)
	if code != 0 {
		t.Fatalf("reload stale pid exit=%d want 0\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "Daemon not running.") || !strings.Contains(stdout, "removed stale pid file") {
		t.Errorf("reload stale pid: stdout missing exact stale-process result:\n%s", stdout)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Errorf("stale pid file should have been removed, stat err=%v", err)
	}
}
