package takeover_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pointer_test.go covers Template.Pointer — doctor's template-driven takeover
// drift probe — for the TOML templates (codex top-key selector + section,
// kimi bare section) and the JSON drift_path templates.

// TestPointerCodex: ok only when model_provider selects our section AND the
// section's base_url equals the proxy URL.
func TestPointerCodex(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.toml")
	cfg := baseCfg() // Listen 127.0.0.1:15721 → expected bare proxy URL
	good := `model_provider = "model-proxy"

[model_providers."model-proxy"]
name = "model-proxy"
base_url = "http://127.0.0.1:15721"
wire_api = "responses"
`
	if err := os.WriteFile(file, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	tpl := presetFor(t, "codex", file)
	if cur, exp := tpl.Pointer(cfg); cur != exp {
		t.Errorf("good config: current=%q expected=%q, want equal", cur, exp)
	}

	// model_provider switched away (e.g. user edited back to openai).
	bad := strings.Replace(good, `model_provider = "model-proxy"`, `model_provider = "openai"`, 1)
	os.WriteFile(file, []byte(bad), 0o600)
	if cur, _ := tpl.Pointer(cfg); cur != `model_provider = "openai"` {
		t.Errorf("wrong model_provider: current=%q", cur)
	}

	// Section intact but base_url stale (listen port changed).
	stale := strings.Replace(good, `base_url = "http://127.0.0.1:15721"`, `base_url = "http://127.0.0.1:9999"`, 1)
	os.WriteFile(file, []byte(stale), 0o600)
	if cur, exp := tpl.Pointer(cfg); cur != "http://127.0.0.1:9999" || exp != "http://127.0.0.1:15721" {
		t.Errorf("stale base_url: current=%q expected=%q", cur, exp)
	}

	tpl.File = filepath.Join(dir, "nope.toml")
	if cur, _ := tpl.Pointer(cfg); cur != "(file missing)" {
		t.Errorf("missing file: current=%q", cur)
	}
}

// TestPointerKimi: the pointer is the base_url inside the [providers."<pid>"]
// section the kimi template writes, expected to equal the versioned proxy
// endpoint (proxyURL + /v1).
func TestPointerKimi(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "config.toml")
	cfg := baseCfg()

	// Produce the file with the real template rewrite, not a hand-written copy.
	if err := os.WriteFile(file, []byte(""), 0o600); err != nil {
		t.Fatal(err)
	}
	tpl := presetFor(t, "kimi", file)
	if err := tpl.Rewrite(cfg, nil, nil); err != nil {
		t.Fatal(err)
	}

	cur, exp := tpl.Pointer(cfg)
	if exp != "http://127.0.0.1:15721/v1" {
		t.Errorf("expected=%q, want http://127.0.0.1:15721/v1", exp)
	}
	if cur != exp {
		t.Errorf("after rewrite: current=%q expected=%q, want equal (no drift)", cur, exp)
	}

	// Point the provider elsewhere → drift.
	data, _ := os.ReadFile(file)
	os.WriteFile(file, []byte(strings.Replace(string(data), `base_url = "http://127.0.0.1:15721/v1"`, `base_url = "http://127.0.0.1:9999/v1"`, 1)), 0o600)
	if cur, exp := tpl.Pointer(cfg); cur == exp {
		t.Errorf("stale base_url must drift: current=%q expected=%q", cur, exp)
	}

	// Missing file → placeholder current, which can never equal expected.
	tpl.File = filepath.Join(dir, "nope.toml")
	if cur, _ := tpl.Pointer(cfg); cur != "(file missing)" {
		t.Errorf("missing file: current=%q", cur)
	}
}

// TestPointerJSON: the drift_path templates (claude bare, opencode /v1) read
// their pointer from the declared JSON path.
func TestPointerJSON(t *testing.T) {
	dir := t.TempDir()
	cfg := baseCfg()

	claudeFile := filepath.Join(dir, "claude.json")
	os.WriteFile(claudeFile, []byte(`{"env":{"ANTHROPIC_BASE_URL":"http://127.0.0.1:15721"}}`), 0o600)
	if cur, exp := presetFor(t, "claude", claudeFile).Pointer(cfg); cur != exp || exp != "http://127.0.0.1:15721" {
		t.Errorf("claude pointer: current=%q expected=%q", cur, exp)
	}

	ocFile := filepath.Join(dir, "opencode.json")
	os.WriteFile(ocFile, []byte(`{"provider":{"model-proxy":{"options":{"baseURL":"http://127.0.0.1:9999/v1"}}}}`), 0o600)
	if cur, exp := presetFor(t, "opencode", ocFile).Pointer(cfg); cur != "http://127.0.0.1:9999/v1" || exp != "http://127.0.0.1:15721/v1" {
		t.Errorf("opencode pointer: current=%q expected=%q", cur, exp)
	}
}

// TestPointerEnv: the env-format template (gemini-cli) reads its pointer from
// the managed KEY whose rendered value is the base URL.
func TestPointerEnv(t *testing.T) {
	dir := t.TempDir()
	cfg := baseCfg()
	file := filepath.Join(dir, ".env")
	os.WriteFile(file, []byte("GOOGLE_GEMINI_BASE_URL=http://127.0.0.1:15721/v1\nGEMINI_API_KEY=PROXY_MANAGED\n"), 0o600)
	if cur, exp := presetFor(t, "gemini-cli", file).Pointer(cfg); cur != exp || exp != "http://127.0.0.1:15721/v1" {
		t.Errorf("env pointer: current=%q expected=%q", cur, exp)
	}
	os.WriteFile(file, []byte("GEMINI_API_KEY=PROXY_MANAGED\n"), 0o600)
	if cur, _ := presetFor(t, "gemini-cli", file).Pointer(cfg); cur != "(missing)" {
		t.Errorf("env pointer missing key: current=%q", cur)
	}
}
