package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// takeover_test.go covers the client-config rewrite functions (rewriteClaude,
// rewriteOpencode, rewritePi, rewriteCodex) and their TOML helpers. These are
// pure file/string operations against the takeover target files — fully
// testable with temp dirs.

func testTakeoverConfig(t *testing.T, dir string) *Config {
	return &Config{
		Providers: map[string]Provider{
			"aqp": {
				OpenAIBaseURL: "http://x", Provider: "aqp",
				Models: []string{"glm-5.2"},
			},
		},
		Routes: map[string][]RouteTarget{
			"glm-5.2": {{Provider: "aqp", Model: "glm-5.2", Priority: 1}},
		},
		Takeover: Takeover{
			ProxyURL:   "http://127.0.0.1:15721",
			Claude:     filepath.Join(dir, "claude.json"),
			Opencode:   filepath.Join(dir, "opencode.json"),
			Codex:      filepath.Join(dir, "codex.toml"),
			Pi:         filepath.Join(dir, "pi.json"),
			ProviderID: "model-proxy",
		},
	}
}

// --- rewriteClaude: sets ANTHROPIC_BASE_URL + ANTHROPIC_AUTH_TOKEN ---

func TestRewriteClaude(t *testing.T) {
	dir := t.TempDir()
	cfg := testTakeoverConfig(t, dir)
	// Start with an existing settings.json (possibly with other env keys).
	os.WriteFile(cfg.Takeover.Claude, []byte(`{"env":{"OTHER":"x"},"theme":"dark"}`), 0o644)

	if err := rewriteClaude(cfg); err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	b, _ := os.ReadFile(cfg.Takeover.Claude)
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	env, _ := v["env"].(map[string]any)
	if env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:15721" {
		t.Errorf("ANTHROPIC_BASE_URL=%v want http://127.0.0.1:15721", env["ANTHROPIC_BASE_URL"])
	}
	if env["ANTHROPIC_AUTH_TOKEN"] != "PROXY_MANAGED" {
		t.Errorf("ANTHROPIC_AUTH_TOKEN=%v want PROXY_MANAGED", env["ANTHROPIC_AUTH_TOKEN"])
	}
	if env["OTHER"] != "x" { // existing keys preserved
		t.Errorf("OTHER=%v want x (existing env should be preserved)", env["OTHER"])
	}
	if v["theme"] != "dark" {
		t.Errorf("theme=%v want dark (non-env keys preserved)", v["theme"])
	}
}

// --- rewriteClaude on a missing file: creates a new one (or errors predictably) ---

func TestRewriteClaude_NewFile(t *testing.T) {
	dir := t.TempDir()
	cfg := testTakeoverConfig(t, dir)
	if err := rewriteClaude(cfg); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(cfg.Takeover.Claude)
	if err != nil {
		t.Fatalf("claude file not created: %v", err)
	}
	if !strings.Contains(string(b), "ANTHROPIC_BASE_URL") {
		t.Errorf("new claude file missing ANTHROPIC_BASE_URL: %s", b)
	}
}

// --- rewriteOpencode: writes provider entry with /v1 baseURL + models ---

func TestRewriteOpencode(t *testing.T) {
	dir := t.TempDir()
	cfg := testTakeoverConfig(t, dir)
	os.WriteFile(cfg.Takeover.Opencode, []byte(`{}`), 0o644)

	if err := rewriteOpencode(cfg, nil, nil); err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	b, _ := os.ReadFile(cfg.Takeover.Opencode)
	json.Unmarshal(b, &v)
	prov, _ := v["provider"].(map[string]any)
	p, _ := prov["model-proxy"].(map[string]any)
	if p == nil {
		t.Fatalf("opencode missing provider 'model-proxy': %s", b)
	}
	opts, _ := p["options"].(map[string]any)
	baseURL, _ := opts["baseURL"].(string)
	if !strings.HasSuffix(baseURL, "/v1") {
		t.Errorf("opencode baseURL=%q must end with /v1 (opencode appends /messages)", baseURL)
	}
	if opts["apiKey"] != "PROXY_MANAGED" {
		t.Errorf("opencode apiKey=%v want PROXY_MANAGED", opts["apiKey"])
	}
	models, _ := p["models"].(map[string]any)
	if _, ok := models["glm-5.2"]; !ok {
		t.Errorf("opencode models missing glm-5.2: %v", models)
	}
}

// --- rewritePi: writes provider with bare baseURL (no /v1) + anthropic-messages api ---

func TestRewritePi(t *testing.T) {
	dir := t.TempDir()
	cfg := testTakeoverConfig(t, dir)
	os.WriteFile(cfg.Takeover.Pi, []byte(`{}`), 0o644)

	if err := rewritePi(cfg, nil, nil); err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	b, _ := os.ReadFile(cfg.Takeover.Pi)
	json.Unmarshal(b, &v)
	prov, _ := v["providers"].(map[string]any)
	p, _ := prov["model-proxy"].(map[string]any)
	if p == nil {
		t.Fatalf("pi missing provider 'model-proxy': %s", b)
	}
	if p["api"] != "anthropic-messages" {
		t.Errorf("pi api=%v want anthropic-messages", p["api"])
	}
	baseURL, _ := p["baseUrl"].(string)
	if strings.HasSuffix(baseURL, "/v1") {
		t.Errorf("pi baseUrl=%q must NOT end with /v1 (pi appends /v1/messages itself)", baseURL)
	}
	models, _ := p["models"].([]any)
	if len(models) == 0 {
		t.Errorf("pi models empty; want at least one")
	}
}

// --- rewriteCodex: injects [model_providers."id"] + sets top-level model_provider ---

func TestRewriteCodex(t *testing.T) {
	dir := t.TempDir()
	cfg := testTakeoverConfig(t, dir)
	os.WriteFile(cfg.Takeover.Codex, []byte(`model_provider = "old"
[model_providers."old"]
name = "old"
`), 0o644)

	if err := rewriteCodex(cfg); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(cfg.Takeover.Codex)
	text := string(b)

	if !strings.Contains(text, `model_provider = "model-proxy"`) {
		t.Errorf("codex config missing top-level model_provider:\n%s", text)
	}
	if !strings.Contains(text, `[model_providers."model-proxy"]`) {
		t.Errorf("codex config missing [model_providers.\"model-proxy\"] section:\n%s", text)
	}
	// Note: rewriteCodex does NOT remove a pre-existing provider section under
	// a different id; it only injects/replaces its own. The top-level
	// model_provider key is what selects the active provider.
}

// --- setTOMLTopKey: replaces existing top-level key ---

func TestSetTOMLTopKey_Replace(t *testing.T) {
	in := `model_provider = "old"
[some]
x = 1
`
	out := setTOMLTopKey(in, "model_provider", `"new"`)
	if !strings.Contains(out, `model_provider = "new"`) {
		t.Errorf("setTOMLTopKey replace:\n%s", out)
	}
	if strings.Contains(out, `"old"`) {
		t.Errorf("setTOMLTopKey did not remove old value:\n%s", out)
	}
}

// --- setTOMLTopKey: inserts a new key before the first section ---

func TestSetTOMLTopKey_InsertBeforeSection(t *testing.T) {
	in := `[some]
x = 1
`
	out := setTOMLTopKey(in, "model_provider", `"mp"`)
	idxKey := strings.Index(out, `model_provider = "mp"`)
	idxSec := strings.Index(out, "[some]")
	if idxKey < 0 || idxSec < 0 {
		t.Fatalf("setTOMLTopKey insert missing key/section:\n%s", out)
	}
	if idxKey > idxSec {
		t.Errorf("setTOMLTopKey inserted key after section:\n%s", out)
	}
}

// --- setTOMLTopKey: appends when no section exists ---

func TestSetTOMLTopKey_Append(t *testing.T) {
	in := ``
	out := setTOMLTopKey(in, "model_provider", `"mp"`)
	if !strings.Contains(out, `model_provider = "mp"`) {
		t.Errorf("setTOMLTopKey append:\n%s", out)
	}
}

// --- replaceOrAppendTOMLSection: appends a new section ---

func TestReplaceOrAppendTOMLSection_Append(t *testing.T) {
	in := `existing = true
`
	section := `
[foo]
bar = "baz"
`
	out := replaceOrAppendTOMLSection(in, "foo", section)
	if !strings.Contains(out, `[foo]`) || !strings.Contains(out, `bar = "baz"`) {
		t.Errorf("replaceOrAppendTOMLSection append:\n%s", out)
	}
	if !strings.Contains(out, "existing = true") {
		t.Errorf("replaceOrAppendTOMLSection append dropped existing content:\n%s", out)
	}
}

// --- replaceOrAppendTOMLSection: replaces an existing section, stops at next ---

func TestReplaceOrAppendTOMLSection_Replace(t *testing.T) {
	in := `[foo]
old = "x"

[other]
keep = true
`
	section := `
[foo]
new = "y"
`
	out := replaceOrAppendTOMLSection(in, "foo", section)
	if !strings.Contains(out, `new = "y"`) {
		t.Errorf("replace did not add new key:\n%s", out)
	}
	if strings.Contains(out, `old = "x"`) {
		t.Errorf("replace did not drop old key:\n%s", out)
	}
	if !strings.Contains(out, `keep = true`) {
		t.Errorf("replace clobbered the NEXT [other] section:\n%s", out)
	}
}

// --- exposedModels: picks best-priority target's metadata ---

func TestExposedModels_PicksBestPriority(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{
			"a": {Provider: "static", Models: []string{"m1"}},
			"b": {Provider: "static", Models: []string{"m1"}},
		},
		Routes: map[string][]RouteTarget{
			"m1": {
				{Provider: "b", Model: "m1", Priority: 2},
				{Provider: "a", Model: "m1", Priority: 1}, // best
			},
		},
	}
	// Metadata is now runtime-sourced (models.dev); build the hydrated map directly.
	meta := map[string]map[string]ProviderModel{
		"a": {"m1": {Context: 1000, Output: 2000}},
		"b": {"m1": {Context: 3000, Output: 4000}},
	}
	got := exposedModels(cfg, meta, nil)
	if len(got) != 1 {
		t.Fatalf("exposedModels len=%d want 1", len(got))
	}
	if got[0].provider != "a" {
		t.Errorf("exposedModels provider=%q want a (priority 1)", got[0].provider)
	}
	if got[0].pm.Context != 1000 {
		t.Errorf("exposedModels context=%d want 1000 (from provider a)", got[0].pm.Context)
	}
}

// --- providerID: default + override ---

func TestProviderID(t *testing.T) {
	if got := providerID(&Config{}); got != "model-proxy" {
		t.Errorf("providerID(empty)=%q want model-proxy", got)
	}
	if got := providerID(&Config{Takeover: Takeover{ProviderID: "custom"}}); got != "custom" {
		t.Errorf("providerID(custom)=%q want custom", got)
	}
}
