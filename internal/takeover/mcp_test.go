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
	if err := os.WriteFile(target, []byte(`{"theme":"dark","mcpServers":{"keep-me":{"type":"http","url":"https://other.example/mcp"},"stale-gone":{"type":"http","url":"http://127.0.0.1:15721/mcp/stale-gone"}}}`), 0o600); err != nil {
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
	if _, ok := servers["stale-gone"]; ok {
		t.Fatalf("stale proxy entry from a previous takeover not cleaned: %s", data)
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

// TestMCPTOMLRendering: the codex preset appends one [mcp_servers."<name>"]
// section per gateway entry, idempotently.
func TestMCPTOMLRendering(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	target := filepath.Join(home, ".codex", "config.toml")
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("# codex config\n[mcp_servers.\"stale-gone\"]\nurl = \"http://127.0.0.1:15721/mcp/stale-gone\"\n[mcp_servers.\"keep-me\"]\nurl = \"https://other.example/mcp\"\n"), 0o600); err != nil {
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
	if strings.Contains(string(text), "stale-gone") {
		t.Fatalf("stale proxy section not cleaned:\n%s", text)
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

// TestMCPEmptySurfaceSkips: with no mcp: config the client's existing MCP
// section is left untouched.
func TestMCPEmptySurfaceSkips(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	target := filepath.Join(home, ".claude.json")
	original := `{"mcpServers":{"keep-me":{"type":"http","url":"https://other.example/mcp"}}}`
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
	if len(servers) != 1 {
		t.Fatalf("empty gateway surface rewrote mcpServers: %s", data)
	}
	if _, ok := servers["keep-me"]; !ok {
		t.Fatalf("existing entry dropped: %s", data)
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
