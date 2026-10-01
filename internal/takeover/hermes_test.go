package takeover_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/takeover"
)

// hermesFixture: one exposed reasoning model (primary by context size).
func hermesFixture() (*configdomain.Config, map[string][]configdomain.RouteTarget) {
	cfg := cfgWith(map[string]configdomain.Provider{
		"zhipu": {OpenAIBaseURL: "https://z/v1", Models: []string{"glm-5.3"}},
	}, nil)
	cfg.MCP = map[string]configdomain.MCPServer{
		"exa": {URL: "https://mcp.exa.ai/mcp", Auth: "none"},
	}
	routes := map[string][]configdomain.RouteTarget{
		"glm-5.3": {{Provider: "zhipu", Model: "glm-5.3", Priority: 1}},
	}
	return cfg, routes
}

const hermesSeed = `# Hermes configuration — hand-maintained, comments matter.
model:
  default: "anthropic/claude-opus-4.6"   # main model
  provider: "auto"
  base_url: "https://openrouter.ai/api/v1"

# Auxiliary tasks ride the main model.
display:
  streaming: true

mcp_servers:
  keep-me:
    url: https://other.example/mcp
  exa:
    url: http://127.0.0.1:9999/mcp/exa   # stale proxy entry from an old run
`

// TestHermesPresetRendering: the hermes preset writes a named provider entry
// (api_mode from the variant protocol), the model selector, and the gateway
// MCP surface — preserving comments, unrelated settings and user MCP servers.
func TestHermesPresetRendering(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	target := filepath.Join(home, ".hermes", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(hermesSeed), 0o600); err != nil {
		t.Fatal(err)
	}
	tmpl, err := takeover.TemplateByName("hermes-openai", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg, routes := hermesFixture()
	if err := tmpl.Rewrite(cfg, nil, routes); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)

	// Comments and unrelated content survive the round-trip.
	for _, want := range []string{
		"# Hermes configuration — hand-maintained, comments matter.",
		"# Auxiliary tasks ride the main model.",
		"streaming: true",
		"keep-me:",
		"url: https://other.example/mcp",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("unrelated content dropped %q:\n%s", want, s)
		}
	}
	// Selector + named provider entry.
	for _, want := range []string{
		"default: glm-5.3",
		"provider: model-proxy-openai",
		"model-proxy-openai:",
		"base_url: http://127.0.0.1:15721/v1",
		"api_key: PROXY_MANAGED",
		"api_mode: chat_completions",
		"session_affinity_header: x-session-id",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q:\n%s", want, s)
		}
	}
	// Gateway MCP surface merged; stale proxy entry rewritten.
	if !strings.Contains(s, "url: http://127.0.0.1:15721/mcp/exa") {
		t.Errorf("gateway mcp entry missing:\n%s", s)
	}
	if strings.Contains(s, "127.0.0.1:9999") {
		t.Errorf("stale proxy URL not cleaned:\n%s", s)
	}

	// Idempotent: re-render produces identical bytes.
	if err := tmpl.Rewrite(cfg, nil, routes); err != nil {
		t.Fatal(err)
	}
	b2, _ := os.ReadFile(target)
	if string(b2) != s {
		t.Fatalf("re-render changed the file:\n%s\n---\n%s", s, b2)
	}
}

// TestHermesVariantsShareFile: two protocol variants (split mode) write
// distinct named provider entries into the same config.yaml; each entry keeps
// its own api_mode and base_url, and the selector points at the last-applied
// variant (hermes selects ONE provider — unified is the sane mode).
func TestHermesVariantsShareFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	target := filepath.Join(home, ".hermes", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	cfg, routes := hermesFixture()
	anthropic, err := takeover.TemplateByName("hermes", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := anthropic.Rewrite(cfg, nil, routes); err != nil {
		t.Fatal(err)
	}
	openai, err := takeover.TemplateByName("hermes-openai", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := openai.Rewrite(cfg, nil, routes); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(target)
	s := string(b)
	if !strings.Contains(s, "api_mode: anthropic_messages") || !strings.Contains(s, "api_mode: chat_completions") {
		t.Fatalf("both provider entries must coexist:\n%s", s)
	}
	if !strings.Contains(s, "base_url: http://127.0.0.1:15721\n") {
		t.Fatalf("anthropic entry must use the bare URL:\n%s", s)
	}
	// Re-applying the anthropic variant flips the selector back without
	// duplicating anything.
	if err := anthropic.Rewrite(cfg, nil, routes); err != nil {
		t.Fatal(err)
	}
	b2, _ := os.ReadFile(target)
	s2 := string(b2)
	if strings.Count(s2, "api_mode: anthropic_messages") != 1 || strings.Count(s2, "api_mode: chat_completions") != 1 {
		t.Fatalf("duplicate provider entries after re-render:\n%s", s2)
	}
	if !strings.Contains(s2, "provider: model-proxy\n") {
		t.Fatalf("selector did not flip back to the anthropic variant:\n%s", s2)
	}
}

// TestYAMLEmptySurfaceSkips: with no gateway MCP surface the YAML config is
// left byte-identical (comment-preserving round-trip with zero changes).
func TestYAMLEmptySurfaceSkips(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	target := filepath.Join(home, ".hermes", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(hermesSeed), 0o600); err != nil {
		t.Fatal(err)
	}
	tmpl, err := takeover.TemplateByName("hermes-openai", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := tmpl.RewriteOptsFiltered(cfgWith(nil, nil), nil, nil, nil, takeover.TakeoverOptions{}); err != nil {
		t.Fatal(err)
	}
	// yaml.set still applies (provider/model are model scope) — only the MCP
	// surface must be untouched: no mcp entry added, no stale entry cleaned.
	b, _ := os.ReadFile(target)
	s := string(b)
	if !strings.Contains(s, "keep-me:") || !strings.Contains(s, "127.0.0.1:9999/mcp/exa") {
		t.Fatalf("empty gateway surface rewrote mcp_servers:\n%s", s)
	}
}

// TestYAMLDriftPointer: doctor's drift probe reads the named provider's
// base_url out of the YAML document.
func TestYAMLDriftPointer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	target := filepath.Join(home, ".hermes", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	tmpl, err := takeover.TemplateByName("hermes-openai", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg, routes := hermesFixture()
	if err := tmpl.Rewrite(cfg, nil, routes); err != nil {
		t.Fatal(err)
	}
	current, expected := tmpl.Pointer(cfg)
	if expected != "http://127.0.0.1:15721/v1" {
		t.Fatalf("expected = %q", expected)
	}
	if current != expected {
		t.Fatalf("current = %q right after takeover (no drift expected)", current)
	}
	b, _ := os.ReadFile(target)
	if err := os.WriteFile(target, []byte(strings.Replace(string(b), "http://127.0.0.1:15721/v1", "http://127.0.0.1:9999/v1", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	current, _ = tmpl.Pointer(cfg)
	if current != "http://127.0.0.1:9999/v1" {
		t.Fatalf("drifted pointer = %q", current)
	}
}

// TestYAMLTemplateValidation: format yaml requires yaml.set (or an mcp
// block); a yaml template with an mcp block validates.
func TestYAMLTemplateValidation(t *testing.T) {
	if _, err := takeover.ParseTemplate("bad", "test", []byte("file: /tmp/x.yaml\nformat: yaml\n")); err == nil ||
		!strings.Contains(err.Error(), "yaml.set") {
		t.Fatalf("format yaml without set/mcp must fail: %v", err)
	}
	parsed, err := takeover.ParseTemplate("ok", "test", []byte("file: /tmp/x.yaml\nformat: yaml\nmcp:\n  json_path: mcp_servers\n  json_entry: {url: \"{{mcp.url}}\"}\n"))
	if err != nil {
		t.Fatalf("mcp-only yaml template must validate: %v", err)
	}
	if len(parsed) != 1 {
		t.Fatalf("ParseTemplate = %d templates, want 1", len(parsed))
	}
}
