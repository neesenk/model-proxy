package takeover_test

import (
	"encoding/json"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/takeover"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"model-proxy/internal/catalog"
)

// takeover_test.go covers the takeover client templates (presets rendered by
// the engine: json/toml/env formats + model shapes) and the TOML text helpers.
// These are pure file/string operations against the takeover target files —
// fully testable with temp dirs.

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

// --- kimi template: ~/.kimi/config.toml provider + model blocks ---

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
