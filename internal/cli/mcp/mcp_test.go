package climcp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"model-proxy/internal/cli/clitest"
)

// TestHelperProcess is the subprocess entry for os.Exit-ing command paths,
// and doubles as the fake MCP stdio child when MCP_STDIO_CHILD=1.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("MCP_STDIO_CHILD") == "1" {
		os.Exit(runStdioChild())
	}
	clitest.HelperProcess(t, map[string]func([]string){"mcp": RunMCP})
}

// runStdioChild serves newline-delimited JSON-RPC on stdin/stdout for the
// stdio `mcp test` probe, echoing its CHILD_KEY env in the tool result.
func runStdioChild() int {
	scanner := bufio.NewScanner(os.Stdin)
	writer := bufio.NewWriter(os.Stdout)
	for scanner.Scan() {
		var v struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &v); err != nil {
			continue
		}
		switch v.Method {
		case "initialize":
			fmt.Fprintf(writer, `{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-03-26","capabilities":{"tools":{}},"serverInfo":{"name":"fake-stdio","version":"3.1"}}}`+"\n", v.ID)
		case "tools/list":
			fmt.Fprintf(writer, `{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"image_analysis","inputSchema":{"type":"object"}}]}}`+"\n", v.ID)
		}
		writer.Flush()
	}
	return 0
}

func mcpListConfig(zsURL, exaURL string) string {
	return fmt.Sprintf(`
listen: 127.0.0.1:1
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://example.com/api/v1}
mcp:
  zs:
    provider: zhipu
    url: %s
  exa:
    url: %s
    auth: none
`, zsURL, exaURL)
}

func TestMCPList(t *testing.T) {
	cfgPath := clitest.WriteTempConfig(t, mcpListConfig("https://open.bigmodel.cn/api/mcp/web_search_prime/mcp", "https://mcp.exa.ai/mcp"))
	out := clitest.GrabStdout(t, func() {
		RunMCP([]string{"list", "--config", cfgPath})
	})
	for _, want := range []string{"zs", "exa", "provider", "zhipu", "none", "web_search_prime"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
}

func TestMCPListEmpty(t *testing.T) {
	cfgPath := clitest.WriteTempConfig(t, `
listen: 127.0.0.1:1
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://example.com/api/v1}
`)
	out := clitest.GrabStdout(t, func() {
		RunMCP([]string{"list", "--config", cfgPath})
	})
	if !strings.Contains(out, "no mcp servers configured") {
		t.Fatalf("stdout=%q", out)
	}
}

// fakeHandshakeServer answers the MCP handshake for `mcp test`, enforcing the
// expected credential.
func fakeHandshakeServer(wantAuth string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if wantAuth != "" && r.Header.Get("Authorization") != wantAuth {
			http.Error(w, "bad key", http.StatusUnauthorized)
			return
		}
		body := make([]byte, 0, 4096)
		chunk := make([]byte, 4096)
		for {
			n, err := r.Body.Read(chunk)
			body = append(body, chunk[:n]...)
			if err != nil {
				break
			}
		}
		switch s := string(body); {
		case strings.Contains(s, `"initialize"`):
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26","capabilities":{},"serverInfo":{"name":"fake-mcp","version":"9.9"}}}`))
		case strings.Contains(s, "notifications/initialized"):
			w.WriteHeader(http.StatusAccepted)
		case strings.Contains(s, "tools/list"):
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"search"},{"name":"read"}]}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestMCPTestProviderBacked(t *testing.T) {
	up := fakeHandshakeServer("Bearer pool-key-1")
	defer up.Close()
	clitest.SetPoolHome(t, t.TempDir())
	clitest.WritePoolFile(t, "zhipu", "zhipu", "pool-key-1")
	cfgPath := clitest.WriteTempConfig(t, mcpListConfig(up.URL, "https://mcp.exa.ai/mcp"))

	out := clitest.GrabStdout(t, func() {
		RunMCP([]string{"test", "zs", "--config", cfgPath})
	})
	for _, want := range []string{"fake-mcp", "9.9", "2 tools", "search", "read", "via zhipu"} {
		if !strings.Contains(out, want) {
			t.Errorf("test output missing %q:\n%s", want, out)
		}
	}
}

func TestMCPTestAuthNone(t *testing.T) {
	up := fakeHandshakeServer("") // no credential expected
	defer up.Close()
	clitest.SetPoolHome(t, t.TempDir())
	cfgPath := clitest.WriteTempConfig(t, mcpListConfig("https://x.invalid/mcp", up.URL))

	out := clitest.GrabStdout(t, func() {
		RunMCP([]string{"test", "exa", "--config", cfgPath})
	})
	if !strings.Contains(out, "fake-mcp") {
		t.Fatalf("auth:none test failed:\n%s", out)
	}
	if strings.Contains(out, "via ") {
		t.Errorf("auth:none must not name an account:\n%s", out)
	}
}

func TestMCPTestUnknownServer(t *testing.T) {
	cfgPath := clitest.WriteTempConfig(t, mcpListConfig("https://x.invalid/mcp", "https://x.invalid/mcp"))
	_, stderr, code := clitest.RunCLI(t, "mcp", cfgPath, "test", "ghost")
	if code == 0 || !strings.Contains(stderr, `no mcp server "ghost"`) {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
}

func TestMCPUnknownSubcommand(t *testing.T) {
	cfgPath := clitest.WriteTempConfig(t, mcpListConfig("https://x.invalid/mcp", "https://x.invalid/mcp"))
	_, stderr, code := clitest.RunCLI(t, "mcp", cfgPath, "bogus")
	if code == 0 || !strings.Contains(stderr, `unknown mcp subcommand`) {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
}

// customHeaderConfig adds a volcengine-style custom-auth-header server.
func customHeaderConfig(upURL string) string {
	return fmt.Sprintf(`
listen: 127.0.0.1:1
providers:
  volcengine: {provider_id: volcengine, openai_base_url: https://example.com/api/v3}
mcp:
  dp:
    provider: volcengine
    url: %s
    auth_header: X-Agent-Plan-Key
`, upURL)
}

// fakePlanKeyServer expects the raw key under X-Agent-Plan-Key (no Authorization).
func fakePlanKeyServer(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Agent-Plan-Key") != "plan-key-9" || r.Header.Get("Authorization") != "" {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		body := make([]byte, 0, 4096)
		chunk := make([]byte, 4096)
		for {
			n, err := r.Body.Read(chunk)
			body = append(body, chunk[:n]...)
			if err != nil {
				break
			}
		}
		if strings.Contains(string(body), `"initialize"`) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-03-26","capabilities":{},"serverInfo":{"name":"datapro","version":"1.0"}}}`))
			return
		}
		if strings.Contains(string(body), "tools/list") {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"dataPro_search"}]}}`))
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}))
}

func TestMCPTestCustomAuthHeader(t *testing.T) {
	up := fakePlanKeyServer(t)
	defer up.Close()
	clitest.SetPoolHome(t, t.TempDir())
	clitest.WritePoolFile(t, "volcengine", "volcengine", "plan-key-9")
	cfgPath := clitest.WriteTempConfig(t, customHeaderConfig(up.URL))

	out := clitest.GrabStdout(t, func() {
		RunMCP([]string{"test", "dp", "--config", cfgPath})
	})
	for _, want := range []string{"datapro", "1 tools", "dataPro_search", "via volcengine"} {
		if !strings.Contains(out, want) {
			t.Errorf("custom-header output missing %q:\n%s", want, out)
		}
	}
}

func TestMCPTestDisabledServer(t *testing.T) {
	cfgPath := clitest.WriteTempConfig(t, `
listen: 127.0.0.1:1
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://example.com/api/v1}
mcp:
  off: {provider: zhipu, url: https://x.invalid/mcp, enabled: false}
`)
	_, stderr, code := clitest.RunCLI(t, "mcp", cfgPath, "test", "off")
	if code == 0 || !strings.Contains(stderr, "disabled") {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
}

func TestMCPTestNoAccount(t *testing.T) {
	cfgPath := clitest.WriteTempConfig(t, mcpListConfig("https://x.invalid/mcp", "https://x.invalid/mcp"))
	// Fresh empty HOME (RunCLI pins one): the provider exists but has no
	// credential, so AuthHeaders fails closed at request time.
	_, stderr, code := clitest.RunCLI(t, "mcp", cfgPath, "test", "zs")
	if code == 0 || !strings.Contains(stderr, "not logged in") {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
}

func TestMCPTestUpstreamError(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad key", http.StatusUnauthorized)
	}))
	defer up.Close()
	home := t.TempDir()
	clitest.SetPoolHome(t, home)
	clitest.WritePoolFile(t, "zhipu", "zhipu", "pool-key-1")
	cfgPath := clitest.WriteTempConfig(t, mcpListConfig(up.URL, up.URL))
	_, stderr, code := clitest.RunCLIWithHome(t, home, "mcp", cfgPath, "test", "zs")
	if code == 0 || !strings.Contains(stderr, "401") {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
}

func TestMCPTestMissingUsageArg(t *testing.T) {
	cfgPath := clitest.WriteTempConfig(t, mcpListConfig("https://x.invalid/mcp", "https://x.invalid/mcp"))
	_, stderr, code := clitest.RunCLI(t, "mcp", cfgPath, "test")
	if code == 0 || !strings.Contains(stderr, "usage: model-proxy mcp test") {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
}

func TestMCPListShowsRoutes(t *testing.T) {
	cfgPath := clitest.WriteTempConfig(t, `
listen: 127.0.0.1:1
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://example.com/api/v1}
mcp:
  zs: {provider: zhipu, url: https://example.com/mcp}
  exa: {url: https://mcp.exa.ai/mcp, auth: none}
mcp_routes:
  web-search:
    targets:
      - {mcp: zs, tools: {web_search: web_search_prime}}
      - {mcp: exa, tools: {web_search: web_search_exa}}
`)
	out := clitest.GrabStdout(t, func() {
		RunMCP([]string{"list", "--config", cfgPath})
	})
	for _, want := range []string{"ROUTE", "web-search", "zs (1 tools)", "exa (1 tools)", "→"} {
		if !strings.Contains(out, want) {
			t.Errorf("route list output missing %q:\n%s", want, out)
		}
	}
}

func TestMCPTestRejectsRoute(t *testing.T) {
	cfgPath := clitest.WriteTempConfig(t, `
listen: 127.0.0.1:1
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://example.com/api/v1}
mcp:
  zs: {provider: zhipu, url: https://example.com/mcp}
mcp_routes:
  web-search:
    targets:
      - {mcp: zs, tools: {web_search: web_search_prime}}
`)
	_, stderr, code := clitest.RunCLI(t, "mcp", cfgPath, "test", "web-search")
	if code == 0 || !strings.Contains(stderr, "aggregated route") || !strings.Contains(stderr, "zs") {
		t.Fatalf("exit=%d stderr=%q", code, stderr)
	}
}

func TestMCPTestStdio(t *testing.T) {
	clitest.SetPoolHome(t, t.TempDir())
	clitest.WritePoolFile(t, "zhipu", "zhipu", "pool-key-1")
	cfgPath := clitest.WriteTempConfig(t, `
listen: 127.0.0.1:1
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://example.com/api/v1}
mcp:
  vision:
    transport: stdio
    provider: zhipu
    command: ["`+os.Args[0]+`", "-test.run=TestHelperProcess"]
    env:
      MCP_STDIO_CHILD: "1"
      CHILD_KEY: ${account.api_key}
`)
	out := clitest.GrabStdout(t, func() {
		RunMCP([]string{"test", "vision", "--config", cfgPath})
	})
	for _, want := range []string{"fake-stdio", "3.1", "stdio", "1 tools", "image_analysis", "via zhipu"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdio test output missing %q:\n%s", want, out)
		}
	}
}
