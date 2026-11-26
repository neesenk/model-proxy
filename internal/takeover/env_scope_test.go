package takeover_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/takeover"
)

// ScopeMCP promises "MCP surface only; provider/models untouched". An
// env-format template carries nothing but provider/model keys (base URL, API
// key) and has no MCP surface, so an mcp-scoped takeover must leave the file
// exactly as it found it. rewriteEnv used to ignore the scope: an MCP-only
// takeover with gemini-cli selected rewrote the base URL and API key of a
// client whose provider config nobody asked to touch.
func TestEnvTemplateScopeMCPWritesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	target := filepath.Join(home, ".gemini", ".env")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	const seed = "GOOGLE_GEMINI_BASE_URL=https://generativelanguage.googleapis.com/v1beta\nGEMINI_API_KEY=users-own-key\n"
	if err := os.WriteFile(target, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	tmpl, err := takeover.TemplateByName("gemini-cli", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := cfgWith(map[string]configdomain.Provider{
		"a": {OpenAIBaseURL: "https://a/v1", Models: []string{"m"}},
	}, nil)
	routes := map[string][]configdomain.RouteTarget{"m": {{Provider: "a", Model: "m", Priority: 1}}}

	if err := tmpl.RewriteScoped(cfg, nil, routes, takeover.ScopeMCP); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != seed {
		t.Fatalf("an mcp-scoped takeover rewrote the provider keys of an env template:\n%s", got)
	}

	// The same template under the model scope writes them, as always.
	if err := tmpl.RewriteScoped(cfg, nil, routes, takeover.ScopeModel); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "GOOGLE_GEMINI_BASE_URL=http://127.0.0.1:15721/v1\n") ||
		strings.Contains(string(got), "users-own-key") {
		t.Fatalf("a model-scoped takeover did not write the provider keys:\n%s", got)
	}
}
