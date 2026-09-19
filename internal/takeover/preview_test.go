package takeover_test

import (
	"encoding/json"
	"model-proxy/internal/config"
	"model-proxy/internal/takeover"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// previewFacts returns facts with metadata warnings disabled and an explicit
// route table (preview templates carry no models: block, so Meta stays nil).
func previewFacts(cfg *config.Config) takeover.ModelFacts {
	return takeover.ModelFacts{Routes: map[string][]config.RouteTarget{}, SourceDefault: -1}
}

// TestPreviewWrites_MergesAndNeverTouchesRealFile covers the core contract:
// the preview renders the exact merged result (proxy keys injected, unrelated
// keys preserved) while the real client file stays byte-identical.
func TestPreviewWrites_MergesAndNeverTouchesRealFile(t *testing.T) {
	dir := t.TempDir()
	claudeFile := filepath.Join(dir, "claude.json")
	templatesDir := writeTemplateOverrides(t, map[string]string{"claude": claudeFile})
	cfg := &config.Config{Listen: "127.0.0.1:15721"}
	original := `{"env":{"OLD":"keep-me"},"other":[1,2]}`
	if err := os.WriteFile(claudeFile, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	writes, err := takeover.PreviewWrites(cfg, "claude", templatesDir, takeover.ModeUnified, previewFacts(cfg), false, takeover.ScopeAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(writes) != 1 {
		t.Fatalf("writes len = %d, want 1", len(writes))
	}
	w := writes[0]
	if w.File != claudeFile || !w.Exists {
		t.Errorf("write = %+v, want file=%s exists=true", w, claudeFile)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(w.Content), &doc); err != nil {
		t.Fatalf("preview content is not JSON: %v\n%s", err, w.Content)
	}
	env, _ := doc["env"].(map[string]any)
	if env["OLD"] != "keep-me" {
		t.Errorf("preview lost unrelated key: %v", doc)
	}
	if !strings.Contains(w.Content, "http://127.0.0.1:15721") || !strings.Contains(w.Content, "PROXY_MANAGED") {
		t.Errorf("preview missing proxy keys:\n%s", w.Content)
	}

	// The real file must be untouched — that is the whole point of a preview.
	after, err := os.ReadFile(claudeFile)
	if err != nil || string(after) != original {
		t.Errorf("preview mutated the real file: %s (err %v)", after, err)
	}
	// And a real takeover on top of the previewed state produces the same bytes.
	if err := takeover.RunTakeover(cfg, "claude", filepath.Join(dir, ".mp"), takeover.ModelFacts{SourceDefault: -1}, templatesDir, takeover.ModeUnified); err != nil {
		t.Fatal(err)
	}
	run, _ := os.ReadFile(claudeFile)
	if string(run) != w.Content {
		t.Errorf("preview content != real takeover result:\npreview: %s\nrun:     %s", w.Content, run)
	}
}

// TestPreviewWrites_MissingFileShowsFreshContent verifies absent client
// configs preview as exists=false with the fresh file takeover would create
// (no ErrNoFile — the run path skips/errors, the preview shows the future).
func TestPreviewWrites_MissingFileShowsFreshContent(t *testing.T) {
	dir := t.TempDir()
	claudeFile := filepath.Join(dir, "absent.json")
	templatesDir := writeTemplateOverrides(t, map[string]string{"claude": claudeFile})
	cfg := &config.Config{Listen: "127.0.0.1:15721"}
	writes, err := takeover.PreviewWrites(cfg, "claude", templatesDir, takeover.ModeUnified, previewFacts(cfg), false, takeover.ScopeAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(writes) != 1 || writes[0].Exists {
		t.Fatalf("writes = %+v, want one non-existing file", writes)
	}
	if !strings.Contains(writes[0].Content, "ANTHROPIC_BASE_URL") {
		t.Errorf("fresh preview missing injected key:\n%s", writes[0].Content)
	}
}

// multiVariantTemplates builds a two-variant family (myagent) sharing one
// JSON config file, plus a claude template for cross-family assertions.
func multiVariantTemplates(t *testing.T, dir, agentFile string) string {
	t.Helper()
	td := filepath.Join(t.TempDir(), "templates")
	if err := os.MkdirAll(td, 0o700); err != nil {
		t.Fatal(err)
	}
	base := "file: " + agentFile + `
format: json
client: myagent
json:
  set:
    providers.{{provider_id}}:
      baseUrl: "{{base_url}}"
      apiKey: "{{token}}"
`
	if err := os.WriteFile(filepath.Join(td, "myagent.yaml"), []byte("protocol: anthropic\n"+base), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(td, "myagent-openai.yaml"), []byte("protocol: openai\nprovider_id: model-proxy-openai\n"+base), 0o600); err != nil {
		t.Fatal(err)
	}
	return td
}

// TestPreviewWrites_FamilyModeSelection mirrors ResolveClientsMode's family
// semantics: unified resolves ONE variant, a protocol mode pins its variant,
// an exact template name pins regardless.
func TestPreviewWrites_FamilyModeSelection(t *testing.T) {
	dir := t.TempDir()
	agentFile := filepath.Join(dir, "myagent.json")
	templatesDir := multiVariantTemplates(t, dir, agentFile)
	cfg := &config.Config{Listen: "127.0.0.1:15721"}

	// Unified: one write for one variant (no route signal → default variant).
	writes, err := takeover.PreviewWrites(cfg, "myagent", templatesDir, takeover.ModeUnified, previewFacts(cfg), false, takeover.ScopeAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(writes) != 1 || len(writes[0].Templates) != 1 || writes[0].Templates[0] != "myagent" {
		t.Fatalf("unified writes = %+v, want single myagent write", writes)
	}

	// Protocol mode pins the openai variant of the family.
	writes, err = takeover.PreviewWrites(cfg, "myagent", templatesDir, takeover.ResolveMode("openai"), previewFacts(cfg), false, takeover.ScopeAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(writes) != 1 || writes[0].Templates[0] != "myagent-openai" {
		t.Fatalf("openai-mode writes = %+v, want myagent-openai", writes)
	}

	// Exact template name pins regardless of the default selection.
	writes, err = takeover.PreviewWrites(cfg, "myagent-openai", templatesDir, takeover.ModeUnified, previewFacts(cfg), false, takeover.ScopeAll)
	if err != nil || len(writes) != 1 || writes[0].Templates[0] != "myagent-openai" {
		t.Fatalf("exact-pin writes = %+v err=%v, want myagent-openai", writes, err)
	}
	// ... but a protocol mode that contradicts the pinned template errors —
	// same contract as the run path (naming the variant + --mode is a user error).
	if _, err := takeover.PreviewWrites(cfg, "myagent-openai", templatesDir, takeover.ResolveMode("anthropic"), previewFacts(cfg), false, takeover.ScopeAll); err == nil ||
		!strings.Contains(err.Error(), "speaks protocol") {
		t.Errorf("contradicting mode on exact pin: want error, got %v", err)
	}
}

// TestPreviewWrites_SplitMergesVariantsPerFile verifies split mode reports
// ONE merged write per file with all contributing variants listed in
// application order — the dialog must show the final document, not one
// partial document per variant.
func TestPreviewWrites_SplitMergesVariantsPerFile(t *testing.T) {
	dir := t.TempDir()
	agentFile := filepath.Join(dir, "myagent.json")
	templatesDir := multiVariantTemplates(t, dir, agentFile)
	cfg := &config.Config{
		Listen: "127.0.0.1:15721",
		Providers: map[string]config.Provider{
			"po": {Provider: "static", OpenAIBaseURL: "http://x", Models: []string{"m1"}},
			"pa": {Provider: "static", AnthropicBaseURL: "http://y", Models: []string{"m2"}},
		},
	}
	facts := takeover.ModelFactsFor(cfg, "myagent", dir, templatesDir, takeover.ModeSplit)
	writes, err := takeover.PreviewWrites(cfg, "myagent", templatesDir, takeover.ModeSplit, facts, false, takeover.ScopeAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(writes) != 1 {
		t.Fatalf("split writes = %+v, want ONE merged file entry", writes)
	}
	if len(writes[0].Templates) != 2 ||
		writes[0].Templates[0] != "myagent" || writes[0].Templates[1] != "myagent-openai" {
		t.Fatalf("split templates = %v, want [myagent myagent-openai]", writes[0].Templates)
	}
	if !strings.Contains(writes[0].Content, "model-proxy-openai") {
		t.Errorf("split preview missing a variant's provider entry:\n%s", writes[0].Content)
	}
	// m2 rides the anthropic variant: both providers present in one document.
	if !strings.Contains(writes[0].Content, `"model-proxy"`) {
		t.Errorf("split preview missing default variant's provider entry:\n%s", writes[0].Content)
	}
}

// TestPreviewWrites_UnknownClientErrors keeps the resolution contract: the
// preview fails exactly where a run would.
func TestPreviewWrites_UnknownClientErrors(t *testing.T) {
	cfg := &config.Config{Listen: "127.0.0.1:15721"}
	if _, err := takeover.PreviewWrites(cfg, "nope", "", takeover.ModeUnified, previewFacts(cfg), false, takeover.ScopeAll); err == nil ||
		!strings.Contains(err.Error(), `unknown takeover client "nope"`) {
		t.Errorf("unknown client: want error, got %v", err)
	}
}

// TestPreviewWrites_ManagedOnly renders from an EMPTY file: Content carries
// only the template's own entries (the editor's "what this template writes"
// view), while Exists still reports the real file's presence.
func TestPreviewWrites_ManagedOnly(t *testing.T) {
	dir := t.TempDir()
	claudeFile := filepath.Join(dir, "claude.json")
	templatesDir := writeTemplateOverrides(t, map[string]string{"claude": claudeFile})
	cfg := &config.Config{Listen: "127.0.0.1:15721"}
	if err := os.WriteFile(claudeFile, []byte(`{"env":{"OLD":"keep-me"},"other":1}`), 0o644); err != nil {
		t.Fatal(err)
	}

	writes, err := takeover.PreviewWrites(cfg, "claude", templatesDir, takeover.ModeUnified, previewFacts(cfg), true, takeover.ScopeAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(writes) != 1 || !writes[0].Exists {
		t.Fatalf("managed-only writes = %+v, want one entry with exists=true", writes)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(writes[0].Content), &doc); err != nil {
		t.Fatalf("managed-only content is not JSON: %v\n%s", err, writes[0].Content)
	}
	if _, has := doc["env"].(map[string]any)["OLD"]; has {
		t.Errorf("managed-only scope leaked the client's own keys:\n%s", writes[0].Content)
	}
	if _, has := doc["other"]; has {
		t.Errorf("managed-only scope leaked unrelated keys:\n%s", writes[0].Content)
	}
	if !strings.Contains(writes[0].Content, "ANTHROPIC_BASE_URL") {
		t.Errorf("managed-only scope missing the template's own entry:\n%s", writes[0].Content)
	}
}

// TestPreviewWritesOpts_SubsetSelections: the dialog's partial takeover —
// an MCP subset writes only the named gateway entries; a model subset only
// the named models. Nil (absent) selections keep everything.
func TestPreviewWritesOpts_SubsetSelections(t *testing.T) {
	dir := t.TempDir()
	agentFile := filepath.Join(dir, "myagent.json")
	templatesDir := multiVariantTemplates(t, dir, agentFile)
	// An MCP-writing template over the same family file.
	mcpTpl := `file: ` + agentFile + `
format: json
client: myagent-mcp
mcp:
  json_path: mcpServers
  json_entry: {type: http, url: "{{mcp.url}}"}
`
	if err := os.WriteFile(filepath.Join(templatesDir, "myagent-mcp.yaml"), []byte(mcpTpl), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Listen: "127.0.0.1:15721",
		Providers: map[string]config.Provider{
			"po": {Provider: "static", OpenAIBaseURL: "http://x", Models: []string{"m1", "m2"}},
		},
		MCP:       map[string]config.MCPServer{"alpha": {URL: "https://a/mcp"}, "beta": {URL: "https://b/mcp"}},
		MCPRoutes: map[string]config.MCPRoute{"gamma": {}},
	}
	facts := takeover.ModelFactsFor(cfg, "", dir, templatesDir, takeover.ModeUnified)

	// MCP subset: only alpha+gamma are written; beta stays out.
	writes, err := takeover.PreviewWritesOpts(cfg, "myagent-mcp", templatesDir,
		takeover.TakeoverOptions{Mode: takeover.ModeUnified, MCP: []string{"alpha", "gamma"}}, facts, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(writes) != 1 {
		t.Fatalf("writes = %+v", writes)
	}
	if !strings.Contains(writes[0].Content, "/mcp/alpha") || !strings.Contains(writes[0].Content, "/mcp/gamma") {
		t.Errorf("selected MCP entries missing:\n%s", writes[0].Content)
	}
	if strings.Contains(writes[0].Content, "/mcp/beta") {
		t.Errorf("unselected MCP entry leaked:\n%s", writes[0].Content)
	}

	// Model subset: only m1 rides the models collection. (A dedicated
	// models-writing template — the shared family fixture only writes the
	// provider entry.)
	modelsTpl := `file: ` + agentFile + `
format: json
client: myagent-models
protocol: openai
json:
  set:
    providers.{{provider_id}}: {baseUrl: "{{base_url}}"}
models:
  shape: pi
  json_path: providers.{{provider_id}}.models
`
	if err := os.WriteFile(filepath.Join(templatesDir, "myagent-models.yaml"), []byte(modelsTpl), 0o600); err != nil {
		t.Fatal(err)
	}
	writes, err = takeover.PreviewWritesOpts(cfg, "myagent-models", templatesDir,
		takeover.TakeoverOptions{Mode: takeover.ModeUnified, Models: []string{"m1"}}, facts, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(writes[0].Content, "m1") {
		t.Errorf("selected model missing:\n%s", writes[0].Content)
	}
	if strings.Contains(writes[0].Content, "m2") {
		t.Errorf("unselected model leaked:\n%s", writes[0].Content)
	}

	// Scope validation is part of the options.
	if _, err := takeover.PreviewWritesOpts(cfg, "myagent", templatesDir,
		takeover.TakeoverOptions{Mode: takeover.ModeUnified, Scope: "bogus"}, facts, true); err == nil {
		t.Error("bogus scope must fail validation")
	}
}

// TestPreviewWritesOpts_ScopeMCPManagedOnly: a managed-only mcp-scope preview
// never touches (nor reports) the main config — only the MCP target shows
// up; the model-scope mirror skips the aux file. Regression: the report
// collector read the main config's never-created private copy and errored.
func TestPreviewWritesOpts_ScopeMCPManagedOnly(t *testing.T) {
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
	if err := os.WriteFile(claudeJSON, []byte(`{"mcpServers":{"mine":{"url":"https://other/mcp"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Listen: "127.0.0.1:15721",
		MCP:    map[string]config.MCPServer{"exa": {URL: "https://mcp.exa.ai/mcp"}},
	}
	facts := takeover.ModelFactsFor(cfg, "", home, home, takeover.ModeUnified)

	writes, err := takeover.PreviewWritesOpts(cfg, "claude", home,
		takeover.TakeoverOptions{Mode: takeover.ModeUnified, Scope: takeover.ScopeMCP}, facts, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(writes) != 1 || writes[0].File != claudeJSON {
		t.Fatalf("mcp-scope writes = %+v, want only the aux ~/.claude.json", writes)
	}
	if !strings.Contains(writes[0].Content, "/mcp/exa") {
		t.Errorf("aux content missing the gateway entry:\n%s", writes[0].Content)
	}
	if strings.Contains(writes[0].Content, "KEEP") {
		t.Errorf("managed-only scope leaked client keys:\n%s", writes[0].Content)
	}

	// Mirror: model scope on a managed-only preview skips the aux entry.
	writes, err = takeover.PreviewWritesOpts(cfg, "claude", home,
		takeover.TakeoverOptions{Mode: takeover.ModeUnified, Scope: takeover.ScopeModel}, facts, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(writes) != 1 || writes[0].File != settings {
		t.Fatalf("model-scope writes = %+v, want only settings.json", writes)
	}
	if !strings.Contains(writes[0].Content, "ANTHROPIC_BASE_URL") {
		t.Errorf("settings content missing the model takeover:\n%s", writes[0].Content)
	}
}

// TestPreviewWritesDraft: the editor's unsaved-draft preview renders the
// DRAFT, not the on-disk template — a modified provider id shows up, an
// invalid draft is a parse error, and no real file is ever touched.
func TestPreviewWritesDraft(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg := &config.Config{
		Listen: "127.0.0.1:15721",
		Providers: map[string]config.Provider{
			"aqp": {Provider: "aqp", OpenAIBaseURL: "http://x", Models: []string{"m1"}},
		},
	}
	drafts := t.TempDir() // an on-disk template dir that must NOT be consulted
	onDisk := `name: claude-disk
file: ` + home + `/.claude/settings.json
format: json
client: claude-disk
json:
  set:
    env.DISK: "yes"
`
	if err := os.WriteFile(filepath.Join(drafts, "claude-draft.yaml"), []byte(onDisk), 0o600); err != nil {
		t.Fatal(err)
	}
	draft := `name: claude-draft
file: ` + home + `/.claude/settings.json
format: json
client: claude-draft
json:
  set:
    env.DRAFT: "yes"
`
	writes, err := takeover.PreviewWritesDraft(cfg, "claude-draft", draft,
		takeover.TakeoverOptions{Mode: takeover.ModeUnified, Scope: takeover.ScopeAll},
		takeover.ModelFacts{SourceDefault: -1}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(writes) != 1 || !strings.Contains(writes[0].Content, `"DRAFT": "yes"`) || strings.Contains(writes[0].Content, "DISK") {
		t.Fatalf("draft preview must carry the draft, not the disk template: %+v", writes)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("draft preview must not write the real client file")
	}
	if _, err := takeover.PreviewWritesDraft(cfg, "claude-draft", "name: [",
		takeover.TakeoverOptions{Mode: takeover.ModeUnified}, takeover.ModelFacts{SourceDefault: -1}, true); err == nil {
		t.Fatalf("invalid draft must be a parse error")
	}
}

// TestPreviewWrites_EscapesRedirectedRealPath: when the real side-file path
// contains characters that need escaping inside a JSON/TOML string (quotes,
// backslashes), the preview replacement must produce a valid config document,
// not corrupt the string with raw text substitution.
func TestPreviewWrites_EscapesRedirectedRealPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// A directory with a quote in its name exercises TOML double-quote escaping.
	realDir := filepath.Join(home, `real"quotes`, ".codex")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	realCatalog := filepath.Join(realDir, "model-proxy-models.json")

	templatesDir := t.TempDir()
	tpl := `description: test codex
file: ` + filepath.Join(home, `.codex`, `config.toml`) + `
format: toml
client: codex
protocol: responses
models:
  shape: codex
  catalog_file: ` + strconv.Quote(realCatalog) + `
toml:
  top_keys:
    model_provider: '"{{provider_id}}"'
`
	if err := os.WriteFile(filepath.Join(templatesDir, "codex.yaml"), []byte(tpl), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Listen: "127.0.0.1:15721",
		Providers: map[string]config.Provider{
			"aqp": {Provider: "aqp", OpenAIBaseURL: "http://x", Models: []string{"glm-5.2"}},
		},
		Routes: map[string][]config.RouteTarget{
			"glm-5.2": {{Provider: "aqp", Model: "glm-5.2", Priority: 1}},
		},
	}
	writes, err := takeover.PreviewWrites(cfg, "codex", templatesDir, takeover.ModeUnified, takeover.ModelFacts{Routes: cfg.Routes, SourceDefault: -1}, false, takeover.ScopeAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(writes) != 1 {
		t.Fatalf("writes = %+v, want 1", writes)
	}
	content := writes[0].Content
	// The real path must appear, and the TOML line must remain parseable as a
	// double-quoted string (the quote inside the path is escaped).
	if !strings.Contains(content, `model_catalog_json = "`+escapeTOMLString(realCatalog)+`"`) {
		t.Fatalf("redirected real path not correctly escaped in TOML:\n%s", content)
	}
}

func escapeTOMLString(s string) string {
	// TOML double-quoted strings use the same escapes as Go string literals for
	// the characters that matter here (", \, \b, \t, \n, \f, \r).
	return strconv.Quote(s)[1 : len(strconv.Quote(s))-1]
}
