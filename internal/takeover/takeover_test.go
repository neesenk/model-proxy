package takeover_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"model-proxy/internal/catalog"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/takeover"
)

// takeover_test.go covers the takeover client templates (presets rendered by
// the engine: json/toml/env formats + model shapes) and the TOML text helpers.
// These are pure file/string operations against the takeover target files —
// fully testable with temp dirs.

// TestMain isolates HOME for the WHOLE package: preset templates carry
// user-home-relative auxiliary paths (codex's ~/.codex/model-proxy-models.json
// catalog, claude's ~/.claude.json MCP storage) that a test driving a preset
// rewrites even when the main config file is redirected to a temp path.
// Without this, `go test` silently clobbers the developer's real client
// configs (pitfalls.md 26b family).
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "takeover-test-home-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "test home:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(home)
	os.Setenv("HOME", home)
	code := m.Run()
	os.Exit(code)
}

func baseCfg() *configdomain.Config {
	return &configdomain.Config{
		Listen: "127.0.0.1:15721",
		Providers: map[string]configdomain.Provider{
			"aqp": {
				OpenAIBaseURL: "http://x", Provider: "aqp",
				Models: []string{"glm-5.2"},
			},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"glm-5.2": {{Provider: "aqp", Model: "glm-5.2", Priority: 1}},
		},
	}
}

// presetFor loads a preset template and points its File at a temp path.
func presetFor(t *testing.T, name, file string) *takeover.Template {
	t.Helper()
	tpl, err := takeover.TemplateByName(name, "")
	if err != nil {
		t.Fatalf("preset %s: %v", name, err)
	}
	tpl.File = file
	return tpl
}

func mustReadTOML(t *testing.T, file string) []byte {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// --- claude template: sets ANTHROPIC_BASE_URL + ANTHROPIC_AUTH_TOKEN ---

func TestTemplateClaude(t *testing.T) {
	dir := t.TempDir()
	cfg := baseCfg()
	file := filepath.Join(dir, "claude.json")
	// Start with an existing settings.json (possibly with other env keys).
	os.WriteFile(file, []byte(`{"env":{"OTHER":"x"},"theme":"dark"}`), 0o644)

	if err := presetFor(t, "claude", file).Rewrite(cfg, nil, cfg.Routes); err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	b, _ := os.ReadFile(file)
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

// --- claude template on a missing file: creates a new one ---

func TestTemplateClaude_NewFile(t *testing.T) {
	dir := t.TempDir()
	cfg := baseCfg()
	file := filepath.Join(dir, "claude.json")
	if err := presetFor(t, "claude", file).Rewrite(cfg, nil, cfg.Routes); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("claude file not created: %v", err)
	}
	if !strings.Contains(string(b), "ANTHROPIC_BASE_URL") {
		t.Errorf("new claude file missing ANTHROPIC_BASE_URL: %s", b)
	}
}

// --- opencode template: writes provider entry with /v1 baseURL + models ---

func TestTemplateOpencode(t *testing.T) {
	dir := t.TempDir()
	cfg := baseCfg()
	file := filepath.Join(dir, "opencode.json")
	os.WriteFile(file, []byte(`{}`), 0o644)

	if err := presetFor(t, "opencode", file).Rewrite(cfg, nil, cfg.Routes); err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	b, _ := os.ReadFile(file)
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

// TestTemplateOpencode_ReasoningAndToolCallFlags: opencode's model schema
// carries reasoning/tool_call booleans; without them opencode treats
// reasoning models as plain chat models. Metadata drives the flags; models
// without metadata get neither (opencode defaults both false).
func TestTemplateOpencode_ReasoningAndToolCallFlags(t *testing.T) {
	dir := t.TempDir()
	cfg := baseCfg()
	file := filepath.Join(dir, "opencode.json")
	os.WriteFile(file, []byte(`{}`), 0o644)
	meta := map[string]map[string]catalog.Model{
		"aqp": {"glm-5.2": {Reasoning: true, ToolCall: true,
			Modalities: catalog.Modalities{Input: []string{"text", "image"}, Output: []string{"text"}}}},
	}
	if err := presetFor(t, "opencode", file).Rewrite(cfg, meta, cfg.Routes); err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	b, _ := os.ReadFile(file)
	json.Unmarshal(b, &v)
	p, _ := v["provider"].(map[string]any)["model-proxy"].(map[string]any)
	m, _ := p["models"].(map[string]any)["glm-5.2"].(map[string]any)
	if m["reasoning"] != true || m["tool_call"] != true {
		t.Errorf("reasoning/tool_call flags not written from metadata: %v", m)
	}
	mods, _ := m["modalities"].(map[string]any)
	in, _ := mods["input"].([]any)
	if len(in) != 2 || in[1] != "image" {
		t.Errorf("input modalities = %v, want [text image]", in)
	}
}

// --- pi template: writes provider with bare baseURL (no /v1) + anthropic-messages api ---

func TestTemplatePi(t *testing.T) {
	dir := t.TempDir()
	cfg := baseCfg()
	file := filepath.Join(dir, "pi.json")
	os.WriteFile(file, []byte(`{}`), 0o644)

	if err := presetFor(t, "pi", file).Rewrite(cfg, nil, nil); err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	b, _ := os.ReadFile(file)
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

// --- codex template: injects [model_providers."id"] + sets top-level model_provider ---

func TestTemplateCodex(t *testing.T) {
	dir := t.TempDir()
	cfg := baseCfg()
	file := filepath.Join(dir, "codex.toml")
	os.WriteFile(file, []byte(`model_provider = "old"
[model_providers."old"]
name = "old"
`), 0o644)

	if err := presetFor(t, "codex", file).Rewrite(cfg, nil, cfg.Routes); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(file)
	text := string(b)

	if !strings.Contains(text, `model_provider = "model-proxy"`) {
		t.Errorf("codex config missing top-level model_provider:\n%s", text)
	}
	if !strings.Contains(text, `[model_providers."model-proxy"]`) {
		t.Errorf("codex config missing [model_providers.\"model-proxy\"] section:\n%s", text)
	}
	// codex appends /responses to the provider base_url (wire_api=responses),
	// so it MUST carry /v1 — a bare proxy URL makes codex request /responses,
	// which the gateway does not route (502 no route for path /responses).
	if !strings.Contains(text, `base_url = "http://127.0.0.1:15721/v1"`) {
		t.Errorf("codex base_url must be the versioned endpoint (codex appends /responses):\n%s", text)
	}
	// Note: the codex template does NOT remove a pre-existing provider section
	// under a different id; it only injects/replaces its own. The top-level
	// model_provider key is what selects the active provider.
}

// --- setTOMLTopKey: replaces existing top-level key ---

func TestSetTOMLTopKey_Replace(t *testing.T) {
	in := `model_provider = "old"
[some]
x = 1
`
	out := takeover.SetTOMLTopKey(in, "model_provider", `"new"`)
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
	out := takeover.SetTOMLTopKey(in, "model_provider", `"mp"`)
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
	out := takeover.SetTOMLTopKey(in, "model_provider", `"mp"`)
	if !strings.Contains(out, `model_provider = "mp"`) {
		t.Errorf("setTOMLTopKey append:\n%s", out)
	}
}

// --- setTOMLTopKey: whitespace-tolerant replace (no spaces around `=`) ---

// Regression: matching only the spaced prefix `model_provider = ` missed a
// pre-existing `model_provider="old"` line, so a second key was inserted —
// a TOML duplicate-key parse error that bricks the codex config.
func TestSetTOMLTopKey_ReplaceWithoutSpaces(t *testing.T) {
	for _, in := range []string{
		"model_provider=\"old\"\n[some]\nx = 1\n",
		"model_provider =\"old\"\n",
		"model_provider= \"old\"\n",
		"  model_provider  =  \"old\"\n",
		"\"model_provider\" = \"old\"\n", // quoted key: same TOML key
	} {
		out := takeover.SetTOMLTopKey(in, "model_provider", `"new"`)
		if strings.Contains(out, `"old"`) {
			t.Errorf("setTOMLTopKey did not replace %q:\n%s", in, out)
		}
		if strings.Count(out, "model_provider") != 1 {
			t.Errorf("setTOMLTopKey duplicated the key for %q:\n%s", in, out)
		}
		if !strings.Contains(out, `model_provider = "new"`) {
			t.Errorf("setTOMLTopKey replace %q:\n%s", in, out)
		}
	}
}

// --- setTOMLTopKey: a quoted VALUE mentioning the key is not a false match ---

func TestSetTOMLTopKey_QuotedValueNotCorrupted(t *testing.T) {
	in := `note = "model_provider = keepme"
[some]
x = 1
`
	out := takeover.SetTOMLTopKey(in, "model_provider", `"mp"`)
	if !strings.Contains(out, `note = "model_provider = keepme"`) {
		t.Errorf("setTOMLTopKey corrupted an unrelated quoted value:\n%s", out)
	}
	if strings.Count(out, "model_provider = \"mp\"") != 1 {
		t.Errorf("setTOMLTopKey did not insert the key exactly once:\n%s", out)
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
	out := takeover.ReplaceOrAppendTOMLSection(in, "foo", section)
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
	out := takeover.ReplaceOrAppendTOMLSection(in, "foo", section)
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

// --- replaceOrAppendTOMLSection: a header inside a quoted value is not a match ---

// Regression: the old substring search matched the header text anywhere in
// the file, so a quoted value containing "[foo]" was treated as the section
// header and the file was corrupted. The header must match a whole line.
func TestReplaceOrAppendTOMLSection_HeaderInQuotedValue(t *testing.T) {
	section := `
[foo]
new = "y"
`

	// Append case: no real [foo] section, only a quoted mention — the value
	// must survive untouched and the section must be appended, not "replaced".
	in := `x = "[foo]"
`
	out := takeover.ReplaceOrAppendTOMLSection(in, "foo", section)
	if !strings.Contains(out, `x = "[foo]"`) {
		t.Errorf("quoted value corrupted:\n%s", out)
	}
	if strings.Count(out, "[foo]") != 2 { // the value + the appended header
		t.Errorf("section not appended exactly once:\n%s", out)
	}

	// Replace case: both a quoted mention and a real [foo] section — only the
	// real section is replaced.
	in = `x = "[foo]"

[foo]
old = "x"

[other]
keep = true
`
	out = takeover.ReplaceOrAppendTOMLSection(in, "foo", section)
	if !strings.Contains(out, `x = "[foo]"`) {
		t.Errorf("quoted value corrupted on replace:\n%s", out)
	}
	if !strings.Contains(out, `new = "y"`) || strings.Contains(out, `old = "x"`) {
		t.Errorf("real section not replaced:\n%s", out)
	}
	if !strings.Contains(out, `keep = true`) {
		t.Errorf("replace clobbered the NEXT [other] section:\n%s", out)
	}
}

// --- exposedModels: picks best-priority target's metadata ---

func TestExposedModels_PicksBestPriority(t *testing.T) {
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"a": {Provider: "static", Models: []string{"m1"}},
			"b": {Provider: "static", Models: []string{"m1"}},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"m1": {
				{Provider: "b", Model: "m1", Priority: 2},
				{Provider: "a", Model: "m1", Priority: 1}, // best
			},
		},
	}
	// Metadata is now runtime-sourced (models.dev); build the hydrated map directly.
	meta := map[string]map[string]catalog.Model{
		"a": {"m1": {Context: 1000, Output: 2000}},
		"b": {"m1": {Context: 3000, Output: 4000}},
	}
	got := takeover.ExposedModels(cfg, meta, cfg.Routes)
	if len(got) != 1 {
		t.Fatalf("exposedModels len=%d want 1", len(got))
	}
	if got[0].Provider != "a" {
		t.Errorf("exposedModels provider=%q want a (priority 1)", got[0].Provider)
	}
	if got[0].PM.Context != 1000 {
		t.Errorf("exposedModels context=%d want 1000 (from provider a)", got[0].PM.Context)
	}
}

// --- exposedModels: deterministic sorted order (routes is a map) ---

func TestExposedModels_SortedByExposedName(t *testing.T) {
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"a": {Provider: "static", Models: []string{"m1", "m2", "m3"}},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"zeta":  {{Provider: "a", Model: "m3"}},
			"alpha": {{Provider: "a", Model: "m1"}},
			"mid":   {{Provider: "a", Model: "m2"}},
		},
	}
	got := takeover.ExposedModels(cfg, nil, cfg.Routes)
	if len(got) != 3 {
		t.Fatalf("exposedModels len=%d want 3", len(got))
	}
	want := []string{"alpha", "mid", "zeta"}
	for i, name := range want {
		if got[i].Exposed != name {
			t.Fatalf("exposedModels[%d].Exposed=%q want sorted order %v", i, got[i].Exposed, want)
		}
	}
}

// --- provider id: template default + per-template override ---

func TestTemplateProviderID(t *testing.T) {
	tpl := presetFor(t, "pi", filepath.Join(t.TempDir(), "pi.json"))
	if got := tpl.ProviderIDValue(); got != "model-proxy" {
		t.Errorf("ProviderIDValue(default)=%q want model-proxy", got)
	}
	tpl.ProviderID = "custom"
	if got := tpl.ProviderIDValue(); got != "custom" {
		t.Errorf("ProviderIDValue(custom)=%q want custom", got)
	}
	// Variants writing into the same client file must carry distinct ids.
	for _, name := range []string{"pi", "pi-openai", "pi-responses"} {
		other := presetFor(t, name, filepath.Join(t.TempDir(), "x.json"))
		if name != "pi" && other.ProviderIDValue() == "model-proxy" {
			t.Errorf("%s must override provider_id to coexist with pi", name)
		}
	}
	for _, name := range []string{"opencode", "opencode-openai", "opencode-responses"} {
		other := presetFor(t, name, filepath.Join(t.TempDir(), "x.json"))
		if name != "opencode" && other.ProviderIDValue() == "model-proxy" {
			t.Errorf("%s must override provider_id to coexist with opencode", name)
		}
	}
}

// TestPresetOpencodeVariantNpm pins the opencode package-to-protocol mapping:
// opencode-openai must use @ai-sdk/openai-compatible (Chat Completions), and
// opencode-responses must use @ai-sdk/openai (Responses API).
func TestPresetOpencodeVariantNpm(t *testing.T) {
	wantNpm := map[string]string{
		"opencode":           "@ai-sdk/anthropic",
		"opencode-openai":    "@ai-sdk/openai-compatible",
		"opencode-responses": "@ai-sdk/openai",
	}
	for name, want := range wantNpm {
		tpl := presetFor(t, name, filepath.Join(t.TempDir(), "x.json"))
		set, ok := tpl.JSON.Set["provider.{{provider_id}}"].(map[string]any)
		if !ok {
			t.Fatalf("%s: expected provider set block", name)
		}
		got, _ := set["npm"].(string)
		if got != want {
			t.Errorf("%s npm=%q, want %q", name, got, want)
		}
	}
}

// --- kimi template: ~/.kimi-code/config.toml provider + model blocks ---

func TestTemplateKimi(t *testing.T) {
	dir := t.TempDir()
	cfg := baseCfg()
	file := filepath.Join(dir, "kimi.toml")
	os.WriteFile(file, []byte("[providers.\"existing\"]\ntype = \"kimi\"\n"), 0o644)

	kimi := presetFor(t, "kimi", file)
	if err := kimi.Rewrite(cfg, nil, cfg.Routes); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(file)
	text := string(b)

	if !strings.Contains(text, `[providers."model-proxy"]`) {
		t.Errorf("kimi config missing [providers.\"model-proxy\"] section:\n%s", text)
	}
	if !strings.Contains(text, `type = "openai_legacy"`) {
		t.Errorf("kimi provider must declare openai_legacy (Chat Completions):\n%s", text)
	}
	if !strings.Contains(text, `base_url = "http://127.0.0.1:15721/v1"`) {
		t.Errorf("kimi provider base_url must be the versioned proxy endpoint:\n%s", text)
	}
	if !strings.Contains(text, `api_key = "PROXY_MANAGED"`) {
		t.Errorf("kimi provider must carry the sentinel key:\n%s", text)
	}
	// kimi-cli's LLMModel schema requires provider + model + max_context_size,
	// and the dotted exposed name must be quoted ([models.glm-5.2] would parse
	// as nested tables models → glm-5 → "2").
	if !strings.Contains(text, `[models."glm-5.2"]`) {
		t.Errorf("exposed model missing its quoted [models.\"<name>\"] block:\n%s", text)
	}
	if strings.Contains(text, "[models.glm-5.2]") {
		t.Errorf("dotted model name written unquoted (parses as nested tables):\n%s", text)
	}
	if !strings.Contains(text, `provider = "model-proxy"`) || !strings.Contains(text, `model = "glm-5.2"`) {
		t.Errorf("model block must carry provider + model (wire id):\n%s", text)
	}
	// No catalog metadata was supplied → the required max_context_size falls
	// back to the conservative default (omitting it fails kimi-cli validation).
	if !strings.Contains(text, "max_context_size = 200000") {
		t.Errorf("model block missing required max_context_size fallback:\n%s", text)
	}
	// Pre-existing foreign provider sections are preserved (only our own block is replaced).
	if !strings.Contains(text, `[providers."existing"]`) {
		t.Errorf("foreign provider section dropped:\n%s", text)
	}

	// Idempotent re-run: same content, no duplicated blocks.
	if err := kimi.Rewrite(cfg, nil, cfg.Routes); err != nil {
		t.Fatal(err)
	}
	b2, _ := os.ReadFile(file)
	if strings.Count(string(b2), `[providers."model-proxy"]`) != 1 {
		t.Errorf("re-run duplicated the provider block:\n%s", b2)
	}
	if strings.Count(string(b2), `[models."glm-5.2"]`) != 1 {
		t.Errorf("re-run duplicated the model block:\n%s", b2)
	}
}

// TestTemplateKimi_UsesCatalogContext: hydrated models.dev metadata wins over
// the fallback for the required max_context_size.
func TestTemplateKimi_UsesCatalogContext(t *testing.T) {
	dir := t.TempDir()
	cfg := baseCfg()
	file := filepath.Join(dir, "kimi.toml")
	os.WriteFile(file, []byte(""), 0o644)
	meta := map[string]map[string]catalog.Model{
		"aqp": {"glm-5.2": {Context: 131072, Output: 8192}},
	}

	if err := presetFor(t, "kimi", file).Rewrite(cfg, meta, cfg.Routes); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(file)
	text := string(b)
	if !strings.Contains(text, "max_context_size = 131072") {
		t.Errorf("max_context_size must come from catalog metadata:\n%s", text)
	}
	if strings.Contains(text, "max_context_size = 200000") {
		t.Errorf("fallback context written despite catalog metadata:\n%s", text)
	}
}

// TestTemplateKimi_DropsLegacyUnquotedBlock: the old writer emitted
// [models.glm-5.2] (nested tables, fails kimi-cli validation). A re-run must
// remove that leftover block, not just append the corrected quoted one.
func TestTemplateKimi_DropsLegacyUnquotedBlock(t *testing.T) {
	dir := t.TempDir()
	cfg := baseCfg()
	file := filepath.Join(dir, "kimi.toml")
	os.WriteFile(file, []byte(`[providers."model-proxy"]
type = "openai_legacy"
base_url = "http://127.0.0.1:15721/v1"
api_key = "PROXY_MANAGED"

[models.glm-5.2]
provider = "model-proxy"
`), 0o644)

	if err := presetFor(t, "kimi", file).Rewrite(cfg, nil, cfg.Routes); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(file)
	text := string(b)
	if strings.Contains(text, "[models.glm-5.2]") {
		t.Errorf("legacy unquoted model block survived rewrite:\n%s", text)
	}
	if strings.Count(text, `[models."glm-5.2"]`) != 1 {
		t.Errorf("quoted model block missing or duplicated:\n%s", text)
	}
}

// TestTemplateKimi_ReplacesStaleOwnBlock pins replace-in-place: a previous
// takeover under the same id with a stale base_url must not survive.
func TestTemplateKimi_ReplacesStaleOwnBlock(t *testing.T) {
	dir := t.TempDir()
	cfg := baseCfg()
	file := filepath.Join(dir, "kimi.toml")
	os.WriteFile(file, []byte(`
[providers."model-proxy"]
type = "openai_legacy"
base_url = "http://127.0.0.1:99999/v1"
api_key = "OLD"
`), 0o644)

	if err := presetFor(t, "kimi", file).Rewrite(cfg, nil, cfg.Routes); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(file)
	if strings.Contains(string(b), "99999") || strings.Contains(string(b), "OLD") {
		t.Errorf("stale own block survived rewrite:\n%s", b)
	}
	if strings.Count(string(b), `[providers."model-proxy"]`) != 1 {
		t.Errorf("stale block not replaced in place:\n%s", b)
	}
}

// TestTemplateKimi_CapabilitiesFromMetadata: models.dev metadata drives
// kimi-code's per-model capability block. Without it kimi-cli treats every
// proxied model as text-only and never enables thinking — the regression
// this pins. Mapping verified against kimi's own managed config entries:
// k3-like (reasoning + image/video + tool_call + effort dial) gets the full
// set incl. dynamically_loaded_tools; highspeed-like (no effort dial) gets
// neither dynamically_loaded_tools nor the effort lines; models without
// metadata render an empty array, never a fabricated capability.
func TestTemplateKimi_CapabilitiesFromMetadata(t *testing.T) {
	dir := t.TempDir()
	cfg := baseCfg()
	file := filepath.Join(dir, "kimi.toml")
	os.WriteFile(file, []byte(""), 0o644)
	k3 := catalog.Model{
		Reasoning: true, ToolCall: true,
		Modalities:       catalog.Modalities{Input: []string{"text", "image", "video"}, Output: []string{"text"}},
		ReasoningEfforts: []string{"low", "high", "max"},
	}
	meta := map[string]map[string]catalog.Model{"aqp": {"glm-5.2": k3}}
	if err := presetFor(t, "kimi", file).Rewrite(cfg, meta, cfg.Routes); err != nil {
		t.Fatal(err)
	}
	text := string(mustReadTOML(t, file))
	if !strings.Contains(text, `capabilities = ["thinking", "always_thinking", "image_in", "video_in", "tool_use", "dynamically_loaded_tools"]`) {
		t.Errorf("k3-like capabilities wrong:\n%s", text)
	}
	if !strings.Contains(text, `support_efforts = ["low", "high", "max"]`) ||
		!strings.Contains(text, `default_effort = "max"`) {
		t.Errorf("k3-like effort block wrong:\n%s", text)
	}

	// highspeed-like: same minus the effort dial → no dynamically_loaded_tools,
	// and NO effort lines at all (an empty support_efforts would break the
	// effort selector; the placeholder renders to nothing).
	os.WriteFile(file, []byte(""), 0o644)
	hs := k3
	hs.ReasoningEfforts = nil
	meta = map[string]map[string]catalog.Model{"aqp": {"glm-5.2": hs}}
	if err := presetFor(t, "kimi", file).Rewrite(cfg, meta, cfg.Routes); err != nil {
		t.Fatal(err)
	}
	text = string(mustReadTOML(t, file))
	if !strings.Contains(text, `capabilities = ["thinking", "always_thinking", "image_in", "video_in", "tool_use"]`) {
		t.Errorf("highspeed-like capabilities wrong:\n%s", text)
	}
	if strings.Contains(text, "support_efforts") || strings.Contains(text, "default_effort") {
		t.Errorf("effort-less model must not get effort lines:\n%s", text)
	}

	// No metadata at all → empty capabilities array, no effort lines.
	os.WriteFile(file, []byte(""), 0o644)
	if err := presetFor(t, "kimi", file).Rewrite(cfg, nil, cfg.Routes); err != nil {
		t.Fatal(err)
	}
	text = string(mustReadTOML(t, file))
	if !strings.Contains(text, "capabilities = []") {
		t.Errorf("metadata-less model must render the explicit empty array:\n%s", text)
	}
	if strings.Contains(text, "default_effort") {
		t.Errorf("metadata-less model must not get effort lines:\n%s", text)
	}
}

// TestTemplateCodex_ModelCatalog: the codex shape writes a standalone
// model-catalog JSON (one entry per exposed model, visibility "list" so the
// /model picker shows it, reasoning levels from the effort dial) and points
// model_catalog_json at it — codex then offers every proxy model, replacing
// its bundled list.
func TestTemplateCodex_ModelCatalog(t *testing.T) {
	// HOME is isolated: the run/restore half of this test goes through the
	// REAL preset paths (~ expansion must land inside the sandbox, never in
	// the developer's actual ~/.codex).
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	cfg := baseCfg()
	file := filepath.Join(dir, ".codex", "config.toml")
	catalogFile := filepath.Join(dir, ".codex", "model-proxy-models.json")
	if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	tpl, err := takeover.TemplateByName("codex", "")
	if err != nil {
		t.Fatal(err)
	}
	tpl.File = file
	tpl.Models.CatalogFile = catalogFile
	meta := map[string]map[string]catalog.Model{
		"aqp": {"glm-5.2": {Context: 131072, Reasoning: true, ToolCall: true,
			Modalities:       catalog.Modalities{Input: []string{"text", "image"}, Output: []string{"text"}},
			ReasoningEfforts: []string{"low", "high", "max"}}},
	}
	if err := tpl.Rewrite(cfg, meta, cfg.Routes); err != nil {
		t.Fatal(err)
	}

	// The client config points at the catalog (versioned proxy URL pinned by
	// TestTemplateCodex; here the catalog wiring matters).
	text := string(mustReadTOML(t, file))
	if !strings.Contains(text, `model_catalog_json = "`+catalogFile+`"`) {
		t.Errorf("config missing model_catalog_json:\n%s", text)
	}
	var doc struct {
		Models []struct {
			Slug         string `json:"slug"`
			Visibility   string `json:"visibility"`
			Context      int64  `json:"context_window"`
			DefaultLevel string `json:"default_reasoning_level"`
			Levels       []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
			ShellType string   `json:"shell_type"`
			Input     []string `json:"input_modalities"`
			Messages  *struct {
				Template string `json:"instructions_template"`
			} `json:"model_messages"`
		} `json:"models"`
	}
	if err := json.Unmarshal(mustReadTOML(t, catalogFile), &doc); err != nil {
		t.Fatalf("catalog not valid JSON: %v", err)
	}
	if len(doc.Models) != 1 || doc.Models[0].Slug != "glm-5.2" {
		t.Fatalf("catalog models = %+v, want one glm-5.2", doc.Models)
	}
	m := doc.Models[0]
	if m.Visibility != "list" || m.Context != 131072 {
		t.Errorf("catalog entry missing visibility/context: %+v", m)
	}
	// Regression: codex's ModelInfo has 16 serde-REQUIRED fields and rejects
	// entries missing instructions ("missing both base_instructions and
	// model_messages.instructions_template") — every entry must carry them.
	if m.Messages == nil || m.Messages.Template == "" {
		t.Errorf("catalog entry missing model_messages.instructions_template")
	}
	if m.ShellType != "unified_exec" {
		t.Errorf("catalog entry missing shell_type: %q", m.ShellType)
	}
	if m.DefaultLevel != "max" || len(m.Levels) != 3 || m.Levels[0].Effort != "low" {
		t.Errorf("reasoning levels wrong: %+v", m)
	}
	if len(m.Input) != 2 || m.Input[1] != "image" {
		t.Errorf("input modalities = %v, want [text image]", m.Input)
	}

	// Restore removes the catalog together with the takeover (real preset
	// paths under the isolated HOME — proving the end-to-end flow). Reset the
	// config to a clean pre-takeover state first: the backup the run takes
	// must not carry this test's earlier direct-rewrite leftovers.
	if err := os.WriteFile(file, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Remove(catalogFile)
	bakDir := filepath.Join(dir, ".mp")
	facts := takeover.ModelFactsFor(cfg, "codex", dir, dir, takeover.ModeUnified)
	if err := takeover.RunTakeover(cfg, "codex", bakDir, facts, dir, takeover.ModeUnified); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".codex", "model-proxy-models.json")); err != nil {
		t.Fatalf("run via preset did not write the catalog: %v", err)
	}
	if err := takeover.RunRestore(cfg, "codex", bakDir, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".codex", "model-proxy-models.json")); !os.IsNotExist(err) {
		t.Errorf("restore must delete the model catalog: stat err=%v", err)
	}
	if strings.Contains(string(mustReadTOML(t, filepath.Join(dir, ".codex", "config.toml"))), "model_catalog_json") {
		t.Errorf("restore must remove model_catalog_json from the config")
	}
}

// TestTemplateClaude_ModelAndMCPCombined: one claude takeover covers BOTH
// files — the model takeover (settings.json env injection) and the MCP
// surface (~/.claude.json mcpServers, the merged former claude-mcp preset) —
// each with its own backup unit, and restore returns both originals.
func TestTemplateClaude_ModelAndMCPCombined(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(home, ".claude", "settings.json")
	claudeJSON := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(settings, []byte(`{"env":{"KEEP":"1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudeJSON, []byte(`{"theme":"dark","mcpServers":{"mine":{"url":"https://other.example/mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:15721",
		MCP:    map[string]configdomain.MCPServer{"exa": {URL: "https://mcp.exa.ai/mcp"}},
	}
	bakDir := filepath.Join(home, ".mp")
	if err := takeover.RunTakeover(cfg, "claude", bakDir, takeover.ModelFacts{SourceDefault: -1}, home, takeover.ModeUnified); err != nil {
		t.Fatal(err)
	}
	// Model takeover landed in settings.json (unrelated keys kept).
	env := mustJSONField(t, settings, "env")
	if env["KEEP"] != "1" || env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:15721" {
		t.Errorf("settings.json env wrong: %v", env)
	}
	// MCP surface landed in ~/.claude.json (own file, user entry kept).
	doc := mustJSONField(t, claudeJSON, "mcpServers")
	if _, ok := doc["mine"]; !ok {
		t.Errorf("user's own mcp server dropped: %v", doc)
	}
	exa, _ := doc["exa"].(map[string]any)
	if exa == nil || exa["url"] != "http://127.0.0.1:15721/mcp/exa" {
		t.Errorf("gateway mcp entry missing/wrong: %v", doc)
	}
	// Both backup units exist.
	for _, marker := range []string{"claude.bak", "claude-mcp.bak"} {
		if _, err := os.Stat(filepath.Join(bakDir, marker)); err != nil {
			t.Errorf("backup marker %s missing: %v", marker, err)
		}
	}
	// Restore returns both originals and removes both markers.
	if err := takeover.RunRestore(cfg, "claude", bakDir, home); err != nil {
		t.Fatal(err)
	}
	if got := string(mustReadTOML(t, settings)); got != `{"env":{"KEEP":"1"}}` {
		t.Errorf("settings.json not restored verbatim: %s", got)
	}
	if !strings.Contains(string(mustReadTOML(t, claudeJSON)), `"mine"`) ||
		strings.Contains(string(mustReadTOML(t, claudeJSON)), "127.0.0.1:15721") {
		t.Errorf("~/.claude.json not restored to its original content")
	}
	for _, marker := range []string{"claude.bak", "claude-mcp.bak", "claude-mcp.bak.meta"} {
		if _, err := os.Stat(filepath.Join(bakDir, marker)); !os.IsNotExist(err) {
			t.Errorf("marker %s must be gone after restore", marker)
		}
	}
}

// mustJSONField parses a JSON file and returns one object field as a map.
func mustJSONField(t *testing.T, file, field string) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(mustReadTOML(t, file), &v); err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	m, _ := v[field].(map[string]any)
	if m == nil {
		t.Fatalf("%s missing object field %q: %v", file, field, v)
	}
	return m
}

// TestTemplateKimi_ModelAndMCPCombined: kimi's MCP surface lives in its own
// file (~/.kimi-code/mcp.json, mcpServers with url entries) next to the
// model takeover's config.toml — one takeover covers both, each with its own
// backup unit, and restore returns both.
func TestTemplateKimi_ModelAndMCPCombined(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".kimi-code"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgTOML := filepath.Join(home, ".kimi-code", "config.toml")
	mcpJSON := filepath.Join(home, ".kimi-code", "mcp.json")
	if err := os.WriteFile(cfgTOML, []byte("default_model = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mcpJSON, []byte(`{"mcpServers":{"mine":{"url":"https://other/mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:15721",
		Providers: map[string]configdomain.Provider{
			"aqp": {OpenAIBaseURL: "http://x", Provider: "aqp", Models: []string{"glm-5.2"}},
		},
		MCP: map[string]configdomain.MCPServer{"exa": {URL: "https://mcp.exa.ai/mcp"}},
	}
	bakDir := filepath.Join(home, ".mp")
	if err := takeover.RunTakeover(cfg, "kimi", bakDir, takeover.ModelFacts{SourceDefault: -1}, home, takeover.ModeUnified); err != nil {
		t.Fatal(err)
	}
	// Model takeover in config.toml (unrelated top key kept).
	if !strings.Contains(string(mustReadTOML(t, cfgTOML)), `providers."model-proxy"`) ||
		!strings.Contains(string(mustReadTOML(t, cfgTOML)), "default_model") {
		t.Errorf("config.toml takeover wrong:\n%s", mustReadTOML(t, cfgTOML))
	}
	// MCP surface in mcp.json (own file, user entry kept).
	servers := mustJSONField(t, mcpJSON, "mcpServers")
	if _, ok := servers["mine"]; !ok {
		t.Errorf("user's own mcp server dropped: %v", servers)
	}
	exa, _ := servers["exa"].(map[string]any)
	if exa == nil || exa["url"] != "http://127.0.0.1:15721/mcp/exa" {
		t.Errorf("gateway entry missing/wrong: %v", servers)
	}
	// Both backup units.
	for _, marker := range []string{"kimi.bak", "kimi-mcp.bak"} {
		if _, err := os.Stat(filepath.Join(bakDir, marker)); err != nil {
			t.Errorf("backup %s missing: %v", marker, err)
		}
	}
	// Restore returns both originals.
	if err := takeover.RunRestore(cfg, "kimi", bakDir, home); err != nil {
		t.Fatal(err)
	}
	if got := string(mustReadTOML(t, mcpJSON)); !strings.Contains(got, "mine") || strings.Contains(got, "127.0.0.1:15721") {
		t.Errorf("mcp.json not restored verbatim: %s", got)
	}
	if _, err := os.Stat(filepath.Join(bakDir, "kimi-mcp.bak")); !os.IsNotExist(err) {
		t.Errorf("kimi-mcp.bak must be gone after restore")
	}
}

// TestTemplateClaudeDefaultModelEnvs: claude's managed surface covers the
// default-model slots too — ANTHROPIC_MODEL/SONNET/OPUS ride the
// largest-context exposed model (flagship tier), HAIKU the smallest
// (background tier) — so a fresh takeover needs no manual model plumbing.
func TestTemplateClaudeDefaultModelEnvs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := os.WriteFile(settings, []byte(`{"env":{"FOO":"keep"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:15721",
		Providers: map[string]configdomain.Provider{
			"aqp": {Provider: "aqp", OpenAIBaseURL: "http://x", Models: []string{"small", "big"}},
		},
	}
	bakDir := filepath.Join(home, ".mp")
	facts := takeover.ModelFacts{
		Routes: map[string][]configdomain.RouteTarget{
			"small": {{Provider: "aqp", Model: "small"}},
			"big":   {{Provider: "aqp", Model: "big"}},
		},
		Meta: map[string]map[string]catalog.Model{
			"aqp": {
				"small": {Context: 128000},
				"big":   {Context: 1000000},
			},
		},
		SourceDefault: -1,
	}
	if err := takeover.RunTakeover(cfg, "claude", bakDir, facts, home, takeover.ModeUnified); err != nil {
		t.Fatal(err)
	}
	env := mustJSONField(t, settings, "env")
	for _, k := range []string{"ANTHROPIC_MODEL", "ANTHROPIC_DEFAULT_SONNET_MODEL", "ANTHROPIC_DEFAULT_OPUS_MODEL"} {
		if env[k] != "big" {
			t.Errorf("%s = %v, want big (largest context)", k, env[k])
		}
	}
	if env["ANTHROPIC_DEFAULT_HAIKU_MODEL"] != "small" {
		t.Errorf("HAIKU = %v, want small (smallest context)", env["ANTHROPIC_DEFAULT_HAIKU_MODEL"])
	}
	if env["FOO"] != "keep" {
		t.Errorf("unrelated env dropped: %v", env)
	}
}
