package takeover_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"model-proxy/internal/takeover"
)

// template_engine_test.go covers engine mechanics not tied to a preset's
// golden output: env format rewriting, user-template override precedence,
// and template validation errors.

// TestEnvRewrite: managed keys are replaced in place, unmanaged lines and
// comments survive, missing keys append in sorted order, and a missing file
// is created (0600).
func TestEnvRewrite(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, ".env")
	os.WriteFile(file, []byte("# comment\nOTHER=keep\nGOOGLE_GEMINI_BASE_URL=http://stale\n"), 0o600)

	tpl := presetFor(t, "gemini-cli", file)
	if err := tpl.Rewrite(baseCfg(), nil, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(file)
	text := string(b)
	for _, want := range []string{
		"# comment",
		"OTHER=keep",
		"GOOGLE_GEMINI_BASE_URL=http://127.0.0.1:15721/v1",
		"GEMINI_API_KEY=PROXY_MANAGED",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("env rewrite missing %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "stale") {
		t.Errorf("stale value survived:\n%s", text)
	}
	// Idempotent re-run.
	if err := tpl.Rewrite(baseCfg(), nil, nil); err != nil {
		t.Fatal(err)
	}
	b2, _ := os.ReadFile(file)
	if strings.Count(string(b2), "GEMINI_API_KEY=") != 1 {
		t.Errorf("re-run duplicated managed key:\n%s", b2)
	}

	// Missing file → created.
	missing := filepath.Join(dir, "new.env")
	if err := presetFor(t, "gemini-cli", missing).Rewrite(baseCfg(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(missing); err != nil {
		t.Errorf("env template must create a missing file: %v", err)
	}
}

// TestUserTemplateOverridesPreset: a same-named user template replaces the
// preset (file + body), and a new user template joins the resolved set.
func TestUserTemplateOverridesPreset(t *testing.T) {
	dir := t.TempDir()
	custom := filepath.Join(dir, "my-claude.json")
	os.WriteFile(filepath.Join(dir, "claude.yaml"), []byte("file: "+custom+`
format: json
json:
  set:
    env.ANTHROPIC_BASE_URL: "{{base_url}}"
    env.ANTHROPIC_AUTH_TOKEN: "{{token}}"
  drift_path: env.ANTHROPIC_BASE_URL
`), 0o600)
	os.WriteFile(filepath.Join(dir, "my-agent.yaml"), []byte("file: "+filepath.Join(dir, "agent.json")+`
format: json
json:
  set:
    url: "{{base_url}}"
`), 0o600)

	tpl, err := takeover.TemplateByName("claude", dir)
	if err != nil {
		t.Fatal(err)
	}
	if tpl.File != custom {
		t.Errorf("override file=%q want %q", tpl.File, custom)
	}
	if tpl.Source != dir {
		t.Errorf("override source=%q want %q", tpl.Source, dir)
	}
	if _, err := takeover.TemplateByName("my-agent", dir); err != nil {
		t.Errorf("user template must join the set: %v", err)
	}
	// Presets unaffected by the override stay available.
	if _, err := takeover.TemplateByName("codex", dir); err != nil {
		t.Errorf("unrelated preset must survive: %v", err)
	}
}

// TestTemplateValidation: malformed templates fail loudly at parse time.
func TestTemplateValidation(t *testing.T) {
	cases := []struct {
		name, doc, wantSub string
	}{
		{"no-file", "format: json\njson:\n  set: {a: b}\n", "file is required"},
		{"bad-format", "file: /tmp/x\nformat: xml\n", "json|toml|env"},
		{"json-no-set", "file: /tmp/x\nformat: json\n", "json.set"},
		{"bad-shape", "file: /tmp/x\nformat: json\njson:\n  set: {a: b}\nmodels:\n  shape: wat\n", "models.shape"},
		{"shape-no-path", "file: /tmp/x\nformat: json\njson:\n  set: {a: b}\nmodels:\n  shape: opencode\n", "json_path"},
		{"kimi-not-toml", "file: /tmp/x\nformat: json\njson:\n  set: {a: b}\nmodels:\n  shape: kimi\n", "format toml"},
		{"bad-base-url", "file: /tmp/x\nformat: env\nbase_url: v2\nenv:\n  set: {A: B}\n", "bare|v1"},
	}
	for _, tc := range cases {
		if _, err := takeover.ParseTemplate("t", "test", []byte(tc.doc)); err == nil ||
			!strings.Contains(err.Error(), tc.wantSub) {
			t.Errorf("%s: want error containing %q, got %v", tc.name, tc.wantSub, err)
		}
	}
}

// TestParseVariantsTemplate: a variants: document expands into one template
// per variant — each carrying the shared top-level fields (file/format/
// client/mcp) plus its own protocol identity and write blocks. Downstream
// (family selection, backup units, the Web surface) works on the expanded
// templates exactly like the former one-variant-per-file presets.
func TestParseVariantsTemplate(t *testing.T) {
	doc := `
description: pi family
file: ~/.pi/agent/models.json
format: json
client: pi
variants:
  - name: pi
    protocol: anthropic
    base_url: bare
    json:
      set:
        providers.{{provider_id}}: {baseUrl: "{{base_url}}", api: "anthropic-messages"}
      drift_path: providers.{{provider_id}}.baseUrl
    models:
      shape: pi
      json_path: providers.{{provider_id}}.models
  - name: pi-openai
    protocol: openai
    base_url: v1
    provider_id: model-proxy-openai
    json:
      set:
        providers.{{provider_id}}: {baseUrl: "{{base_url}}", api: "openai-completions"}
    models:
      shape: pi
      json_path: providers.{{provider_id}}.models
`
	parsed, err := takeover.ParseTemplate("pi", "preset", []byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed) != 2 {
		t.Fatalf("expanded = %d templates, want 2", len(parsed))
	}
	byName := map[string]*takeover.Template{}
	for _, t := range parsed {
		byName[t.Name] = t
	}
	pi := byName["pi"]
	openai := byName["pi-openai"]
	if pi == nil || openai == nil {
		t.Fatalf("expanded names missing: %v", parsed)
	}
	if pi.File != openai.File || pi.Client != "pi" || pi.Format != "json" {
		t.Errorf("shared fields not inherited: %+v vs %+v", pi, openai)
	}
	if pi.BaseURL != "bare" || openai.BaseURL != "v1" || openai.ProviderID != "model-proxy-openai" {
		t.Errorf("per-variant overrides lost: pi=%+v openai=%+v", pi, openai)
	}
	if pi.Protocol != "anthropic" || openai.Protocol != "openai" {
		t.Errorf("protocols wrong: %q / %q", pi.Protocol, openai.Protocol)
	}
	if pi.JSON == nil || openai.JSON == nil {
		t.Fatalf("per-variant json blocks lost")
	}
	if pi.Models == nil || pi.Models.JSONPath == "" {
		t.Errorf("per-variant models block lost")
	}

	// A single variants entry is a footgun (a plain template says it better).
	if _, err := takeover.ParseTemplate("one", "preset", []byte("file: ~/x.json\nformat: json\nclient: c\nvariants:\n  - name: a\n    protocol: openai\n    json:\n      set: {a: b}\n")); err == nil ||
		!strings.Contains(err.Error(), "two or more") {
		t.Errorf("single variant: want error, got %v", err)
	}
	// Duplicate variant names are ambiguous — reject.
	if _, err := takeover.ParseTemplate("dup", "preset", []byte("file: ~/x.json\nformat: json\nclient: c\nvariants:\n  - name: a\n    protocol: openai\n    json:\n      set: {a: b}\n  - name: a\n    protocol: anthropic\n    json:\n      set: {a: b}\n")); err == nil ||
		!strings.Contains(err.Error(), "declared twice") {
		t.Errorf("duplicate variant: want error, got %v", err)
	}
	// Write blocks at the top level are the plain-template shape — rejected
	// alongside variants to keep documents unambiguous.
	if _, err := takeover.ParseTemplate("mix", "preset", []byte("file: ~/x.json\nformat: json\nclient: c\njson:\n  set: {a: b}\nvariants:\n  - name: a\n    protocol: openai\n    json:\n      set: {a: b}\n")); err == nil ||
		!strings.Contains(err.Error(), "under each variant") {
		t.Errorf("top-level write block with variants: want error, got %v", err)
	}
}
