package takeover

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	configdomain "model-proxy/internal/config"
)

// tomlkeys_internal_test.go covers the unexported TOML editors' semantic
// matching (see tomlkeys_test.go for the public-surface contract).

// TestRemoveTOMLSection_MatchesAllSpellings: removal (also_remove legacy
// cleanup) is semantic — quote/whitespace variants of the SAME table all go,
// and every duplicate goes. The unquoted dotted form of a dotted-key model
// id (`models.glm-5.3-flash` = models→glm-5→"3-flash") is a DIFFERENT table
// in TOML and must not be touched by the quoted target (also_remove exists
// for exactly that legacy form).
func TestRemoveTOMLSection_MatchesAllSpellings(t *testing.T) {
	in := "keep = 1\n[models.k3]\nprovider = \"x\"\n[models.\"k3\"]\nprovider = \"y\"\n[models.'k3']\nprovider = \"z\"\n[models.other]\nprovider = \"w\"\n"
	out := removeTOMLSection(in, `models."k3"`)
	if strings.Contains(out, "k3") {
		t.Errorf("semantic spellings not removed:\n%s", out)
	}
	if !strings.Contains(out, "[models.other]") || !strings.Contains(out, "keep = 1") {
		t.Errorf("unrelated content dropped:\n%s", out)
	}

	legacy := "[models.glm-5.3-flash]\nprovider = \"x\"\n"
	if out := removeTOMLSection(legacy, `models."glm-5.3-flash"`); out != legacy {
		t.Errorf("unquoted dotted form must not match the quoted target (different table):\n%s", out)
	}
	if out := removeTOMLSection(legacy, "models.glm-5.3-flash"); out == legacy {
		t.Errorf("also_remove target must match the unquoted legacy form:\n%s", out)
	}
}

// TestRemoveTOMLSectionsWithURL_SemanticMatch: stale proxy MCP cleanup
// recognizes client-normalized spellings of the generated namespace.
func TestRemoveTOMLSectionsWithURL_SemanticMatch(t *testing.T) {
	in := `[mcp_servers.exa]
url = "http://127.0.0.1:15721/mcp/exa"

[mcp_servers."mine"]
url = "https://other/mcp"

[mcp_servers.'exa']
url = "http://127.0.0.1:15721/mcp/exa"
`
	out := removeTOMLSectionsWithURL(in, "http://127.0.0.1:15721/mcp/", map[string]bool{`mcp_servers."exa"`: true})
	if strings.Contains(out, "127.0.0.1:15721") {
		t.Errorf("stale proxy sections survived:\n%s", out)
	}
	if !strings.Contains(out, `[mcp_servers."mine"]`) {
		t.Errorf("user section dropped:\n%s", out)
	}
}

// TestParseTOMLKeyPath unit-cases the key-path parser: quoting styles,
// whitespace, escapes, and structural failures.
func TestParseTOMLKeyPath(t *testing.T) {
	cases := []struct {
		in   string
		want []string
		ok   bool
	}{
		{`providers."model-proxy"`, []string{"providers", "model-proxy"}, true},
		{`providers.model-proxy`, []string{"providers", "model-proxy"}, true},
		{` providers . 'model-proxy' `, []string{"providers", "model-proxy"}, true},
		{`models."glm-5.3-flash"`, []string{"models", "glm-5.3-flash"}, true},
		{`a."b.c".d`, []string{"a", "b.c", "d"}, true},
		{`plain`, []string{"plain"}, true},
		{`"with\"quote"`, []string{"with\"quote"}, true},
		{`"\u0041bc"`, []string{"Abc"}, true},
		{`a..b`, nil, false},            // empty part
		{`a."unterminated`, nil, false}, // unterminated quote
	}
	for _, c := range cases {
		got, ok := parseTOMLKeyPath(c.in)
		if ok != c.ok {
			t.Errorf("parseTOMLKeyPath(%q) ok = %v, want %v", c.in, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if !equalTOMLKeyPath(got, c.want) {
			t.Errorf("parseTOMLKeyPath(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestTomlHeaderPath_ArrayDetection: [[x]] parses as an array header and
// never equals a plain target.
func TestTomlHeaderPath_ArrayDetection(t *testing.T) {
	parts, isArray, ok := tomlHeaderPath("[[history]]")
	if !ok || !isArray || !equalTOMLKeyPath(parts, []string{"history"}) {
		t.Fatalf("array header parse = %v %v %v", parts, isArray, ok)
	}
	parts, isArray, ok = tomlHeaderPath("[history]")
	if !ok || isArray || !equalTOMLKeyPath(parts, []string{"history"}) {
		t.Fatalf("plain header parse = %v %v %v", parts, isArray, ok)
	}
}

// TestTomlSectionScalar_SemanticHeader: drift reads follow client-rewritten
// header spellings.
func TestTomlSectionScalar_SemanticHeader(t *testing.T) {
	text := "[providers.model-proxy]\ntype = \"openai\"\nbase_url = \"http://127.0.0.1:15721/v1\"\n"
	if got := tomlSectionScalar(text, `providers."model-proxy"`, "base_url"); got != "http://127.0.0.1:15721/v1" {
		t.Errorf("scalar = %q, want the rewritten header's base_url", got)
	}
}

// TestRetakeoverKimiAfterClientRewrite is the reported incident end-to-end:
// takeover → kimi-cli rewrites config.toml (drops quotes where the bare key
// is valid, keeps user content) → re-takeover must leave exactly one
// providers table and one table per model — not stacked duplicates (TOML
// rejects duplicate tables with a parse error). A third pass is a no-op.
func TestRetakeoverKimiAfterClientRewrite(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".kimi-code"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgTOML := filepath.Join(home, ".kimi-code", "config.toml")
	if err := os.WriteFile(cfgTOML, []byte("default_model = \"glm-5.3-flash\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:15721",
		Providers: map[string]configdomain.Provider{
			"aqp": {OpenAIBaseURL: "http://x", Provider: "aqp", Models: []string{"glm-5.3-flash"}},
		},
	}
	bakDir := filepath.Join(home, ".mp")
	for i := 0; i < 2; i++ {
		if err := RunTakeover(cfg, "kimi", bakDir, ModelFacts{SourceDefault: -1}, home, ModeUnified); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			// Simulate kimi-cli rewriting its config between the two
			// takeovers: quotes dropped where the bare key is valid (the
			// hyphenated provider id needs none; the dotted model id MUST
			// keep quotes — single quotes are the realistic re-spelling),
			// plus a user-added provider.
			clientRewritten := "default_model = \"glm-5.3-flash\"\n" +
				"[providers.model-proxy]\ntype = \"openai\"\nbase_url = \"http://127.0.0.1:15721/v1\"\napi_key = \"PROXY_MANAGED\"\n" +
				"[providers.mine]\ntype = \"openai\"\nbase_url = \"https://mine\"\n" +
				"[models.'glm-5.3-flash']\nprovider = \"model-proxy\"\nmodel = \"glm-5.3-flash\"\nmax_context_size = 200000\n"
			if err := os.WriteFile(cfgTOML, []byte(clientRewritten), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}

	data, err := os.ReadFile(cfgTOML)
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	if got := countTables(out, "providers", "model-proxy"); got != 1 {
		t.Fatalf("model-proxy provider table appears %d times after re-takeover, want 1:\n%s", got, out)
	}
	if got := countTables(out, "models", "glm-5.3-flash"); got != 1 {
		t.Fatalf("model table appears %d times after re-takeover, want 1:\n%s", got, out)
	}
	if !strings.Contains(out, "[providers.mine]") {
		t.Errorf("user's own provider dropped:\n%s", out)
	}
	if got := strings.Count(out, "default_model ="); got != 1 {
		t.Errorf("default_model appears %d times:\n%s", got, out)
	}
	// A third pass over the canonical form is a byte-identical no-op.
	before := out
	if err := RunTakeover(cfg, "kimi", bakDir, ModelFacts{SourceDefault: -1}, home, ModeUnified); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(cfgTOML); err != nil || string(data) != before {
		t.Errorf("third takeover not idempotent:\nbefore:\n%s\nafter:\n%s", before, string(data))
	}
}

// countTables counts plain table headers semantically equal to the wanted
// key path, using the production parser (quoting/whitespace agnostic).
func countTables(text string, want ...string) int {
	count := 0
	for _, line := range strings.Split(text, "\n") {
		parts, isArray, ok := tomlHeaderPath(strings.TrimSpace(line))
		if ok && !isArray && equalTOMLKeyPath(parts, want) {
			count++
		}
	}
	return count
}

// TestRewriteEnv_CollapsesDuplicatesAndExportPrefix: the env editor must not
// grow duplicate managed keys on re-takeover — an `export ` prefix added by
// hand and duplicate KEY lines collapse into one canonical line.
func TestRewriteEnv_CollapsesDuplicatesAndExportPrefix(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "env-test")
	if err := os.WriteFile(file, []byte("# comment\nGOOGLE_GEMINI_BASE_URL=https://old\nexport GOOGLE_GEMINI_BASE_URL=https://older\nOTHER=keep\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tmpl := &Template{
		Name:   "env-test",
		File:   file,
		Format: "env",
		Env: &EnvTemplate{Set: map[string]string{
			"GOOGLE_GEMINI_BASE_URL": "{{base_url}}",
			"GEMINI_API_KEY":         "{{token}}",
		}},
	}
	ctx := renderContext{baseURL: "http://127.0.0.1:15721/v1"}
	if err := tmpl.rewriteEnv(ctx, ScopeAll); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	out := string(data)
	if got := strings.Count(out, "GOOGLE_GEMINI_BASE_URL="); got != 1 {
		t.Errorf("managed key appears %d times, want 1:\n%s", got, out)
	}
	if !strings.Contains(out, "GOOGLE_GEMINI_BASE_URL=http://127.0.0.1:15721/v1") ||
		strings.Contains(out, "https://old") || strings.Contains(out, "export ") {
		t.Errorf("managed key not canonically rewritten:\n%s", out)
	}
	if !strings.Contains(out, "# comment") || !strings.Contains(out, "OTHER=keep") {
		t.Errorf("unmanaged lines dropped:\n%s", out)
	}
	if !strings.Contains(out, "GEMINI_API_KEY=") {
		t.Errorf("missing key not appended:\n%s", out)
	}
}
