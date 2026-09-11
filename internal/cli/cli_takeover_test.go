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
// runCLIWithHome lets a test pin HOME to a pre-populated dir. Client targets
// come from user template overrides in <home>/.model-proxy/takeover-templates
// (the same mechanism production resolves).

// writeTakeoverTemplates writes user template overrides into
// <home>/.model-proxy/takeover-templates for the given name→target-file map.
// Bodies mirror the embedded presets (the override REPLACES the preset).
func writeTakeoverTemplates(t *testing.T, home string, files map[string]string) {
	t.Helper()
	dir := filepath.Join(home, ".model-proxy", "takeover-templates")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	bodies := map[string]func(file string) string{
		"claude": func(f string) string {
			return "file: " + f + `
format: json
json:
  set:
    env.ANTHROPIC_BASE_URL: "{{base_url}}"
    env.ANTHROPIC_AUTH_TOKEN: "{{token}}"
  drift_path: env.ANTHROPIC_BASE_URL
`
		},
		"opencode": func(f string) string {
			return "file: " + f + `
format: json
client: opencode
protocol: anthropic
base_url: v1
json:
  set:
    provider.{{provider_id}}:
      name: "model-proxy"
      npm: "@ai-sdk/anthropic"
      options: {apiKey: "{{token}}", baseURL: "{{base_url}}"}
  drift_path: provider.{{provider_id}}.options.baseURL
models:
  shape: opencode
  json_path: provider.{{provider_id}}.models
`
		},
		"opencode-openai": func(f string) string {
			return "file: " + f + `
format: json
client: opencode
protocol: openai
base_url: v1
provider_id: model-proxy-openai
json:
  set:
    provider.{{provider_id}}:
      name: "model-proxy (openai-compatible)"
      npm: "@ai-sdk/openai-compatible"
      options: {apiKey: "{{token}}", baseURL: "{{base_url}}"}
  drift_path: provider.{{provider_id}}.options.baseURL
models:
  shape: opencode
  json_path: provider.{{provider_id}}.models
`
		},
		"opencode-responses": func(f string) string {
			return "file: " + f + `
format: json
client: opencode
protocol: responses
base_url: v1
provider_id: model-proxy-responses
json:
  set:
    provider.{{provider_id}}:
      name: "model-proxy (responses)"
      npm: "@ai-sdk/openai"
      options: {apiKey: "{{token}}", baseURL: "{{base_url}}"}
  drift_path: provider.{{provider_id}}.options.baseURL
models:
  shape: opencode
  json_path: provider.{{provider_id}}.models
`
		},
		"codex": func(f string) string {
			return "file: " + f + `
format: toml
base_url: bare
toml:
  top_keys:
    model_provider: '"{{provider_id}}"'
  sections:
    - name: 'model_providers."{{provider_id}}"'
      body: |
        name = "model-proxy"
        base_url = "{{base_url}}"
        wire_api = "responses"
        requires_openai_auth = true
`
		},
		"kimi": func(f string) string {
			return "file: " + f + `
format: toml
base_url: v1
toml:
  sections:
    - name: 'providers."{{provider_id}}"'
      body: |
        type = "openai_legacy"
        base_url = "{{base_url}}"
        api_key = "{{token}}"
models:
  shape: kimi
  toml_section: 'models."{{model.id}}"'
  toml_body: |
    provider = "{{provider_id}}"
    model = "{{model.id}}"
    max_context_size = {{model.context}}
  also_remove: 'models.{{model.id}}'
`
		},
	}
	for name, f := range files {
		body, ok := bodies[name]
		if !ok {
			t.Fatalf("no override body for %s", name)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body(f)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// writePlainConfig writes a valid takeover-less config into dir/config.yaml
// (backupDir = <configDir>/.model-proxy; proxy_url defaults to http://listen).
func writePlainConfig(t *testing.T, dir string) string {
	t.Helper()
	body := `listen: 127.0.0.1:15721
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
`
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// -- T1: `takeover claude` backs up the original and rewrites it ---

func TestCLI_TakeoverClaude(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	claudeFile := filepath.Join(dir, "claude.json")
	original := `{"env":{"FOO":"bar"}}`
	if err := os.WriteFile(claudeFile, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := writePlainConfig(t, dir)
	writeTakeoverTemplates(t, home, map[string]string{"claude": claudeFile})

	stdout, _, code := clitest.RunCLIWithHome(t, home, "takeover", cfgPath, "claude")
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
	home := t.TempDir()
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
	cfgPath := writePlainConfig(t, dir)
	writeTakeoverTemplates(t, home, map[string]string{"claude": claudeFile})

	stdout, _, code := clitest.RunCLIWithHome(t, home, "restore", cfgPath, "claude")
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

// -- T3: `takeover <unknown>` exits non-zero (unknown template is a hard error) ---

func TestCLI_TakeoverUnknownClient(t *testing.T) {
	dir := t.TempDir()
	cfgPath := writePlainConfig(t, dir)
	_, _, code := clitest.RunCLI(t, "takeover", cfgPath, "nope")
	// An unknown client name fails template resolution (log.Fatal) — a typo
	// must never no-op silently.
	if code == 0 {
		t.Error("takeover nope: exit=0, want non-zero (unknown template is a hard error)")
	}
}

// --- takeover opencode: rewrites opencode config ---

func TestCLI_TakeoverOpencode(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	opencodeFile := filepath.Join(dir, "opencode.json")
	if err := os.WriteFile(opencodeFile, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := writePlainConfig(t, dir)
	writeTakeoverTemplates(t, home, map[string]string{"opencode": opencodeFile})
	_, _, code := clitest.RunCLIWithHome(t, home, "takeover", cfgPath, "opencode")
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
	home := t.TempDir()
	claudeFile := filepath.Join(dir, "claude.json")
	if err := os.WriteFile(claudeFile, []byte(`{"env":{"ORIGINAL":"1"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := writePlainConfig(t, dir)
	writeTakeoverTemplates(t, home, map[string]string{"claude": claudeFile})
	// First takeover (creates backup + rewrites), then restore.
	if _, _, code := clitest.RunCLIWithHome(t, home, "takeover", cfgPath, "claude"); code != 0 {
		t.Fatalf("takeover claude: exit=%d", code)
	}
	if _, _, code := clitest.RunCLIWithHome(t, home, "restore", cfgPath, "claude"); code != 0 {
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
	dir := t.TempDir()
	ocPath := filepath.Join(dir, "oc.json")
	os.WriteFile(ocPath, []byte(`{}`), 0o644) // takeover backs up the target first; it must exist
	cfgBody := "listen: 127.0.0.1:15721\nproviders:\n  codex:\n    provider_id: codex\n    openai_base_url: https://chatgpt.com/backend-api/codex\nroutes:\n  gpt-5.5:\n    - {provider: codex, model: gpt-5.5}\n"
	cfgPath := clitest.WriteTempConfig(t, cfgBody)

	home := t.TempDir()
	writeTakeoverTemplates(t, home, map[string]string{"opencode-responses": ocPath})
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

// setupDriftScene builds the claude-ok / codex-drift scene and returns the
// config path + home. A local 500 models.dev endpoint keeps the `all`
// metadata hydrate offline; the codex .bak marker makes the drift check
// treat codex as taken over even though its config file is gone.
func setupDriftScene(t *testing.T, extra string) (cfgPath, home string) {
	t.Helper()
	dir := t.TempDir()
	home = t.TempDir()
	claudeFile := filepath.Join(dir, "claude.json")
	if err := os.WriteFile(claudeFile, []byte(`{"env":{"FOO":"bar"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	codexFile := filepath.Join(dir, "codex.toml") // intentionally not created
	writeTakeoverTemplates(t, home, map[string]string{"claude": claudeFile, "codex": codexFile})
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

	body := fmt.Sprintf(`listen: 127.0.0.1:15721
%s
providers:
  aqp:
    openai_base_url: https://x
    provider_id: aqp
    models:
      - glm-5.2
routes:
  glm-5.2:
    - {provider: aqp, model: glm-5.2}
`, extra)
	cfgPath = filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return cfgPath, home
}

// securityLogFiles lists the security-*.log files under the default audit
// dir <home>/.model-proxy/log/security (nil when the dir does not exist —
// nothing was ever appended).
func securityLogFiles(t *testing.T, home string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(home, ".model-proxy", "log", "security"))
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

// TestCLI_TakeoverKimiNoDriftWarning: a kimi takeover writes a pointer the
// template Pointer can read back — the post-write drift check must stay
// silent (before the kimi case existed, every kimi takeover warned about
// drift forever). Regression for the missing "kimi" drift case.
func TestCLI_TakeoverKimiNoDriftWarning(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	kimiFile := filepath.Join(dir, "kimi.toml")
	if err := os.WriteFile(kimiFile, []byte("[providers.\"existing\"]\ntype = \"kimi\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := writePlainConfig(t, dir)
	writeTakeoverTemplates(t, home, map[string]string{"kimi": kimiFile})

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
	home := t.TempDir()
	claudeFile := filepath.Join(dir, "claude.json")
	if err := os.WriteFile(claudeFile, []byte(`{"env":{"FOO":"bar"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := writePlainConfig(t, dir)
	writeTakeoverTemplates(t, home, map[string]string{"claude": claudeFile})

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

	result, err := observeseclog.Query(filepath.Join(home, ".model-proxy", "log", "security"),
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

// TestCLI_TakeoverListShowsProtocolSelection: `takeover list` prints every
// template with family + protocol columns and marks (*) the variant
// auto-selected for each multi-variant family — here an openai-only provider
// pulls the pi family to pi-openai, not the anthropic default.
func TestCLI_TakeoverListShowsProtocolSelection(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	body := `listen: 127.0.0.1:15721
providers:
  zhipu:
    openai_base_url: https://example.invalid/api/paas/v4
    provider_id: zhipu
    models:
      - glm-5.3
`
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, stderr, code := clitest.RunCLIWithHome(t, home, "takeover", cfgPath, "list")
	if code != 0 {
		t.Fatalf("takeover list exit=%d want 0\n--- stderr ---\n%s", code, stderr)
	}
	for _, want := range []string{"* pi-openai", "pi-responses", "* claude", "anthropic", "openai", "auto-selected"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("takeover list output missing %q:\n%s", want, stdout)
		}
	}
	// The anthropic default variant of the pi family must be unmarked here
	// ("* pi  " with padding — "* pi-openai" has no double space after "pi").
	if strings.Contains(stdout, "* pi  ") {
		t.Errorf("openai-native config must not mark the anthropic pi variant:\n%s", stdout)
	}
}

// TestCLI_TakeoverModeSplit: --mode split writes one provider entry per
// natively-spoken protocol and partitions the models among them.
func TestCLI_TakeoverModeSplit(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	piFile := filepath.Join(home, ".pi", "agent", "models.json")
	if err := os.MkdirAll(filepath.Dir(piFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(piFile, []byte(`{"providers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	body := `listen: 127.0.0.1:15721
providers:
  zhipu:
    anthropic_base_url: https://example.invalid/api/anthropic
    provider_id: zhipu
    models:
      - glm-5.3
  codex:
    openai_base_url: https://example.invalid/backend-api/codex
    provider_id: codex
    models:
      - gpt-5.4-mini
`
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := clitest.RunCLIWithHome(t, home, "takeover", cfgPath, "pi", "--mode", "split")
	if code != 0 {
		t.Fatalf("takeover --mode split pi exit=%d want 0\n--- stderr ---\n%s", code, stderr)
	}
	data, err := os.ReadFile(piFile)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{`"model-proxy"`, `"model-proxy-responses"`, `"anthropic-messages"`, `"openai-responses"`, `"glm-5.3"`, `"gpt-5.4-mini"`} {
		if !strings.Contains(text, want) {
			t.Errorf("split result missing %s:\n%s", want, text)
		}
	}
	// The openai variant had no exclusively-openai models → no entry.
	if strings.Contains(text, "model-proxy-openai") {
		t.Errorf("empty openai variant must not be written:\n%s", text)
	}
	// Models partitioned, not duplicated.
	if n := strings.Count(text, `"id": "glm-5.3"`); n != 1 {
		t.Errorf("glm-5.3 appears %d times, want 1:\n%s", n, text)
	}
	if !strings.Contains(stderr, "split by native protocol") {
		t.Errorf("selection note missing from log:\n%s", stderr)
	}
}

// TestCLI_TakeoverModeInvalid: a bogus --mode value fails fast.
func TestCLI_TakeoverModeInvalid(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	cfgPath := writePlainConfig(t, dir)

	_, stderr, code := clitest.RunCLIWithHome(t, home, "takeover", cfgPath, "pi", "--mode", "bogus")
	if code == 0 {
		t.Fatalf("takeover --mode bogus exit=0, want non-zero")
	}
	if !strings.Contains(stderr, "unknown --mode") {
		t.Errorf("want unknown-mode error, got:\n%s", stderr)
	}
}

// TestCLI_TakeoverModeProtocol: --mode <protocol> unifies the family into
// that protocol's variant even when the routes are natively another
// protocol; the log explains what will ride conversion.
func TestCLI_TakeoverModeProtocol(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	piFile := filepath.Join(home, ".pi", "agent", "models.json")
	if err := os.MkdirAll(filepath.Dir(piFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(piFile, []byte(`{"providers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "config.yaml")
	body := `listen: 127.0.0.1:15721
providers:
  zhipu:
    anthropic_base_url: https://example.invalid/api/anthropic
    provider_id: zhipu
    models:
      - glm-5.3
`
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	_, stderr, code := clitest.RunCLIWithHome(t, home, "takeover", cfgPath, "pi", "--mode", "openai")
	if code != 0 {
		t.Fatalf("takeover --mode openai pi exit=%d want 0\n--- stderr ---\n%s", code, stderr)
	}
	data, err := os.ReadFile(piFile)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, `"model-proxy-openai"`) || !strings.Contains(text, `"openai-completions"`) {
		t.Errorf("protocol mode must write the openai variant:\n%s", text)
	}
	if strings.Contains(text, `"anthropic-messages"`) {
		t.Errorf("pinned openai must not also write the anthropic variant:\n%s", text)
	}
	if !strings.Contains(stderr, "pinned by --mode") || !strings.Contains(stderr, "conversion needed for: glm-5.3") {
		t.Errorf("log must explain the pin and its conversion cost:\n%s", stderr)
	}
}
