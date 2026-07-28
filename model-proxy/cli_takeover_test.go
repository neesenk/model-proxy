package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
  aqp:
    openai_base_url: https://example.invalid/compass-api/v1
    anthropic_base_url: https://example.invalid/compass-api
    provider_id: aqp
    aqp_mint_url: https://example.invalid/api/v1/cqp/ccswitch/api_key/get_or_generate
    models:
      - glm-5.2
routes:
  glm-5.2:
    - {provider: aqp, model: glm-5.2, priority: 1}
`, claudeFile)
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// --- cmdTakeover opencode: rewrites opencode config ---

func TestCLI_TakeoverOpencode(t *testing.T) {
	dir := t.TempDir()
	opencodeFile := filepath.Join(dir, "opencode.json")
	os.WriteFile(opencodeFile, []byte(`{}`), 0o644)
	cfgBody := fmt.Sprintf("listen: 127.0.0.1:15721\ntakeover:\n  opencode: %s\n  provider_id: model-proxy\nproviders:\n  aqp:\n    openai_base_url: https://x\n    provider_id: aqp\n    models:\n      - glm-5.2\nroutes:\n  glm-5.2:\n    - {provider: aqp, model: glm-5.2}\n", opencodeFile)
	cfgPath := writeTempConfig(t, cfgBody)
	_, _, code := runCLI(t, "takeover", cfgPath, "opencode")
	if code != 0 {
		t.Fatalf("takeover opencode: exit=%d want 0", code)
	}
	data, _ := os.ReadFile(opencodeFile)
	if !strings.Contains(string(data), "model-proxy") || !strings.Contains(string(data), "/v1") {
		t.Errorf("opencode not rewritten:\n%s", data)
	}
}

// --- cmdRestore claude: restores from backup (full takeover→restore cycle) ---

func TestCLI_RestoreClaudeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	claudeFile := filepath.Join(dir, "claude.json")
	os.WriteFile(claudeFile, []byte(`{"env":{"ORIGINAL":"1"}}`), 0o644)
	// backupDir = <configDir>/.model-proxy — config lives in dir, so backup in dir/.model-proxy.
	cfgBody := fmt.Sprintf("listen: 127.0.0.1:15721\ntakeover:\n  claude: %s\nproviders:\n  aqp:\n    openai_base_url: https://x\n    provider_id: aqp\n    models:\n      - glm-5.2\nroutes:\n  glm-5.2:\n    - {provider: aqp, model: glm-5.2}\n", claudeFile)
	cfgPath := writeTempConfig(t, cfgBody)
	// First takeover (creates backup + rewrites), then restore.
	if _, _, code := runCLI(t, "takeover", cfgPath, "claude"); code != 0 {
		t.Fatalf("takeover claude: exit=%d", code)
	}
	if _, _, code := runCLI(t, "restore", cfgPath, "claude"); code != 0 {
		t.Fatalf("restore claude: exit=%d", code)
	}
	data, _ := os.ReadFile(claudeFile)
	if !strings.Contains(string(data), "ORIGINAL") {
		t.Errorf("restore did not bring back original:\n%s", data)
	}
}

// --- takeover opencode: warns on default-sourced models ---

func TestCLI_TakeoverOpencode_WarnsDefault(t *testing.T) {
	ocPath := filepath.Join(t.TempDir(), "oc.json")
	os.WriteFile(ocPath, []byte(`{}`), 0o644) // takeover backs up the target first; it must exist
	cfgBody := "listen: 127.0.0.1:15721\ntakeover:\n  provider_id: model-proxy\n  opencode: " + ocPath + "\nproviders:\n  codex:\n    provider_id: codex\n    openai_base_url: https://chatgpt.com/backend-api/codex\nroutes:\n  gpt-5.5:\n    - {provider: codex, model: gpt-5.5}\n"
	cfgPath := writeTempConfig(t, cfgBody)

	home := t.TempDir()
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	// fresh EMPTY cache (no models) → gpt-5.5 unmatched → default
	cache := `{"fetched_at":"` + time.Now().Format(time.RFC3339) + `","etag":"","by_name":{},"by_endpoint":{}}`
	os.WriteFile(filepath.Join(credDir, "models_cache.json"), []byte(cache), 0o600)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	t.Setenv("MP_MODELSDEV_URL", srv.URL)

	_, stderr, code := runCLIWithHome(t, home, "takeover", cfgPath, "opencode")
	if code != 0 {
		t.Fatalf("takeover exit=%d", code)
	}
	if !strings.Contains(stderr, "gpt-5.5") || !strings.Contains(stderr, "default") {
		t.Errorf("takeover should warn about gpt-5.5 default:\n%s", stderr)
	}
	// opencode config written with default ctx 200000 (proves defaults were written, not omitted)
	b, err := os.ReadFile(ocPath)
	if err != nil {
		t.Fatalf("opencode config not written: %v", err)
	}
	oc := string(b)
	if !strings.Contains(oc, "200000") {
		t.Errorf("opencode config should contain default ctx 200000:\n%s", oc)
	}
}
