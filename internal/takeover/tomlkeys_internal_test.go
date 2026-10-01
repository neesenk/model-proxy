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

// TestRemoveTOMLSectionsWithURL_HeaderTrailingComment: a stale proxy MCP
// section whose header carries a TOML-legal trailing comment
// (`[mcp_servers.exa] # stale`) is still a section header and must be
// cleaned when its body holds the proxy URL — the pre-fix HasSuffix("]")
// precheck rejected the line before tomlHeaderPath could strip the comment,
// so the stale section survived. User sections and non-header lines (full
// comments, key/value lines) must pass through untouched.
func TestRemoveTOMLSectionsWithURL_HeaderTrailingComment(t *testing.T) {
	in := `# top comment
[mcp_servers.exa] # stale proxy entry
url = "http://127.0.0.1:15721/mcp/exa"

[mcp_servers."mine"] # user's own
url = "https://other/mcp"

key = "[brackets in a value]"
`
	out := removeTOMLSectionsWithURL(in, "http://127.0.0.1:15721/mcp/", map[string]bool{`mcp_servers."exa"`: true})
	if strings.Contains(out, "127.0.0.1:15721") || strings.Contains(out, "stale proxy entry") {
		t.Errorf("commented stale proxy section survived:\n%s", out)
	}
	if !strings.Contains(out, `[mcp_servers."mine"] # user's own`) || !strings.Contains(out, "https://other/mcp") {
		t.Errorf("user section dropped:\n%s", out)
	}
	if !strings.Contains(out, "# top comment") || !strings.Contains(out, `key = "[brackets in a value]"`) {
		t.Errorf("non-header lines dropped:\n%s", out)
	}
}

// TestRemoveTOMLSection_MultiLineConstructs: header-shaped lines inside
// multi-line strings (”' or """) and `[`-prefixed lines inside multi-line
// arrays are content, not structure. Removal must take the WHOLE managed
// section — no orphaned value lines left behind (the old boundary scan
// stopped at a `[`-prefixed array element) — and must never eat a user
// multi-line string in a neighboring section (the old header scan matched
// header-shaped lines inside string bodies).
func TestRemoveTOMLSection_MultiLineConstructs(t *testing.T) {
	in := "[docs]\ntext = '''\n[models.\"k3\"] # note\n'''\n\n" +
		"[models.\"k3\"]\nprovider = \"x\"\nfallbacks = [\n [\"a\"]\n]\n\n[keep]\nx = 1\n"
	out := removeTOMLSection(in, `models."k3"`)
	if !strings.Contains(out, "text = '''\n[models.\"k3\"] # note\n'''") {
		t.Errorf("user multi-line string content destroyed:\n%s", out)
	}
	if strings.Contains(out, `provider = "x"`) || strings.Contains(out, `["a"]`) {
		t.Errorf("managed section not fully removed:\n%s", out)
	}
	if !strings.Contains(out, "[keep]\nx = 1") {
		t.Errorf("unrelated section dropped:\n%s", out)
	}
}

// TestRemoveTOMLSectionsWithURL_PreservesLineEndings pins the CRLF contract:
// the old implementation unconditionally normalized CRLF→LF, rewriting every
// line ending of the file even when no section was removed. A no-op run must
// return the input byte-for-byte, and a removing run must keep the original
// endings of the surviving lines.
func TestRemoveTOMLSectionsWithURL_PreservesLineEndings(t *testing.T) {
	user := "[mcp_servers.\"mine\"]\r\nurl = \"https://other/mcp\"\r\n"
	if out := removeTOMLSectionsWithURL(user, "http://127.0.0.1:15721/mcp/", map[string]bool{`mcp_servers."exa"`: true}); out != user {
		t.Errorf("no-op run rewrote bytes:\n%q\nwant:\n%q", out, user)
	}
	stale := "[mcp_servers.exa]\r\nurl = \"http://127.0.0.1:15721/mcp/exa\"\r\n" + user
	if out := removeTOMLSectionsWithURL(stale, "http://127.0.0.1:15721/mcp/", map[string]bool{`mcp_servers."exa"`: true}); out != user {
		t.Errorf("stale section not removed or surviving line endings rewritten:\n%q\nwant:\n%q", out, user)
	}
}

// TestRemoveTOMLSectionsWithURL_MultiLineArrayBody: a `[`-prefixed array
// element line inside a stale section's body is content, not a section
// boundary — the old scan stopped there, so a proxy URL further down the
// body was never seen and the stale section survived cleanup.
func TestRemoveTOMLSectionsWithURL_MultiLineArrayBody(t *testing.T) {
	in := "[mcp_servers.exa]\nfallbacks = [\n [\"a\"]\n]\nurl = \"http://127.0.0.1:15721/mcp/exa\"\n\n" +
		"[mcp_servers.\"mine\"]\nurl = \"https://other/mcp\"\n"
	out := removeTOMLSectionsWithURL(in, "http://127.0.0.1:15721/mcp/", map[string]bool{`mcp_servers."exa"`: true})
	if strings.Contains(out, "127.0.0.1:15721") || strings.Contains(out, "fallbacks") {
		t.Errorf("stale proxy section survived:\n%s", out)
	}
	if !strings.Contains(out, `[mcp_servers."mine"]`) || !strings.Contains(out, "https://other/mcp") {
		t.Errorf("user section dropped:\n%s", out)
	}
}

// TestTakeoverKimiArrayTableConflictFailsClosed is the end-to-end twin of
// TestReplaceOrAppendTOMLSection_ArrayTableConflict: the client config
// already declares the managed provider as an ARRAY of tables
// ([[providers."model-proxy"]]). Writing the plain template table over it
// would redefine the key and TOML would reject the whole config — the
// takeover must fail closed and leave the file byte-for-byte untouched.
func TestTakeoverKimiArrayTableConflictFailsClosed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".kimi-code"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgTOML := filepath.Join(home, ".kimi-code", "config.toml")
	original := "default_model = \"glm-5.3-flash\"\n[[providers.\"model-proxy\"]]\ntype = \"openai\"\n"
	if err := os.WriteFile(cfgTOML, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:15721",
		Providers: map[string]configdomain.Provider{
			"aqp": {OpenAIBaseURL: "http://x", Provider: "aqp", Models: []string{"glm-5.3-flash"}},
		},
	}
	err := RunTakeover(cfg, "kimi", filepath.Join(home, ".mp"), ModelFacts{SourceDefault: -1}, home, ModeUnified)
	if err == nil {
		t.Fatal("takeover over a conflicting array table must fail closed")
	}
	if !strings.Contains(err.Error(), `[[providers."model-proxy"]]`) {
		t.Errorf("error must name the conflicting array table: %v", err)
	}
	data, rerr := os.ReadFile(cfgTOML)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(data) != original {
		t.Errorf("failed takeover rewrote the client config:\n%s", data)
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
		// The full basic-string escape set: if any short escape stops decoding,
		// a client header spelled with it silently stops matching its managed
		// section — the re-takeover duplicate-append incident class (9b8f949).
		{`"a\tb"`, []string{"a\tb"}, true},
		{`"a\bb"`, []string{"a\bb"}, true},
		{`"a\nb"`, []string{"a\nb"}, true},
		{`"a\fb"`, []string{"a\fb"}, true},
		{`"a\rb"`, []string{"a\rb"}, true},
		{`"a\\b"`, []string{`a\b`}, true},
		{`"\u00e9"`, []string{"é"}, true},
		{`"\U0001F680"`, []string{"\U0001F680"}, true}, // 8-digit \U
		{`"trunc\u00"`, []string{`trunc\u00`}, true},   // truncated \u: verbatim, never mangling
		{`"bad\q"`, []string{`bad\q`}, true},           // unknown escape: verbatim
		{`'a\tb'`, []string{`a\tb`}, true},             // literal string keeps escapes verbatim
		{`a..b`, nil, false},                           // empty part
		{`a."unterminated`, nil, false},                // unterminated quote
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

// TestTomlHeaderPath_TrailingComment: TOML allows a comment after the closing
// bracket; the header must still parse. A `#` inside a quoted key part is
// data, not a comment, and an unterminated quote never parses.
func TestTomlHeaderPath_TrailingComment(t *testing.T) {
	parts, isArray, ok := tomlHeaderPath(`[providers.model-proxy] # my note`)
	if !ok || isArray || !equalTOMLKeyPath(parts, []string{"providers", "model-proxy"}) {
		t.Errorf("commented header parse = %v %v %v", parts, isArray, ok)
	}
	parts, isArray, ok = tomlHeaderPath(`[[history]] # array note`)
	if !ok || !isArray || !equalTOMLKeyPath(parts, []string{"history"}) {
		t.Errorf("commented array header parse = %v %v %v", parts, isArray, ok)
	}
	parts, _, ok = tomlHeaderPath(`[providers."a#b"]`)
	if !ok || !equalTOMLKeyPath(parts, []string{"providers", "a#b"}) {
		t.Errorf("hash inside quoted part must not start a comment = %v %v", parts, ok)
	}
	parts, _, ok = tomlHeaderPath(`[providers.'a#b'] # note`)
	if !ok || !equalTOMLKeyPath(parts, []string{"providers", "a#b"}) {
		t.Errorf("hash inside literal part must not start a comment = %v %v", parts, ok)
	}
	if _, _, ok = tomlHeaderPath(`[providers."unterminated # not a comment`); ok {
		t.Error("unterminated quote must not parse as a header")
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

// TestRemoveTOMLSection_BOM: removal sees through a BOM'd header and keeps
// the BOM on the surviving content.
func TestRemoveTOMLSection_BOM(t *testing.T) {
	in := "\uFEFF[providers.\"model-proxy\"]\nbase_url = \"http://old\"\n[keep]\nx = 1\n"
	out := removeTOMLSection(in, `providers."model-proxy"`)
	if strings.Contains(out, "http://old") {
		t.Errorf("BOM'd section not removed:\n%s", out)
	}
	if !strings.HasPrefix(out, "\uFEFF") || !strings.Contains(out, "[keep]") {
		t.Errorf("BOM / unrelated section lost:\n%s", out)
	}
}

// TestRemoveTOMLSectionsWithURL_BOM: stale-section cleanup matches BOM'd
// headers, and a BOM'd file with nothing to remove comes back byte-identical
// (the zero-deletion contract must survive BOM handling).
func TestRemoveTOMLSectionsWithURL_BOM(t *testing.T) {
	in := "\uFEFF[mcp_servers.exa]\nurl = \"http://127.0.0.1:15721/mcp/exa\"\n[keep]\nx = 1\n"
	out := removeTOMLSectionsWithURL(in, "127.0.0.1:15721/mcp/", map[string]bool{"mcp_servers.exa": true})
	if strings.Contains(out, "mcp/exa") {
		t.Errorf("BOM'd stale MCP section not removed:\n%s", out)
	}
	if !strings.HasPrefix(out, "\uFEFF") || !strings.Contains(out, "[keep]") {
		t.Errorf("BOM / unrelated section lost:\n%s", out)
	}

	noMatch := "\uFEFF[mcp_servers.user]\nurl = \"https://elsewhere\"\n"
	if out := removeTOMLSectionsWithURL(noMatch, "127.0.0.1:15721/mcp/", map[string]bool{"mcp_servers.exa": true}); out != noMatch {
		t.Errorf("zero-deletion run must stay byte-identical:\nin:  %q\nout: %q", noMatch, out)
	}
}

// TestTakeoverKimiDottedKeyConflictFailsClosed: the dotted-key spelling of
// the managed table is the inline/dotted counterpart of the [[array]]
// conflict — takeover must refuse (file untouched) instead of appending a
// header that redefines the key and bricks the client config.
func TestTakeoverKimiDottedKeyConflictFailsClosed(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".kimi-code"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfgTOML := filepath.Join(home, ".kimi-code", "config.toml")
	original := "default_model = \"glm-5.3-flash\"\nproviders.\"model-proxy\" = { type = \"openai\" }\n"
	if err := os.WriteFile(cfgTOML, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:15721",
		Providers: map[string]configdomain.Provider{
			"aqp": {OpenAIBaseURL: "http://x", Provider: "aqp", Models: []string{"glm-5.3-flash"}},
		},
	}
	err := RunTakeover(cfg, "kimi", filepath.Join(home, ".mp"), ModelFacts{SourceDefault: -1}, home, ModeUnified)
	if err == nil {
		t.Fatal("takeover over a dotted-key definition of the managed table must fail closed")
	}
	if !strings.Contains(err.Error(), `providers."model-proxy"`) {
		t.Errorf("error must name the conflicting key line: %v", err)
	}
	data, rerr := os.ReadFile(cfgTOML)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(data) != original {
		t.Errorf("failed takeover rewrote the client config:\n%s", data)
	}
}
