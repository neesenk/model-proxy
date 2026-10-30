package takeover_test

// mcp_auth_test.go — the MCP half of the S2 boundary: gateway /mcp/ endpoints
// sit behind the SAME forward-surface api-keys gate as /v1/* (proxy_http.go
// branches /mcp/ before protocol.ForPath), so a takeover that rewrites a
// client's MCP entries to point at the gateway must embed the credential too
// — otherwise every MCP call 401s and key-capable clients nag for a login
// they can never complete. Presets whose client schema supports HTTP headers
// (pi/claude/kimi/zcode/opencode JSON `headers`; codex/stepcode TOML
// `http_headers`) embed Authorization: Bearer {{token}} — the real first key
// when the daemon authenticates, the PROXY_MANAGED sentinel on unkeyed
// deployments — and the preview masks the real key back to the sentinel.
// workbuddy/hermes stay keyless (no header-capable schema verified); that gap
// is documented in docs/client-takeover.md.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"model-proxy/internal/takeover"
)

// mcpAuthCases lists the header-capable JSON presets: template name, the
// client MCP file (below HOME), and the two json_path keys leading to the
// per-server map.
var mcpAuthCases = []struct {
	template string
	target   string
	path     [2]string
}{
	{"claude", ".claude.json", [2]string{"mcpServers", ""}},
	{"pi", ".pi/agent/mcp.json", [2]string{"mcpServers", ""}},
	{"kimi", ".kimi-code/mcp.json", [2]string{"mcpServers", ""}},
	{"zcode", ".zcode/cli/config.json", [2]string{"mcp", "servers"}},
	{"opencode", ".config/opencode/opencode.json", [2]string{"mcp", ""}},
}

// jsonEntryAuthorization digs entry.headers.Authorization out of a rendered
// JSON MCP document.
func jsonEntryAuthorization(t *testing.T, data []byte, path [2]string, server string) any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	servers, ok := v[path[0]].(map[string]any)
	if !ok {
		t.Fatalf("%s missing in rendered config: %s", path[0], data)
	}
	if path[1] != "" {
		servers, ok = servers[path[1]].(map[string]any)
		if !ok {
			t.Fatalf("%s.%s missing in rendered config: %s", path[0], path[1], data)
		}
	}
	entry, ok := servers[server].(map[string]any)
	if !ok {
		t.Fatalf("entry %q missing: %s", server, data)
	}
	headers, _ := entry["headers"].(map[string]any)
	return headers["Authorization"]
}

// TestMCPJSONEntriesEmbedForwardKey: with api_keys_file configured every
// header-capable JSON preset writes Authorization: Bearer <real first key>
// into each gateway MCP entry; unkeyed deployments keep the sentinel.
func TestMCPJSONEntriesEmbedForwardKey(t *testing.T) {
	for _, tc := range mcpAuthCases {
		t.Run(tc.template, func(t *testing.T) {
			render := func(keysFile string) []byte {
				home := t.TempDir()
				t.Setenv("HOME", home)
				target := filepath.Join(home, tc.target)
				if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
					t.Fatal(err)
				}
				tmpl, err := takeover.TemplateByName(tc.template, t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				cfg := cfgWithMCP()
				cfg.Web.Auth.APIKeysFile = keysFile
				if err := tmpl.Rewrite(cfg, nil, nil); err != nil {
					t.Fatal(err)
				}
				b, err := os.ReadFile(target)
				if err != nil {
					t.Fatal(err)
				}
				return b
			}

			// web-search is the route entry every preset renders (the unrouted
			// exa also qualifies; one is enough to pin the credential).
			if got := jsonEntryAuthorization(t, render(writeForwardKeys(t)), tc.path, "web-search"); got != "Bearer mp-forward-key-1" {
				t.Errorf("entry Authorization = %v, want the real first forward key", got)
			}
			if got := jsonEntryAuthorization(t, render(""), tc.path, "web-search"); got != "Bearer PROXY_MANAGED" {
				t.Errorf("unkeyed entry Authorization = %v, want the sentinel", got)
			}
		})
	}
}

// TestMCPTOMLEntriesEmbedForwardKey: codex and stepcode write
// http_headers = { Authorization = "Bearer …" } into every [mcp_servers]
// section — codex needs its main config.toml seeded like TestMCPTOMLRendering.
func TestMCPTOMLEntriesEmbedForwardKey(t *testing.T) {
	cases := []struct{ template, target string }{
		{"codex", ".codex/config.toml"},
		{"stepcode", ".stepcode/config.toml"},
	}
	for _, tc := range cases {
		t.Run(tc.template, func(t *testing.T) {
			render := func(keysFile string) string {
				home := t.TempDir()
				t.Setenv("HOME", home)
				target := filepath.Join(home, tc.target)
				if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, []byte("# client config\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				tmpl, err := takeover.TemplateByName(tc.template, t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				cfg := cfgWithMCP()
				cfg.Web.Auth.APIKeysFile = keysFile
				if err := tmpl.Rewrite(cfg, nil, nil); err != nil {
					t.Fatal(err)
				}
				b, err := os.ReadFile(target)
				if err != nil {
					t.Fatal(err)
				}
				return string(b)
			}

			withKeys := render(writeForwardKeys(t))
			if !strings.Contains(withKeys, `http_headers = { Authorization = "Bearer mp-forward-key-1" }`) {
				t.Errorf("TOML section must carry the real first forward key:\n%s", withKeys)
			}
			without := render("")
			if !strings.Contains(without, `http_headers = { Authorization = "Bearer PROXY_MANAGED" }`) {
				t.Errorf("unkeyed TOML section must carry the sentinel:\n%s", without)
			}
		})
	}
}

// TestPreviewMasksMCPHeaderKey: the preview's MCP aux write displays the
// sentinel where the run embeds the real key — same masking contract as the
// provider blocks (TestPreviewWritesMasksForwardKey).
func TestPreviewMasksMCPHeaderKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	mcpJSON := filepath.Join(home, ".pi", "agent", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(mcpJSON), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mcpJSON, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := cfgWithMCP()
	cfg.Web.Auth.APIKeysFile = writeForwardKeys(t)
	facts := takeover.ModelFactsFor(cfg, "pi", home, "", takeover.ModeUnified, nil)
	writes, err := takeover.PreviewWrites(cfg, "pi", "", takeover.ModeUnified, facts, false, takeover.ScopeAll)
	if err != nil {
		t.Fatal(err)
	}
	var aux *takeover.PreviewWrite
	for i := range writes {
		if writes[i].File == mcpJSON {
			aux = &writes[i]
		}
	}
	if aux == nil {
		t.Fatalf("preview must cover the pi aux mcp.json: %+v", writes)
	}
	for _, key := range []string{"mp-forward-key-1", "mp-forward-key-2"} {
		if strings.Contains(aux.Content, key) {
			t.Errorf("preview leaks forward key in the MCP entry:\n%s", aux.Content)
		}
	}
	if !strings.Contains(aux.Content, `"Authorization": "Bearer PROXY_MANAGED"`) {
		t.Errorf("preview must show the masked sentinel in the MCP entry:\n%s", aux.Content)
	}
}
