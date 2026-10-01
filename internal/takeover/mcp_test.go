package takeover_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/takeover"
)

// cfgWithMCP builds a config with two MCP servers and one route.
func cfgWithMCP() *configdomain.Config {
	cfg := cfgWith(map[string]configdomain.Provider{
		"zhipu": {OpenAIBaseURL: "https://z/v1", Models: []string{"glm-5.3"}},
	}, nil)
	cfg.MCP = map[string]configdomain.MCPServer{
		"zhipu-search": {Provider: "zhipu", URL: "https://open.bigmodel.cn/api/mcp/web_search_prime/mcp"},
		"exa":          {URL: "https://mcp.exa.ai/mcp", Auth: "none"},
	}
	cfg.MCPRoutes = map[string]configdomain.MCPRoute{
		"web-search": {Targets: []configdomain.MCPRouteTarget{
			{Server: "zhipu-search", Tools: map[string]string{"web_search": "web_search_prime"}},
		}},
	}
	return cfg
}

// TestMCPJSONRendering: the merged claude preset writes the gateway surface
// into ~/.claude.json mcpServers (its own mcp.file, separate from the model
// takeover's settings.json), preserving unrelated content, idempotently.
func TestMCPJSONRendering(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	target := filepath.Join(home, ".claude.json")
	// Seed with a stale proxy entry (name in the current generated namespace
	// but pointing at an old proxy URL), a user server elsewhere, and a user
	// server that happens to share the proxy URL prefix but is not in the
	// generated namespace — the latter must survive cleanup.
	if err := os.WriteFile(target, []byte(`{"theme":"dark","mcpServers":{"keep-me":{"type":"http","url":"https://other.example/mcp"},"same-prefix-user":{"type":"http","url":"http://127.0.0.1:15721/mcp/same-prefix-user"},"exa":{"type":"http","url":"http://127.0.0.1:9999/mcp/exa"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tmpl, err := takeover.TemplateByName("claude", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := cfgWithMCP()
	if err := tmpl.Rewrite(cfg, nil, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if v["theme"] != "dark" {
		t.Fatalf("unrelated content dropped: %s", data)
	}
	servers, _ := v["mcpServers"].(map[string]any)
	// Default prune: zhipu-search is aggregated into the web-search route, so
	// only the route and the unrouted exa server render.
	for _, name := range []string{"exa", "web-search"} {
		entry, ok := servers[name].(map[string]any)
		if !ok {
			t.Fatalf("mcpServers missing %q: %s", name, data)
		}
		if entry["type"] != "http" || entry["url"] != "http://127.0.0.1:15721/mcp/"+name {
			t.Fatalf("entry %q = %v", name, entry)
		}
	}
	if _, ok := servers["zhipu-search"]; ok {
		t.Fatalf("routed member rendered separately (prune broken): %s", data)
	}
	if _, ok := servers["keep-me"]; !ok {
		t.Fatalf("pre-existing server dropped: %s", data)
	}
	if _, ok := servers["same-prefix-user"]; !ok {
		t.Fatalf("user server sharing the proxy URL prefix was dropped: %s", data)
	}
	// Idempotent: second render produces identical bytes.
	if err := tmpl.Rewrite(cfg, nil, nil); err != nil {
		t.Fatal(err)
	}
	data2, _ := os.ReadFile(target)
	if string(data2) != string(data) {
		t.Fatal("second render changed the file")
	}
}

// TestMCPPiAuxFileRendering: the pi preset carries a top-level shared mcp
// block writing the gateway surface into ~/.pi/agent/mcp.json — the Pi-owned
// global MCP file the pi-mcp-adapter extension reads (`mcpServers` entries,
// transport inferred from `url`, no `type` field). Every variant of the
// family shares the block, so the openai variant lands in the same aux file
// next to its own models.json provider entry.
func TestMCPPiAuxFileRendering(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	target := filepath.Join(home, ".pi", "agent", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	// Seed with the adapter's own settings block, a user server, and a stale
	// proxy entry from an old run (old proxy port).
	if err := os.WriteFile(target, []byte(`{"settings":{"ancestorConfigRoots":[]},"mcpServers":{"mine":{"command":"npx","args":["-y","some-mcp"]},"exa":{"url":"http://127.0.0.1:9999/mcp/exa"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tmpl, err := takeover.TemplateByName("pi", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := cfgWithMCP()
	if err := tmpl.Rewrite(cfg, nil, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if _, ok := v["settings"].(map[string]any); !ok {
		t.Fatalf("adapter settings block dropped: %s", data)
	}
	servers, _ := v["mcpServers"].(map[string]any)
	for _, name := range []string{"exa", "web-search"} {
		entry, ok := servers[name].(map[string]any)
		if !ok {
			t.Fatalf("mcpServers missing %q: %s", name, data)
		}
		if entry["url"] != "http://127.0.0.1:15721/mcp/"+name {
			t.Fatalf("entry %q = %v", name, entry)
		}
		// type: http rides along for the stricter readers (pi-mcp-client
		// validates it explicitly; pi-mcp-adapter ignores it and discriminates
		// by url) — same shape the claude preset writes.
		if entry["type"] != "http" {
			t.Fatalf("entry %q must carry type=http for cross-adapter compat: %v", name, entry)
		}
	}
	if _, ok := servers["zhipu-search"]; ok {
		t.Fatalf("routed member rendered separately: %s", data)
	}
	mine, ok := servers["mine"].(map[string]any)
	if !ok || mine["command"] != "npx" {
		t.Fatalf("user stdio server dropped: %s", data)
	}

	// The openai variant shares the same aux file: rendering it merges into
	// the same document (idempotent surface, user entries intact).
	openai, err := takeover.TemplateByName("pi-openai", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := openai.Rewrite(cfg, nil, nil); err != nil {
		t.Fatal(err)
	}
	data2, _ := os.ReadFile(target)
	var v2 map[string]any
	if err := json.Unmarshal(data2, &v2); err != nil {
		t.Fatal(err)
	}
	servers2, _ := v2["mcpServers"].(map[string]any)
	if len(servers2) != 3 { // exa + web-search + mine
		t.Fatalf("variant rewrite changed the surface: %s", data2)
	}

	// Idempotent: a second pi render produces identical bytes.
	if err := tmpl.Rewrite(cfg, nil, nil); err != nil {
		t.Fatal(err)
	}
	data3, _ := os.ReadFile(target)
	if string(data3) != string(data2) {
		t.Fatal("second render changed the aux file")
	}
}

// TestMCPTOMLRendering: the codex preset appends one [mcp_servers."<name>"]
// section per gateway entry, idempotently.
func TestMCPTOMLRendering(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	target := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	// Seed with a stale proxy section (name in the current generated namespace
	// but pointing at an old proxy URL), a user section elsewhere, and a user
	// section that shares the proxy URL prefix but is not in the generated
	// namespace — the latter must survive cleanup.
	if err := os.WriteFile(target, []byte("# codex config\n[mcp_servers.\"exa\"]\nurl = \"http://127.0.0.1:9999/mcp/exa\"\n[mcp_servers.\"same-prefix-user\"]\nurl = \"http://127.0.0.1:15721/mcp/same-prefix-user\"\n[mcp_servers.\"keep-me\"]\nurl = \"https://other.example/mcp\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tmpl, err := takeover.TemplateByName("codex", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := cfgWithMCP()
	if err := tmpl.Rewrite(cfg, nil, nil); err != nil {
		t.Fatal(err)
	}
	text, _ := os.ReadFile(target)
	// Default prune: the route plus the unrouted server only.
	for _, name := range []string{"exa", "web-search"} {
		section := `[mcp_servers."` + name + `"]`
		if !strings.Contains(string(text), section) {
			t.Fatalf("missing section %s:\n%s", section, text)
		}
	}
	if strings.Contains(string(text), `[mcp_servers."zhipu-search"]`) {
		t.Fatalf("routed member rendered separately (prune broken):\n%s", text)
	}
	if !strings.Contains(string(text), `url = "http://127.0.0.1:15721/mcp/web-search"`) {
		t.Fatalf("route URL missing:\n%s", text)
	}
	if !strings.Contains(string(text), `[mcp_servers."same-prefix-user"]`) {
		t.Fatalf("user section sharing the proxy URL prefix was dropped:\n%s", text)
	}
	if strings.Contains(string(text), "http://127.0.0.1:9999/mcp/exa") {
		t.Fatalf("stale proxy URL not rewritten:\n%s", text)
	}
	if err := tmpl.Rewrite(cfg, nil, nil); err != nil {
		t.Fatal(err)
	}
	text2, _ := os.ReadFile(target)
	if strings.Count(string(text2), `[mcp_servers."exa"]`) != 1 {
		t.Fatalf("duplicate mcp section after re-render:\n%s", text2)
	}
	if !strings.Contains(string(text2), `[mcp_servers."keep-me"]`) {
		t.Fatalf("unrelated mcp section dropped:\n%s", text2)
	}
}

// TestMCPEmptySurfaceSkips: with no mcp: config the gateway surface is empty,
// so the client's existing MCP section is left untouched — even entries that
// look like proxy leftovers are preserved because the template has nothing to
// generate and therefore no namespace to clean.
func TestMCPEmptySurfaceSkips(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	target := filepath.Join(home, ".claude.json")
	original := `{"mcpServers":{"keep-me":{"type":"http","url":"https://other.example/mcp"},"exa":{"type":"http","url":"http://127.0.0.1:15721/mcp/exa"}}}`
	if err := os.WriteFile(target, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	tmpl, err := takeover.TemplateByName("claude", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := tmpl.Rewrite(cfgWith(nil, nil), nil, nil); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(target)
	var v map[string]any
	json.Unmarshal(data, &v)
	servers, _ := v["mcpServers"].(map[string]any)
	if len(servers) != 2 {
		t.Fatalf("empty gateway surface rewrote mcpServers: %s", data)
	}
	if _, ok := servers["keep-me"]; !ok {
		t.Fatalf("existing entry dropped: %s", data)
	}
	if _, ok := servers["exa"]; !ok {
		t.Fatalf("proxy-looking entry dropped when gateway surface empty: %s", data)
	}
}

// TestMCPJSONEntryValidation: malformed mcp blocks fail template parsing.
func TestMCPJSONEntryValidation(t *testing.T) {
	if _, err := takeover.ParseTemplate("bad", "test", []byte(`
file: /tmp/x.json
format: json
json:
  set: {a: b}
mcp:
  json_path: mcpServers
`)); err == nil || !strings.Contains(err.Error(), "json_entry") {
		t.Fatalf("missing json_entry must fail: %v", err)
	}
	if _, err := takeover.ParseTemplate("bad2", "test", []byte(`
file: /tmp/x.env
format: env
env:
  set: {A: b}
mcp:
  json_path: mcpServers
  json_entry: {type: http}
`)); err == nil || !strings.Contains(err.Error(), "format env") {
		t.Fatalf("env format must reject mcp: %v", err)
	}
}

// TestMCPPointerNoDriftProbe: an mcp-only template (nil json: block — user
// defined, the merged claude preset carries a probe) must report "(no drift
// probe)" from doctor's Pointer instead of panicking (regression: nil-pointer
// deref after takeover).
func TestMCPPointerNoDriftProbe(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	parsed, err := takeover.ParseTemplate("my-mcp", "test", []byte("file: ~/.myagent.json\nformat: json\nmcp:\n  json_path: mcpServers\n  json_entry: {type: http, url: \"{{mcp.url}}\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed) != 1 {
		t.Fatalf("ParseTemplate(my-mcp) = %d templates, want 1", len(parsed))
	}
	current, expected := parsed[0].Pointer(cfgWithMCP())
	if current != "(file missing)" && current != "(no drift probe)" {
		t.Fatalf("current = %q", current)
	}
	if expected == "" {
		t.Fatal("expected pointer must not be empty")
	}
}

// TestMCPRoutedMembersPrune pins the three surface shapes: default writes
// routes + unrouted servers only; include_routed_members restores every
// server; an explicit MCP subset names any server (routed or not) and wins.
func TestMCPRoutedMembersPrune(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	target := filepath.Join(home, ".claude.json")
	tmpl, err := takeover.TemplateByName("claude", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := cfgWithMCP()
	if err := tmpl.Rewrite(cfg, nil, nil); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(target)
	if !strings.Contains(string(data), "/mcp/web-search") || !strings.Contains(string(data), "/mcp/exa") {
		t.Fatalf("route+unrouted missing:\n%s", data)
	}
	if strings.Contains(string(data), "/mcp/zhipu-search") {
		t.Fatalf("routed member written by default:\n%s", data)
	}

	// include_routed_members: every server comes back.
	tmpl2, err := takeover.TemplateByName("claude", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tmpl2.MCP.IncludeRoutedMembers = true
	if err := tmpl2.Rewrite(cfg, nil, nil); err != nil {
		t.Fatal(err)
	}
	data2, _ := os.ReadFile(target)
	for _, name := range []string{"zhipu-search", "exa", "web-search"} {
		if !strings.Contains(string(data2), "/mcp/"+name+"\"") {
			t.Fatalf("full surface missing %s:\n%s", name, data2)
		}
	}

	// Explicit subset: naming the routed member writes it (direct connection
	// is an execution-point choice and outranks the prune).
	tmpl3, err := takeover.TemplateByName("claude", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := tmpl3.RewriteOptsFiltered(cfg, nil, nil, nil, takeover.TakeoverOptions{MCP: []string{"zhipu-search"}}); err != nil {
		t.Fatal(err)
	}
	data3, _ := os.ReadFile(target)
	if !strings.Contains(string(data3), "/mcp/zhipu-search\"") {
		t.Fatalf("named routed member not written:\n%s", data3)
	}
	if strings.Contains(string(data3), "/mcp/web-search") {
		t.Fatalf("unnamed entries written under subset:\n%s", data3)
	}
}

// TestMCPSurfaceNames pins the shared default-surface list the admin API and
// Web chips key on: routes + unrouted servers, sorted.
func TestMCPSurfaceNames(t *testing.T) {
	names := takeover.MCPSurfaceNames(cfgWithMCP())
	if strings.Join(names, ",") != "exa,web-search" {
		t.Fatalf("MCPSurfaceNames = %v", names)
	}
	// A disabled route is not exposed (its endpoint 404s) and owns nothing:
	// zhipu-search falls back to direct exposure so the capability stays
	// reachable.
	cfg := cfgWithMCP()
	off := false
	r := cfg.MCPRoutes["web-search"]
	r.Enabled = &off
	cfg.MCPRoutes["web-search"] = r
	if got := takeover.MCPSurfaceNames(cfg); strings.Join(got, ",") != "exa,zhipu-search" {
		t.Fatalf("disabled route surface = %v", got)
	}
}

// TestMCPJSONExplicitEmptySelectionClears: an explicit empty MCP selection
// (opts.MCP = []) clears proxy-managed entries in the generated namespace
// while preserving the user's own servers.
func TestMCPJSONExplicitEmptySelectionClears(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	target := filepath.Join(home, ".claude.json")
	if err := os.WriteFile(target, []byte(`{"mcpServers":{"keep-me":{"type":"http","url":"https://other.example/mcp"},"exa":{"type":"http","url":"http://127.0.0.1:15721/mcp/exa"},"same-prefix-user":{"type":"http","url":"http://127.0.0.1:15721/mcp/same-prefix-user"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	tmpl, err := takeover.TemplateByName("claude", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := cfgWithMCP()
	if err := tmpl.RewriteOptsFiltered(cfg, nil, nil, nil, takeover.TakeoverOptions{MCP: []string{}}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(target)
	var v map[string]any
	json.Unmarshal(data, &v)
	servers, _ := v["mcpServers"].(map[string]any)
	if _, ok := servers["exa"]; ok {
		t.Fatalf("explicit empty selection left proxy entry exa: %s", data)
	}
	if _, ok := servers["web-search"]; ok {
		t.Fatalf("explicit empty selection left proxy entry web-search: %s", data)
	}
	if _, ok := servers["keep-me"]; !ok {
		t.Fatalf("explicit empty selection dropped user entry keep-me: %s", data)
	}
	if _, ok := servers["same-prefix-user"]; !ok {
		t.Fatalf("explicit empty selection dropped user entry same-prefix-user: %s", data)
	}
}

// TestMCPTOMLExplicitEmptySelectionClears is the TOML-side mirror of
// TestMCPJSONExplicitEmptySelectionClears.
func TestMCPTOMLExplicitEmptySelectionClears(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	target := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("[mcp_servers.\"exa\"]\nurl = \"http://127.0.0.1:15721/mcp/exa\"\n[mcp_servers.\"same-prefix-user\"]\nurl = \"http://127.0.0.1:15721/mcp/same-prefix-user\"\n[mcp_servers.\"keep-me\"]\nurl = \"https://other.example/mcp\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tmpl, err := takeover.TemplateByName("codex", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := cfgWithMCP()
	if err := tmpl.RewriteOptsFiltered(cfg, nil, nil, nil, takeover.TakeoverOptions{MCP: []string{}}); err != nil {
		t.Fatal(err)
	}
	text, _ := os.ReadFile(target)
	if strings.Contains(string(text), `[mcp_servers."exa"]`) {
		t.Fatalf("explicit empty selection left proxy section exa:\n%s", text)
	}
	if strings.Contains(string(text), `[mcp_servers."web-search"]`) {
		t.Fatalf("explicit empty selection left proxy section web-search:\n%s", text)
	}
	if !strings.Contains(string(text), `[mcp_servers."keep-me"]`) {
		t.Fatalf("explicit empty selection dropped user section keep-me:\n%s", text)
	}
	if !strings.Contains(string(text), `[mcp_servers."same-prefix-user"]`) {
		t.Fatalf("explicit empty selection dropped user section same-prefix-user:\n%s", text)
	}
}

// TestMCPTOMLCRLFAndCustomPrefixSection: CRLF line endings must not prevent
// stale section cleanup, and a user-defined section that shares the header
// prefix and proxy URL must survive because its name is not in the generated
// namespace. Surviving user lines keep their ORIGINAL CRLF endings — the old
// unconditional CRLF→LF normalization rewrote the whole file's line endings
// even when nothing was removed.
func TestMCPTOMLCRLFAndCustomPrefixSection(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	target := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	// CRLF file with a stale proxy section, a user custom section sharing the
	// prefix and proxy URL, and an unrelated section.
	crlf := "[mcp_servers.\"exa\"]\r\nurl = \"http://127.0.0.1:15721/mcp/exa\"\r\n[mcp_servers.\"my-custom\"]\r\nurl = \"http://127.0.0.1:15721/mcp/my-custom\"\r\n[mcp_servers.\"keep-me\"]\r\nurl = \"https://other.example/mcp\"\r\n"
	if err := os.WriteFile(target, []byte(crlf), 0o600); err != nil {
		t.Fatal(err)
	}
	tmpl, err := takeover.TemplateByName("codex", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := cfgWithMCP()
	if err := tmpl.Rewrite(cfg, nil, nil); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(target)
	text := string(b)
	// The stale exa section was cleaned despite the CRLF endings and the
	// current surface re-rendered (freshly rendered sections use LF).
	if !strings.Contains(text, "[mcp_servers.\"exa\"]\nurl = \"http://127.0.0.1:15721/mcp/exa\"\n") {
		t.Fatalf("current proxy section exa missing:\n%s", text)
	}
	// User sections survive with their original CRLF line endings intact.
	if !strings.Contains(text, "[mcp_servers.\"my-custom\"]\r\nurl = \"http://127.0.0.1:15721/mcp/my-custom\"\r\n") {
		t.Fatalf("user custom section dropped or its line endings rewritten:\n%q", text)
	}
	if !strings.Contains(text, "[mcp_servers.\"keep-me\"]\r\nurl = \"https://other.example/mcp\"\r\n") {
		t.Fatalf("unrelated user section dropped or its line endings rewritten:\n%q", text)
	}
}

// TestMCPStepcodeTOMLAuxRendering: the stepcode preset writes models into
// ~/.stepcode/models.json (pi shape, same as pi) and the gateway MCP surface
// as [mcp_servers."<name>"] sections into the SEPARATE ~/.stepcode/config.toml
// — Step Code's unified TOML config, the first mcp.file in TOML syntax.
// Root settings and user MCP servers survive; stale proxy sections are
// cleaned; the whole render is idempotent.
func TestMCPStepcodeTOMLAuxRendering(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	main := filepath.Join(home, ".stepcode", "models.json")
	aux := filepath.Join(home, ".stepcode", "config.toml")
	if err := os.MkdirAll(filepath.Dir(main), 0o700); err != nil {
		t.Fatal(err)
	}
	// Seed config.toml like a real Step Code install: root settings, a user
	// MCP server, and a stale proxy entry from an old run (old proxy port).
	if err := os.WriteFile(aux, []byte("theme = \"dark\"\n\n[mcp_servers.keep-me]\nurl = \"https://other.example/mcp\"\n\n[mcp_servers.exa]\nurl = \"http://127.0.0.1:9999/mcp/exa\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tmpl, err := takeover.TemplateByName("stepcode", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := cfgWithMCP()
	routes := map[string][]configdomain.RouteTarget{
		"glm-5.3": {{Provider: "zhipu", Model: "glm-5.3", Priority: 1}},
	}
	if err := tmpl.Rewrite(cfg, nil, routes); err != nil {
		t.Fatal(err)
	}

	// models.json: provider entry + pi-shaped model collection.
	mdata, err := os.ReadFile(main)
	if err != nil {
		t.Fatal(err)
	}
	var mv map[string]any
	if err := json.Unmarshal(mdata, &mv); err != nil {
		t.Fatal(err)
	}
	providers, _ := mv["providers"].(map[string]any)
	prov, ok := providers["model-proxy"].(map[string]any)
	if !ok {
		t.Fatalf("providers.model-proxy missing: %s", mdata)
	}
	if prov["baseUrl"] != "http://127.0.0.1:15721" || prov["api"] != "anthropic-messages" || prov["apiKey"] != "PROXY_MANAGED" {
		t.Fatalf("provider entry = %v", prov)
	}
	models, _ := prov["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("models collection = %v", models)
	}
	if m0, _ := models[0].(map[string]any); m0["id"] != "glm-5.3" {
		t.Fatalf("model entry = %v", m0)
	}

	// config.toml: root settings + user server survive, stale proxy section
	// cleaned, current surface written (route + unrouted server only).
	text, err := os.ReadFile(aux)
	if err != nil {
		t.Fatal(err)
	}
	s := string(text)
	if !strings.Contains(s, `theme = "dark"`) {
		t.Fatalf("root settings dropped:\n%s", s)
	}
	if !strings.Contains(s, "[mcp_servers.keep-me]") {
		t.Fatalf("user mcp server dropped:\n%s", s)
	}
	if strings.Contains(s, "http://127.0.0.1:9999/mcp/exa") {
		t.Fatalf("stale proxy URL not cleaned:\n%s", s)
	}
	for _, name := range []string{"exa", "web-search"} {
		if !strings.Contains(s, `[mcp_servers."`+name+`"]`) {
			t.Fatalf("missing section %s:\n%s", name, s)
		}
		if !strings.Contains(s, `url = "http://127.0.0.1:15721/mcp/`+name+`"`) {
			t.Fatalf("missing URL for %s:\n%s", name, s)
		}
	}
	if strings.Contains(s, `[mcp_servers."zhipu-search"]`) {
		t.Fatalf("routed member rendered separately (prune broken):\n%s", s)
	}

	// Idempotent: rendering converges to a fixed point (the first render may
	// leave a blank line where the stale section was cleaned — same semantic
	// idempotency contract as the codex TOML main-file rendering: no
	// duplicate sections, no further changes once converged).
	if err := tmpl.Rewrite(cfg, nil, routes); err != nil {
		t.Fatal(err)
	}
	text2, _ := os.ReadFile(aux)
	if err := tmpl.Rewrite(cfg, nil, routes); err != nil {
		t.Fatal(err)
	}
	text3, _ := os.ReadFile(aux)
	if string(text3) != string(text2) {
		t.Fatalf("render did not converge:\n%s\n---\n%s", text2, text3)
	}
	for _, header := range []string{`[mcp_servers."exa"]`, `[mcp_servers."web-search"]`, "[mcp_servers.keep-me]"} {
		if strings.Count(string(text3), header) != 1 {
			t.Fatalf("duplicate or missing section %s after re-render:\n%s", header, text3)
		}
	}
	mdata2, _ := os.ReadFile(main)
	if string(mdata2) != string(mdata) {
		t.Fatal("second render changed models.json")
	}

	// The openai variant shares the same TOML aux file.
	openai, err := takeover.TemplateByName("stepcode-openai", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := openai.Rewrite(cfg, nil, routes); err != nil {
		t.Fatal(err)
	}
	text4, _ := os.ReadFile(aux)
	if string(text4) != string(text3) {
		t.Fatalf("variant rewrite changed the shared aux file:\n%s", text4)
	}
}

// TestMCPTOMLAuxEmptySurfaceSkips: with no gateway MCP surface the TOML aux
// file is left byte-identical (user sections — even proxy-looking ones — are
// preserved because the template has no namespace to clean), and a missing
// aux file is NOT created.
func TestMCPTOMLAuxEmptySurfaceSkips(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	aux := filepath.Join(home, ".stepcode", "config.toml")
	if err := os.MkdirAll(filepath.Dir(aux), 0o700); err != nil {
		t.Fatal(err)
	}
	original := "theme = \"dark\"\n\n[mcp_servers.exa]\nurl = \"http://127.0.0.1:15721/mcp/exa\"\n"
	if err := os.WriteFile(aux, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	tmpl, err := takeover.TemplateByName("stepcode", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := tmpl.Rewrite(cfgWith(nil, nil), nil, nil); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(aux)
	if string(data) != original {
		t.Fatalf("empty gateway surface rewrote config.toml:\n%s", data)
	}

	// Missing aux + empty surface: still no file afterwards.
	if err := os.Remove(aux); err != nil {
		t.Fatal(err)
	}
	if err := tmpl.Rewrite(cfgWith(nil, nil), nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(aux); !os.IsNotExist(err) {
		t.Fatalf("empty surface created an aux file: %v", err)
	}
}

// TestMCPTOMLAuxScopes: ScopeMCP writes only the TOML aux (models.json not
// created); ScopeModel writes only models.json (config.toml untouched).
func TestMCPTOMLAuxScopes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	main := filepath.Join(home, ".stepcode", "models.json")
	aux := filepath.Join(home, ".stepcode", "config.toml")
	if err := os.MkdirAll(filepath.Dir(main), 0o700); err != nil {
		t.Fatal(err)
	}
	tmpl, err := takeover.TemplateByName("stepcode", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := cfgWithMCP()
	if err := tmpl.RewriteScoped(cfg, nil, nil, takeover.ScopeMCP); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(main); !os.IsNotExist(err) {
		t.Fatalf("ScopeMCP created the main models.json: %v", err)
	}
	text, err := os.ReadFile(aux)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(text), `[mcp_servers."web-search"]`) {
		t.Fatalf("ScopeMCP did not write the aux surface:\n%s", text)
	}

	if err := os.Remove(aux); err != nil {
		t.Fatal(err)
	}
	if err := tmpl.RewriteScoped(cfg, nil, nil, takeover.ScopeModel); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(main); err != nil {
		t.Fatalf("ScopeModel did not write models.json: %v", err)
	}
	if _, err := os.Stat(aux); !os.IsNotExist(err) {
		t.Fatalf("ScopeModel created the aux config.toml: %v", err)
	}
}

// TestMCPTOMLAuxExplicitEmptySelectionClears: an explicit empty MCP selection
// clears proxy-managed sections in the generated namespace while preserving
// the user's own servers and root settings.
func TestMCPTOMLAuxExplicitEmptySelectionClears(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	aux := filepath.Join(home, ".stepcode", "config.toml")
	if err := os.MkdirAll(filepath.Dir(aux), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(aux, []byte("theme = \"dark\"\n[mcp_servers.\"exa\"]\nurl = \"http://127.0.0.1:15721/mcp/exa\"\n[mcp_servers.\"same-prefix-user\"]\nurl = \"http://127.0.0.1:15721/mcp/same-prefix-user\"\n[mcp_servers.\"keep-me\"]\nurl = \"https://other.example/mcp\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tmpl, err := takeover.TemplateByName("stepcode", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := cfgWithMCP()
	if err := tmpl.RewriteOptsFiltered(cfg, nil, nil, nil, takeover.TakeoverOptions{MCP: []string{}}); err != nil {
		t.Fatal(err)
	}
	text, _ := os.ReadFile(aux)
	s := string(text)
	if strings.Contains(s, `[mcp_servers."exa"]`) {
		t.Fatalf("explicit empty selection left proxy section exa:\n%s", s)
	}
	if strings.Contains(s, `[mcp_servers."web-search"]`) {
		t.Fatalf("explicit empty selection left proxy section web-search:\n%s", s)
	}
	for _, keep := range []string{`theme = "dark"`, `[mcp_servers."keep-me"]`, `[mcp_servers."same-prefix-user"]`} {
		if !strings.Contains(s, keep) {
			t.Fatalf("explicit empty selection dropped %q:\n%s", keep, s)
		}
	}
}

// TestMCPTOMLAuxValidation: mcp.file + format toml requires toml_section +
// toml_body; mcp.format without mcp.file is rejected (the main file's format
// decides); unknown mcp.format values are rejected.
func TestMCPTOMLAuxValidation(t *testing.T) {
	if _, err := takeover.ParseTemplate("bad", "test", []byte(`
file: /tmp/x.json
format: json
json:
  set: {a: b}
mcp:
  file: /tmp/x.toml
  format: toml
  toml_section: 'mcp_servers."{{mcp.name}}"'
`)); err == nil || !strings.Contains(err.Error(), "toml_body") {
		t.Fatalf("toml aux without toml_body must fail: %v", err)
	}
	if _, err := takeover.ParseTemplate("bad2", "test", []byte(`
file: /tmp/x.json
format: json
json:
  set: {a: b}
mcp:
  format: toml
  toml_section: 'mcp_servers."{{mcp.name}}"'
  toml_body: 'url = "{{mcp.url}}"'
`)); err == nil || !strings.Contains(err.Error(), "mcp.file") {
		t.Fatalf("mcp.format without mcp.file must fail: %v", err)
	}
	if _, err := takeover.ParseTemplate("bad3", "test", []byte(`
file: /tmp/x.json
format: json
json:
  set: {a: b}
mcp:
  file: /tmp/x.txt
  format: ini
`)); err == nil || !strings.Contains(err.Error(), "mcp.format") {
		t.Fatalf("unknown mcp.format must fail: %v", err)
	}
}
