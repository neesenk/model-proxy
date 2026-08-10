package main

import (
	"fmt"
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
