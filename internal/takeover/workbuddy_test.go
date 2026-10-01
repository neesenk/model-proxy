package takeover_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"model-proxy/internal/catalog"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/takeover"
)

// workbuddyFixture: one exposed reasoning+image model with metadata.
func workbuddyFixture() (*configdomain.Config, map[string]map[string]catalog.Model, map[string][]configdomain.RouteTarget) {
	cfg := cfgWith(map[string]configdomain.Provider{
		"zhipu": {OpenAIBaseURL: "https://z/v1", Models: []string{"glm-5.3"}},
	}, nil)
	cfg.MCP = map[string]configdomain.MCPServer{
		"exa": {URL: "https://mcp.exa.ai/mcp", Auth: "none"},
	}
	meta := map[string]map[string]catalog.Model{
		"zhipu": {"glm-5.3": {
			Reasoning:  true,
			ToolCall:   true,
			Context:    1000000,
			Output:     128000,
			Modalities: catalog.Modalities{Input: []string{"text", "image"}},
		}},
	}
	routes := map[string][]configdomain.RouteTarget{
		"glm-5.3": {{Provider: "zhipu", Model: "glm-5.3", Priority: 1}},
	}
	return cfg, meta, routes
}

// TestWorkbuddyPresetRendering: the workbuddy preset renders every exposed
// model as a vendor-marked custom-model entry in models.json (lifting an
// array-rooted original into {models: [...]}), preserving the user's own
// custom models, and merges the gateway MCP surface into the aux mcp.json.
func TestWorkbuddyPresetRendering(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	main := filepath.Join(home, ".workbuddy", "models.json")
	aux := filepath.Join(home, ".workbuddy", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(main), 0o700); err != nil {
		t.Fatal(err)
	}
	// Seed an ARRAY-rooted models.json with a user model, plus an mcp.json
	// with a user server and a stale proxy entry.
	if err := os.WriteFile(main, []byte(`[{"id":"my-local","name":"my-local","vendor":"ollama","url":"http://127.0.0.1:11434/v1"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(aux, []byte(`{"mcpServers":{"keep-me":{"url":"https://other.example/mcp"},"exa":{"url":"http://127.0.0.1:9999/mcp/exa"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tmpl, err := takeover.TemplateByName("workbuddy", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg, meta, routes := workbuddyFixture()
	if err := tmpl.Rewrite(cfg, meta, routes); err != nil {
		t.Fatal(err)
	}

	v := readJSONFile(t, main)
	models, _ := v["models"].([]any)
	if len(models) != 2 {
		t.Fatalf("models = %v, want user entry + ours", models)
	}
	user, _ := models[0].(map[string]any)
	if user["id"] != "my-local" || user["vendor"] != "ollama" {
		t.Fatalf("user custom model dropped or rewritten: %v", user)
	}
	ours, _ := models[1].(map[string]any)
	if ours["id"] != "glm-5.3" || ours["vendor"] != "model-proxy" {
		t.Fatalf("our entry = %v", ours)
	}
	if ours["url"] != "http://127.0.0.1:15721/v1" || ours["apiKey"] != "PROXY_MANAGED" {
		t.Fatalf("entry url/apiKey = %v", ours)
	}
	if ours["maxInputTokens"].(float64) != 1000000 || ours["maxOutputTokens"].(float64) != 128000 {
		t.Fatalf("token limits = %v", ours)
	}
	if ours["supportsToolCall"] != true || ours["supportsImages"] != true || ours["supportsReasoning"] != true {
		t.Fatalf("capabilities = %v", ours)
	}

	// MCP aux: user server preserved, stale cleaned, surface written.
	mv := readJSONFile(t, aux)
	servers, _ := mv["mcpServers"].(map[string]any)
	if _, ok := servers["keep-me"]; !ok {
		t.Fatalf("user mcp server dropped: %v", mv)
	}
	exa, ok := servers["exa"].(map[string]any)
	if !ok || exa["url"] != "http://127.0.0.1:15721/mcp/exa" {
		t.Fatalf("gateway mcp entry = %v", servers)
	}

	// Idempotent: re-render produces identical bytes on both files.
	b1, _ := os.ReadFile(main)
	a1, _ := os.ReadFile(aux)
	if err := tmpl.Rewrite(cfg, meta, routes); err != nil {
		t.Fatal(err)
	}
	b2, _ := os.ReadFile(main)
	a2, _ := os.ReadFile(aux)
	if string(b1) != string(b2) || string(a1) != string(a2) {
		t.Fatalf("re-render changed files:\n%s\n---\n%s", b2, a2)
	}
}

// TestWorkbuddyModelShrink: a re-takeover with fewer exposed models drops
// the stale vendor-marked entries and keeps the user's own.
func TestWorkbuddyModelShrink(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	main := filepath.Join(home, ".workbuddy", "models.json")
	if err := os.MkdirAll(filepath.Dir(main), 0o700); err != nil {
		t.Fatal(err)
	}
	tmpl, err := takeover.TemplateByName("workbuddy", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg, meta, routes := workbuddyFixture()
	routes["glm-5.3-flash"] = []configdomain.RouteTarget{{Provider: "zhipu", Model: "glm-5.3", Priority: 1}}
	if err := tmpl.Rewrite(cfg, meta, routes); err != nil {
		t.Fatal(err)
	}
	if n := len(modelRulesWB(t, main)); n != 2 {
		t.Fatalf("initial render: %d entries, want 2", n)
	}
	if err := tmpl.Rewrite(cfg, meta, routes1WB()); err != nil {
		t.Fatal(err)
	}
	entries := modelRulesWB(t, main)
	if len(entries) != 1 || entries[0].(map[string]any)["id"] != "glm-5.3" {
		t.Fatalf("stale entry survived shrink: %v", entries)
	}
}

func modelRulesWB(t *testing.T, path string) []any {
	t.Helper()
	v := readJSONFile(t, path)
	models, _ := v["models"].([]any)
	return models
}

func routes1WB() map[string][]configdomain.RouteTarget {
	return map[string][]configdomain.RouteTarget{
		"glm-5.3": {{Provider: "zhipu", Model: "glm-5.3", Priority: 1}},
	}
}

// TestWorkbuddyDriftPointer: the drift probe navigates the custom-model
// array by the vendor marker and compares the live url.
func TestWorkbuddyDriftPointer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	main := filepath.Join(home, ".workbuddy", "models.json")
	if err := os.MkdirAll(filepath.Dir(main), 0o700); err != nil {
		t.Fatal(err)
	}
	tmpl, err := takeover.TemplateByName("workbuddy", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg, meta, routes := workbuddyFixture()
	if err := tmpl.Rewrite(cfg, meta, routes); err != nil {
		t.Fatal(err)
	}
	current, expected := tmpl.Pointer(cfg)
	if expected != "http://127.0.0.1:15721/v1" {
		t.Fatalf("expected = %q", expected)
	}
	if current != expected {
		t.Fatalf("current = %q right after takeover (no drift expected)", current)
	}
	b, _ := os.ReadFile(main)
	if err := os.WriteFile(main, []byte(strings.Replace(string(b), "http://127.0.0.1:15721/v1", "http://127.0.0.1:9999/v1", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	current, _ = tmpl.Pointer(cfg)
	if current != "http://127.0.0.1:9999/v1" {
		t.Fatalf("drifted pointer = %q", current)
	}
}

// TestWorkbuddyScopes: ScopeMCP writes only the aux mcp.json (models.json
// not created); ScopeModel writes only models.json.
func TestWorkbuddyScopes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	main := filepath.Join(home, ".workbuddy", "models.json")
	aux := filepath.Join(home, ".workbuddy", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(main), 0o700); err != nil {
		t.Fatal(err)
	}
	tmpl, err := takeover.TemplateByName("workbuddy", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg, meta, routes := workbuddyFixture()
	if err := tmpl.RewriteScoped(cfg, meta, routes, takeover.ScopeMCP); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(main); !os.IsNotExist(err) {
		t.Fatalf("ScopeMCP created models.json: %v", err)
	}
	if _, err := os.Stat(aux); err != nil {
		t.Fatalf("ScopeMCP did not write mcp.json: %v", err)
	}
	if err := os.Remove(aux); err != nil {
		t.Fatal(err)
	}
	if err := tmpl.RewriteScoped(cfg, meta, routes, takeover.ScopeModel); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(main); err != nil {
		t.Fatalf("ScopeModel did not write models.json: %v", err)
	}
	if _, err := os.Stat(aux); !os.IsNotExist(err) {
		t.Fatalf("ScopeModel created mcp.json: %v", err)
	}
}
