package cli

import (
	"fmt"
	"model-proxy/internal/cli/clitest"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	observeseclog "model-proxy/internal/observe/seclog"
)

// This file extends the subprocess pattern (subprocess_test_support_test.go)
// to the takeover/restore CLI handlers that call log.Fatal / os.Exit:
// RunTakeover, RunRestore.
//
// The subprocess dispatcher (TestHelperProcess) covers "takeover" and
// "restore" cases; runCLI pins HOME to a fresh temp dir (isolation), and
// runCLIWithHome lets a test pin HOME to a pre-populated dir.

// -- T1: `takeover claude` backs up the original and rewrites it ---

func TestCLI_TakeoverClaude(t *testing.T) {
	dir := t.TempDir()
	claudeFile := filepath.Join(dir, "claude.json")
	original := `{"env":{"FOO":"bar"}}`
	if err := os.WriteFile(claudeFile, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := writeTakeoverConfig(t, dir, claudeFile)

	stdout, _, code := clitest.RunCLI(t, "takeover", cfgPath, "claude")
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

	stdout, _, code := clitest.RunCLI(t, "restore", cfgPath, "claude")
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
	_, _, code := clitest.RunCLI(t, "takeover", cfgPath, "nope")
	// RunTakeover on an unknown client is a no-op (listClients returns nil →
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

// --- takeover opencode: rewrites opencode config ---

func TestCLI_TakeoverOpencode(t *testing.T) {
	dir := t.TempDir()
	opencodeFile := filepath.Join(dir, "opencode.json")
	if err := os.WriteFile(opencodeFile, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgBody := fmt.Sprintf("listen: 127.0.0.1:15721\ntakeover:\n  opencode: %s\n  provider_id: model-proxy\nproviders:\n  aqp:\n    openai_base_url: https://x\n    provider_id: aqp\n    models:\n      - glm-5.2\nroutes:\n  glm-5.2:\n    - {provider: aqp, model: glm-5.2}\n", opencodeFile)
	cfgPath := clitest.WriteTempConfig(t, cfgBody)
	_, _, code := clitest.RunCLI(t, "takeover", cfgPath, "opencode")
	if code != 0 {
		t.Fatalf("takeover opencode: exit=%d want 0", code)
	}
	data, err := os.ReadFile(opencodeFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "model-proxy") || !strings.Contains(string(data), "/v1") {
		t.Errorf("opencode not rewritten:\n%s", data)
	}
}

// --- restore claude: restores from backup (full takeover→restore cycle) ---

func TestCLI_RestoreClaudeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	claudeFile := filepath.Join(dir, "claude.json")
	if err := os.WriteFile(claudeFile, []byte(`{"env":{"ORIGINAL":"1"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// backupDir = <configDir>/.model-proxy — config lives in dir, so backup in dir/.model-proxy.
	cfgBody := fmt.Sprintf("listen: 127.0.0.1:15721\ntakeover:\n  claude: %s\nproviders:\n  aqp:\n    openai_base_url: https://x\n    provider_id: aqp\n    models:\n      - glm-5.2\nroutes:\n  glm-5.2:\n    - {provider: aqp, model: glm-5.2}\n", claudeFile)
	cfgPath := clitest.WriteTempConfig(t, cfgBody)
	// First takeover (creates backup + rewrites), then restore.
	if _, _, code := clitest.RunCLI(t, "takeover", cfgPath, "claude"); code != 0 {
		t.Fatalf("takeover claude: exit=%d", code)
	}
	if _, _, code := clitest.RunCLI(t, "restore", cfgPath, "claude"); code != 0 {
		t.Fatalf("restore claude: exit=%d", code)
	}
	data, err := os.ReadFile(claudeFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "ORIGINAL") {
		t.Errorf("restore did not bring back original:\n%s", data)
	}
	// Restore ends the takeover: the .bak marker is removed, so the drift
	// check treats the client as not taken over and a future takeover takes
	// a fresh backup. (backupDir = <configDir>/.model-proxy)
	bakDir := filepath.Join(filepath.Dir(cfgPath), ".model-proxy")
	if _, err := os.Stat(filepath.Join(bakDir, "claude.bak")); !os.IsNotExist(err) {
		t.Errorf("restore left the takeover marker behind (stat err=%v)", err)
	}
}

// --- takeover opencode: warns on default-sourced models ---

func TestCLI_TakeoverOpencode_WarnsDefault(t *testing.T) {
	ocPath := filepath.Join(t.TempDir(), "oc.json")
	os.WriteFile(ocPath, []byte(`{}`), 0o644) // takeover backs up the target first; it must exist
	cfgBody := "listen: 127.0.0.1:15721\ntakeover:\n  provider_id: model-proxy\n  opencode: " + ocPath + "\nproviders:\n  codex:\n    provider_id: codex\n    openai_base_url: https://chatgpt.com/backend-api/codex\nroutes:\n  gpt-5.5:\n    - {provider: codex, model: gpt-5.5}\n"
	cfgPath := clitest.WriteTempConfig(t, cfgBody)

	home := t.TempDir()
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	// fresh EMPTY cache (no models) → gpt-5.5 unmatched → default
	cache := `{"fetched_at":"` + time.Now().Format(time.RFC3339) + `","etag":"","by_name":{},"by_endpoint":{}}`
	if err := os.WriteFile(filepath.Join(credDir, "models_cache.json"), []byte(cache), 0o600); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	t.Setenv("MP_MODELSDEV_URL", srv.URL)

	_, stderr, code := clitest.RunCLIWithHome(t, home, "takeover", cfgPath, "opencode")
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

// --- post-takeover drift verification (优化项 9) ---

// driftSceneConfig writes a config with claude (file present) and codex (file
// missing, but a pre-seeded .bak marker simulates a prior takeover whose
// client config later vanished). backupDir = <configDir>/.model-proxy.
func driftSceneConfig(t *testing.T, dir, claudeFile, codexFile, extra string) string {
	t.Helper()
	body := fmt.Sprintf(`listen: 127.0.0.1:15721
%s
takeover:
  proxy_url: http://127.0.0.1:15721
  claude: %s
  codex: %s
providers:
  aqp:
    openai_base_url: https://x
    provider_id: aqp
    models:
      - glm-5.2
routes:
  glm-5.2:
    - {provider: aqp, model: glm-5.2}
`, extra, claudeFile, codexFile)
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// setupDriftScene builds the claude-ok / codex-drift scene and returns the
// config path. A local 500 models.dev endpoint keeps the `all` metadata
// hydrate offline; the codex .bak marker makes the drift check treat codex
// as taken over even though its config file is gone.
func setupDriftScene(t *testing.T, extra string) (cfgPath, home string) {
	t.Helper()
	dir := t.TempDir()
	claudeFile := filepath.Join(dir, "claude.json")
	if err := os.WriteFile(claudeFile, []byte(`{"env":{"FOO":"bar"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	codexFile := filepath.Join(dir, "codex.toml") // intentionally not created
	bakDir := filepath.Join(dir, ".model-proxy")
	if err := os.MkdirAll(bakDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bakDir, "codex.bak"), []byte("model_provider = \"openai\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	t.Cleanup(srv.Close)
	t.Setenv("MP_MODELSDEV_URL", srv.URL)
	return driftSceneConfig(t, dir, claudeFile, codexFile, extra), t.TempDir()
}

// securityLogFiles lists the security-*.log files under <home>/.model-proxy
// (nil when the dir does not exist — nothing was ever appended).
func securityLogFiles(t *testing.T, home string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(home, ".model-proxy"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "security-") {
			out = append(out, e.Name())
		}
	}
	return out
}

// TestCLI_TakeoverKimiNoDriftWarning: a kimi takeover writes a pointer
// TakeoverPointer can read back — the post-write drift check must stay
// silent (before the kimi case existed, every kimi takeover warned about
// drift forever). Regression for the missing "kimi" drift case.
func TestCLI_TakeoverKimiNoDriftWarning(t *testing.T) {
	dir := t.TempDir()
	kimiFile := filepath.Join(dir, "kimi.toml")
	if err := os.WriteFile(kimiFile, []byte("[providers.\"existing\"]\ntype = \"kimi\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgBody := fmt.Sprintf(`listen: 127.0.0.1:15721
takeover:
  proxy_url: http://127.0.0.1:15721
  kimi: %s
providers:
  aqp:
    openai_base_url: https://x
    provider_id: aqp
    models:
      - glm-5.2
routes:
  glm-5.2:
    - {provider: aqp, model: glm-5.2, priority: 1}
`, kimiFile)
	cfgPath := clitest.WriteTempConfig(t, cfgBody)
	home := t.TempDir()

	_, stderr, code := clitest.RunCLIWithHome(t, home, "takeover", cfgPath, "kimi")
	if code != 0 {
		t.Fatalf("takeover kimi exit=%d want 0\n--- stderr ---\n%s", code, stderr)
	}
	if strings.Contains(stderr, "drift") {
		t.Errorf("healthy kimi takeover must not warn about drift:\n%s", stderr)
	}
	if files := securityLogFiles(t, home); len(files) != 0 {
		t.Errorf("no drift → no security audit log, got %v", files)
	}
	// The rewritten file carries the schema-correct quoted model block.
	b, err := os.ReadFile(kimiFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `[models."glm-5.2"]`) || !strings.Contains(string(b), "max_context_size") {
		t.Errorf("kimi config missing quoted model block with max_context_size:\n%s", b)
	}
}

// TestCLI_TakeoverClaudeNoDriftWarning: a healthy takeover passes the
// post-write drift check — no warning line, no seclog drift record.
func TestCLI_TakeoverClaudeNoDriftWarning(t *testing.T) {
	dir := t.TempDir()
	claudeFile := filepath.Join(dir, "claude.json")
	if err := os.WriteFile(claudeFile, []byte(`{"env":{"FOO":"bar"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := writeTakeoverConfig(t, dir, claudeFile)
	home := t.TempDir()

	_, stderr, code := clitest.RunCLIWithHome(t, home, "takeover", cfgPath, "claude")
	if code != 0 {
		t.Fatalf("takeover claude exit=%d want 0\n--- stderr ---\n%s", code, stderr)
	}
	if strings.Contains(stderr, "drift") {
		t.Errorf("healthy takeover must not warn about drift:\n%s", stderr)
	}
	if files := securityLogFiles(t, home); len(files) != 0 {
		t.Errorf("no drift → no security audit log, got %v", files)
	}
}

// TestCLI_TakeoverDriftWarnsAndAudits: `takeover all` rewrites claude fine,
// but codex (taken over earlier, config file since vanished) still drifts
// after the run → stderr warning + one seclog drift record (agent=takeover).
// The drift must not fail the command: exit stays 0.
func TestCLI_TakeoverDriftWarnsAndAudits(t *testing.T) {
	cfgPath, home := setupDriftScene(t, "")

	_, stderr, code := clitest.RunCLIWithHome(t, home, "takeover", cfgPath, "all")
	if code != 0 {
		t.Fatalf("takeover all exit=%d want 0\n--- stderr ---\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "codex drift detected right after takeover") ||
		!strings.Contains(stderr, "(file missing)") {
		t.Errorf("missing drift warning for codex:\n%s", stderr)
	}
	if strings.Contains(stderr, "claude drift") {
		t.Errorf("claude was rewritten successfully and must not drift:\n%s", stderr)
	}

	result, err := observeseclog.Query(filepath.Join(home, ".model-proxy"),
		observeseclog.Filter{Kind: observeseclog.KindDrift})
	if err != nil {
		t.Fatalf("query audit log: %v", err)
	}
	if len(result.Records) != 1 {
		t.Fatalf("drift records = %d, want exactly 1 (only codex drifted)", len(result.Records))
	}
	rec := result.Records[0]
	if rec.Agent != "takeover" || rec.Kind != observeseclog.KindDrift {
		t.Errorf("record kind/agent = %q/%q, want drift/takeover", rec.Kind, rec.Agent)
	}
	if !strings.Contains(rec.Detail, "client=codex") {
		t.Errorf("detail missing client=codex: %q", rec.Detail)
	}
}

// TestCLI_TakeoverDriftAuditDisabled: guard.audit=false suppresses the seclog
// append; the stderr drift warning still appears.
func TestCLI_TakeoverDriftAuditDisabled(t *testing.T) {
	cfgPath, home := setupDriftScene(t, "guard:\n  audit: false\n")

	_, stderr, code := clitest.RunCLIWithHome(t, home, "takeover", cfgPath, "all")
	if code != 0 {
		t.Fatalf("takeover all exit=%d want 0\n--- stderr ---\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "codex drift detected right after takeover") {
		t.Errorf("audit off must not silence the drift warning:\n%s", stderr)
	}
	if files := securityLogFiles(t, home); len(files) != 0 {
		t.Errorf("audit disabled but security log written: %v", files)
	}
}
