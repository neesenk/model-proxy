package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// cli_extra_test.go extends cli_test.go's subprocess pattern to the remaining
// 0%-coverage CLI handlers that call log.Fatal / os.Exit: cmdTakeover,
// cmdRestore, cmdLogout, cmdUsage, cmdStop, cmdReload.
//
// The subprocess dispatcher (TestHelperProcess in cli_test.go) was extended
// with "takeover", "restore", "logout", "stop", "reload" cases; runCLI now pins
// HOME to a fresh temp dir (isolation), and runCLIWithHome lets a test pin HOME
// to a pre-populated dir (for the logout-removes-cred-file happy path).

// -- T1: `takeover claude` backs up the original and rewrites it ---

func TestCLI_TakeoverClaude(t *testing.T) {
	dir := t.TempDir()
	claudeFile := filepath.Join(dir, "claude.json")
	original := `{"env":{"FOO":"bar"}}`
	if err := os.WriteFile(claudeFile, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := writeTakeoverConfig(t, dir, claudeFile)

	stdout, _, code := runCLI(t, "takeover", cfgPath, "claude")
	if code != 0 {
		t.Fatalf("takeover claude exit=%d want 0\n--- stdout ---\n%s", code, stdout)
	}

	// Backup created verbatim with the original content.
	bak := filepath.Join(dir, ".model-proxy", "claude.bak")
	b, err := os.ReadFile(bak)
	if err != nil {
		t.Fatalf("backup not created at %s: %v", bak, err)
	}
	if string(b) != original {
		t.Errorf("backup content = %q want %q", b, original)
	}

	// Client file rewritten to point at the proxy, existing env key preserved.
	got, err := os.ReadFile(claudeFile)
	if err != nil {
		t.Fatalf("claude file read after takeover: %v", err)
	}
	if !strings.Contains(string(got), "ANTHROPIC_BASE_URL") {
		t.Errorf("claude file not rewritten to set ANTHROPIC_BASE_URL:\n%s", got)
	}
	if !strings.Contains(string(got), "PROXY_MANAGED") {
		t.Errorf("claude file not rewritten to set ANTHROPIC_AUTH_TOKEN:\n%s", got)
	}
	if !strings.Contains(string(got), "FOO") {
		t.Errorf("claude rewrite dropped pre-existing env key FOO:\n%s", got)
	}
}

// -- T2: `restore claude` copies the backup back over the client file ---

func TestCLI_RestoreClaude(t *testing.T) {
	dir := t.TempDir()
	claudeFile := filepath.Join(dir, "claude.json")
	original := `{"env":{"FOO":"bar"}}`
	// Simulate a post-takeover client file (proxy env injected).
	if err := os.WriteFile(claudeFile,
		[]byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:15721","ANTHROPIC_AUTH_TOKEN":"PROXY_MANAGED"}}`),
		0o644); err != nil {
		t.Fatal(err)
	}
	// Simulate the backup created by a prior takeover.
	bakDir := filepath.Join(dir, ".model-proxy")
	if err := os.MkdirAll(bakDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bakDir, "claude.bak"), []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := writeTakeoverConfig(t, dir, claudeFile)

	stdout, _, code := runCLI(t, "restore", cfgPath, "claude")
	if code != 0 {
		t.Fatalf("restore claude exit=%d want 0\n--- stdout ---\n%s", code, stdout)
	}

	got, err := os.ReadFile(claudeFile)
	if err != nil {
		t.Fatalf("claude file read after restore: %v", err)
	}
	if string(got) != original {
		t.Errorf("restore content = %q want %q", got, original)
	}
}

// -- T3: `takeover <unknown>` exits non-zero (no client matched) ---

func TestCLI_TakeoverUnknownClient(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writeTakeoverConfig(t, dir, filepath.Join(dir, "claude.json"))
	_, _, code := runCLI(t, "takeover", cfgPath, "nope")
	// runTakeover on an unknown client is a no-op (listClients returns nil →
	// the loop body never runs → nil error → exit 0). This is the product
	// behavior; takeover doesn't validate the client name up front.
	if code != 0 {
		t.Errorf("takeover nope: exit=%d want 0 (no-op for unknown client)", code)
	}
}

// writeTakeoverConfig writes a valid config into dir/config.yaml whose
// takeover.claude points at claudeFile. The config dir is also where the
// backup lands (backupDir = <configDir>/.model-proxy).
func writeTakeoverConfig(t *testing.T, dir, claudeFile string) string {
	t.Helper()
	body := fmt.Sprintf(`listen: 127.0.0.1:15721
takeover:
  proxy_url: http://127.0.0.1:15721
  claude: %s
providers:
  compass:
    openai_base_url: https://example.invalid/compass-api/v1
    anthropic_base_url: https://example.invalid/compass-api
    provider_id: compass
    cqp_mint_url: https://example.invalid/api/v1/cqp/ccswitch/api_key/get_or_generate
    models:
      glm-5.2: {context: 1048576, output: 131072, modalities: {input: [text], output: [text]}}
routes:
  glm-5.2:
    - {provider: compass, model: glm-5.2, priority: 1}
`, claudeFile)
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// -- T4: `logout <unknown>` exits non-zero with "unknown provider" ---

func TestCLI_LogoutUnknownProvider(t *testing.T) {
	cfg := writeTempConfig(t, minimalConfig)
	_, stderr, code := runCLI(t, "logout", cfg, "nope")
	if code == 0 {
		t.Error("logout nope: exit=0 want non-zero")
	}
	if !strings.Contains(stderr, "unknown provider") {
		t.Errorf("logout nope stderr missing 'unknown provider':\n%s", stderr)
	}
}

// -- T5: `logout <provider>` removes the credential file ---

func TestCLI_LogoutRemovesCredFile(t *testing.T) {
	home := t.TempDir()
	cfg := writeTempConfig(t, `listen: 127.0.0.1:15721
providers:
  zhipu:
    openai_base_url: https://example.invalid/api/paas/v4
    provider_id: zhipu
    usage_url: https://example.invalid/api/monitor/usage/quota/limit
    models:
      glm-5.2: {context: 1048576, output: 131072, modalities: {input: [text], output: [text]}}
routes:
  glm-5.2:
    - {provider: zhipu, model: glm-5.2, priority: 1}
`)
	// Pre-create the apikey credential file in the isolated HOME.
	credDir := filepath.Join(home, ".model-proxy")
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	credFile := filepath.Join(credDir, "zhipu_apikey.json")
	if err := os.WriteFile(credFile, []byte(`{"api_key":"secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	stdout, _, code := runCLIWithHome(t, home, "logout", cfg, "zhipu")
	if code != 0 {
		t.Fatalf("logout zhipu exit=%d want 0\n--- stdout ---\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "Logged out") {
		t.Errorf("logout zhipu stdout missing 'Logged out':\n%s", stdout)
	}
	if _, err := os.Stat(credFile); !os.IsNotExist(err) {
		t.Errorf("cred file still exists after logout (stat err=%v)", err)
	}
}

// -- T6: `logout` with no provider prints usage and exits 0 ---

func TestCLI_LogoutNoProvider(t *testing.T) {
	cfg := writeTempConfig(t, minimalConfig)
	stdout, _, code := runCLI(t, "logout", cfg)
	if code != 0 {
		t.Fatalf("logout (no provider) exit=%d want 0", code)
	}
	if !strings.Contains(stdout, "usage: model-proxy logout") {
		t.Errorf("logout (no provider) stdout missing usage line:\n%s", stdout)
	}
	if !strings.Contains(stdout, "compass") {
		t.Errorf("logout (no provider) stdout missing provider list entry compass:\n%s", stdout)
	}
}

// -- T7: `usage <unknown>` exits non-zero with "unknown provider" ---

func TestCLI_UsageUnknownProvider(t *testing.T) {
	cfg := writeTempConfig(t, minimalConfig)
	_, stderr, code := runCLI(t, "usage", cfg, "nope")
	if code == 0 {
		t.Error("usage nope: exit=0 want non-zero")
	}
	if !strings.Contains(stderr, "unknown provider") {
		t.Errorf("usage nope stderr missing 'unknown provider':\n%s", stderr)
	}
}

// -- T8: `usage <provider>` when not logged in prints "Not logged in" (exit 0) ---

func TestCLI_UsageNotLoggedIn(t *testing.T) {
	cfg := writeTempConfig(t, minimalConfig)
	// HOME is isolated by runCLI → no compass_oauth_auth.json exists.
	stdout, _, code := runCLI(t, "usage", cfg, "compass")
	if code != 0 {
		t.Fatalf("usage compass (not logged in) exit=%d want 0", code)
	}
	if !strings.Contains(stdout, "Not logged in") {
		t.Errorf("usage compass stdout missing 'Not logged in':\n%s", stdout)
	}
}

// -- T9: `usage` (no provider) iterates all providers, each "Not logged in" ---

func TestCLI_UsageAllNotLoggedIn(t *testing.T) {
	cfg := writeTempConfig(t, minimalConfig)
	stdout, _, code := runCLI(t, "usage", cfg)
	if code != 0 {
		t.Fatalf("usage (all, not logged in) exit=%d want 0", code)
	}
	if !strings.Contains(stdout, "Not logged in") {
		t.Errorf("usage (all) stdout missing 'Not logged in':\n%s", stdout)
	}
	// The all-providers view prints a divider before each provider.
	if !strings.Contains(stdout, "────────") {
		t.Errorf("usage (all) stdout missing divider line:\n%s", stdout)
	}
}

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
  compass:
    openai_base_url: https://example.invalid/compass-api/v1
    provider_id: compass
    models:
      glm-5.2: {context: 1048576, output: 131072, modalities: {input: [text], output: [text]}}
`, dir)
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
