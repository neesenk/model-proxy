package config

import (
	"strings"
	"testing"
	"time"
)

// mcpBaseConfig returns a minimal valid Config hosting one provider, so mcp:
// validation failures are attributable to the mcp section itself.
func mcpBaseConfig() *Config {
	return &Config{
		Listen: "127.0.0.1:15722",
		Providers: map[string]Provider{
			"zhipu":      {Provider: "zhipu", OpenAIBaseURL: "https://open.bigmodel.cn/api/paas/v4"},
			"codex":      {Provider: "codex", OpenAIBaseURL: "https://chatgpt.com/backend-api/codex"},
			"volcengine": {Provider: "volcengine", OpenAIBaseURL: "https://ark.cn-beijing.volces.com/api/plan/v3"},
		},
	}
}

func validMCPServer() MCPServer {
	return MCPServer{Provider: "zhipu", URL: "https://open.bigmodel.cn/api/mcp/web_search_prime/mcp"}
}

func TestMCPValidate_Valid(t *testing.T) {
	cfg := mcpBaseConfig()
	cfg.MCP = map[string]MCPServer{
		"zhipu-search": validMCPServer(),
		"exa":          {URL: "https://mcp.exa.ai/mcp", Auth: "none"},
		"exa-implicit": {URL: "https://mcp.exa.ai/mcp"}, // auth defaults to none
		"ark-datapro":  {Provider: "volcengine", URL: "https://datapro.hqd.cn-beijing.volces.com/mcp", AuthHeader: "X-Agent-Plan-Key"},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("valid mcp config rejected: %v", err)
	}
}

func TestMCPValidate_Errors(t *testing.T) {
	cases := []struct {
		name   string
		key    string
		server MCPServer
		want   string
	}{
		{"bad name", "has space", validMCPServer(), "invalid name"},
		{"slash in name", "a/b", validMCPServer(), "invalid name"},
		{"empty url", "s", MCPServer{Provider: "zhipu"}, "url is empty"},
		{"bad url scheme", "s", MCPServer{Provider: "zhipu", URL: "ftp://x"}, "not a valid http(s) URL"},
		{"bad auth value", "s", MCPServer{Provider: "zhipu", URL: "https://x", Auth: "oauth"}, "auth \"oauth\" invalid"},
		{"provider without entry", "s", MCPServer{Provider: "ghost", URL: "https://x"}, "not defined under providers:"},
		{"provider plus auth none", "s", MCPServer{Provider: "zhipu", URL: "https://x", Auth: "none"}, "drop one of them"},
		{"auth provider without provider", "s", MCPServer{URL: "https://x", Auth: "provider"}, "requires provider:"},
		{"custom header on oauth provider", "s", MCPServer{Provider: "codex", URL: "https://x", AuthHeader: "X-Key"}, "OAuth provider"},
		{"custom header with auth none", "s", MCPServer{URL: "https://x", AuthHeader: "X-Key"}, "meaningless with auth: none"},
		{"invalid header name", "s", MCPServer{Provider: "zhipu", URL: "https://x", AuthHeader: "Bad Header"}, "not a valid HTTP header name"},
		{"bad timeout", "s", MCPServer{Provider: "zhipu", URL: "https://x", Timeout: "later"}, "not a valid duration"},
		{"bad proxy", "s", MCPServer{Provider: "zhipu", URL: "https://x", ProxyURL: "nonsense://"}, "proxy_url"},
		{"command on http transport", "s", MCPServer{Provider: "zhipu", URL: "https://x", Command: []string{"npx", "-y", "@z_ai/mcp-server"}}, "command is only valid with transport stdio"},
		{"env on http transport", "s", MCPServer{Provider: "zhipu", URL: "https://x", Env: map[string]string{"Z_AI_MODE": "ZHIPU"}}, "env is only valid with transport stdio"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := mcpBaseConfig()
			cfg.MCP = map[string]MCPServer{tc.key: tc.server}
			err := cfg.validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestMCPValidate_CustomHeaderOnApikeyProviders(t *testing.T) {
	cfg := mcpBaseConfig()
	cfg.MCP = map[string]MCPServer{
		"dp": {Provider: "volcengine", URL: "https://x", AuthHeader: "X-Agent-Plan-Key"},
		"zp": {Provider: "zhipu", URL: "https://x", AuthHeader: "Authorization"}, // explicit default form
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("apikey providers with custom auth header rejected: %v", err)
	}
}

// TestMCPLoadFromYAML guards the rawConfig silent-drop trap (precedent:
// TestLoadCacheAndShadow): the mcp: section must survive LoadConfigFromBytes,
// not just direct Config construction.
func TestMCPLoadFromYAML(t *testing.T) {
	cfg, err := LoadConfigFromBytes("x", []byte(`
listen: 127.0.0.1:1
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://example.com/api/v1}
mcp:
  zs:
    provider: zhipu
    url: https://open.bigmodel.cn/api/mcp/web_search_prime/mcp
    timeout: 120s
  exa:
    url: https://mcp.exa.ai/mcp
    auth: none
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.MCP) != 2 {
		t.Fatalf("mcp did not load from yaml: %+v", cfg.MCP)
	}
	zs := cfg.MCP["zs"]
	if zs.Provider != "zhipu" || zs.URL == "" || zs.Timeout != "120s" {
		t.Errorf("zs = %+v", zs)
	}
	if cfg.MCP["exa"].MCPAuthMode() != "none" {
		t.Errorf("exa auth mode = %q", cfg.MCP["exa"].MCPAuthMode())
	}
}

func TestMCPEffectiveValues(t *testing.T) {
	s := MCPServer{}
	if !s.MCPEffectiveEnabled() {
		t.Error("default enabled = false")
	}
	off := false
	s.Enabled = &off
	if s.MCPEffectiveEnabled() {
		t.Error("explicit enabled: false not honored")
	}
	if d := (MCPServer{}).MCPTimeoutDuration(); d != 300*time.Second {
		t.Errorf("default timeout = %v", d)
	}
	if d := (MCPServer{Timeout: "90s"}).MCPTimeoutDuration(); d != 90*time.Second {
		t.Errorf("timeout = %v", d)
	}
	if d := (MCPServer{Timeout: "bogus"}).MCPTimeoutDuration(); d != 300*time.Second {
		t.Errorf("malformed timeout fallback = %v", d)
	}
	// Auth mode resolution.
	if m := (MCPServer{Provider: "zhipu"}).MCPAuthMode(); m != "provider" {
		t.Errorf("mode with provider = %q", m)
	}
	if m := (MCPServer{}).MCPAuthMode(); m != "none" {
		t.Errorf("mode without provider = %q", m)
	}
	if m := (MCPServer{Auth: "none", Provider: "zhipu"}).MCPAuthMode(); m != "none" {
		t.Errorf("explicit none = %q", m)
	}
}

func TestMCPRoutesValidate_Valid(t *testing.T) {
	cfg := mcpBaseConfig()
	cfg.MCP = map[string]MCPServer{
		"zs":  validMCPServer(),
		"exa": {URL: "https://mcp.exa.ai/mcp", Auth: "none"},
	}
	cfg.MCPRoutes = map[string]MCPRoute{
		"web-search": {Targets: []MCPRouteTarget{
			{Server: "zs", Tools: map[string]string{"web_search": "web_search_prime"}},
			{Server: "exa", Tools: map[string]string{"web_search": "web_search_exa"}},
		}},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("valid mcp_routes rejected: %v", err)
	}
}

func TestMCPRoutesValidate_Errors(t *testing.T) {
	base := func() *Config {
		cfg := mcpBaseConfig()
		cfg.MCP = map[string]MCPServer{"zs": validMCPServer()}
		return cfg
	}
	cases := []struct {
		name  string
		key   string
		route MCPRoute
		want  string
	}{
		{"name collides with server", "zs", MCPRoute{Targets: []MCPRouteTarget{{Server: "zs", Tools: map[string]string{"a": "b"}}}}, "collides"},
		{"no targets", "r", MCPRoute{}, "no targets"},
		{"empty server ref", "r", MCPRoute{Targets: []MCPRouteTarget{{Tools: map[string]string{"a": "b"}}}}, "mcp is empty"},
		{"dangling server ref", "r", MCPRoute{Targets: []MCPRouteTarget{{Server: "ghost", Tools: map[string]string{"a": "b"}}}}, "not defined under mcp:"},
		{"empty tools", "r", MCPRoute{Targets: []MCPRouteTarget{{Server: "zs"}}}, "tools is empty"},
		{"bad canonical name", "r", MCPRoute{Targets: []MCPRouteTarget{{Server: "zs", Tools: map[string]string{"bad name": "b"}}}}, "canonical tool"},
		{"empty backend name", "r", MCPRoute{Targets: []MCPRouteTarget{{Server: "zs", Tools: map[string]string{"a": ""}}}}, "backend tool"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			cfg.MCPRoutes = map[string]MCPRoute{tc.key: tc.route}
			err := cfg.validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want substring %q", err, tc.want)
			}
		})
	}
}

// TestMCPRoutesLoadFromYAML extends the silent-drop guard to mcp_routes:.
func TestMCPRoutesLoadFromYAML(t *testing.T) {
	cfg, err := LoadConfigFromBytes("x", []byte(`
listen: 127.0.0.1:1
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://example.com/api/v1}
mcp:
  zs: {provider: zhipu, url: https://example.com/mcp}
mcp_routes:
  web-search:
    targets:
      - {mcp: zs, tools: {web_search: web_search_prime}}
`))
	if err != nil {
		t.Fatal(err)
	}
	r, ok := cfg.MCPRoutes["web-search"]
	if !ok || len(r.Targets) != 1 || r.Targets[0].Server != "zs" || r.Targets[0].Tools["web_search"] != "web_search_prime" {
		t.Fatalf("mcp_routes did not load: %+v", cfg.MCPRoutes)
	}
}

func TestMCPHeadersValidation(t *testing.T) {
	cfg := mcpBaseConfig()
	cfg.MCP = map[string]MCPServer{
		"tavily": {URL: "https://mcp.tavily.com/mcp/", Auth: "none", Headers: map[string]string{"Authorization": "env:TAVILY_API_KEY"}},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("env: header rejected: %v", err)
	}
	cfg.MCP["tavily"] = MCPServer{URL: "https://x", Auth: "none", Headers: map[string]string{"Authorization": "literal-key"}}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "env:VAR") {
		t.Fatalf("literal header must be rejected: %v", err)
	}
	cfg.MCP["tavily"] = MCPServer{URL: "https://x", Auth: "none", Headers: map[string]string{"Bad Header": "env:X"}}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "not a valid HTTP header name") {
		t.Fatalf("bad header name: %v", err)
	}
}

func TestResolveMCPHeaders(t *testing.T) {
	t.Setenv("TAVILY_API_KEY", "tvly-123")
	out, err := ResolveMCPHeaders(MCPServer{Headers: map[string]string{"Authorization": "env:TAVILY_API_KEY"}})
	if err != nil || out["Authorization"] != "tvly-123" {
		t.Fatalf("resolve = %v err=%v", out, err)
	}
	if _, err := ResolveMCPHeaders(MCPServer{Headers: map[string]string{"X": "env:DEFINITELY_UNSET_VAR_XYZ"}}); err == nil || !strings.Contains(err.Error(), "not set") {
		t.Fatalf("unset env must error: %v", err)
	}
	if out, err := ResolveMCPHeaders(MCPServer{}); out != nil || err != nil {
		t.Fatalf("no headers = %v %v", out, err)
	}
	// Empty-but-set env:VAR must resolve to "" (allowed), distinct from unset.
	t.Setenv("MCP_EMPTY_HEADER", "")
	out, err = ResolveMCPHeaders(MCPServer{Headers: map[string]string{"X-Empty": "env:MCP_EMPTY_HEADER"}})
	if err != nil {
		t.Fatalf("empty-but-set header env: = %v", err)
	}
	if out["X-Empty"] != "" {
		t.Fatalf("empty header value = %q", out["X-Empty"])
	}
}

func TestMCPTransportValidation(t *testing.T) {
	cfg := mcpBaseConfig()
	cfg.MCP = map[string]MCPServer{
		"legacy": {Provider: "zhipu", URL: "https://x/sse", Transport: "sse"},
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("transport sse rejected: %v", err)
	}
	cfg.MCP["legacy"] = MCPServer{Provider: "zhipu", URL: "https://x", Transport: "http2"}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "transport") {
		t.Fatalf("bad transport must fail: %v", err)
	}
	// Routes cannot target legacy sse backends.
	cfg.MCP["legacy"] = MCPServer{Provider: "zhipu", URL: "https://x/sse", Transport: "sse"}
	cfg.MCPRoutes = map[string]MCPRoute{
		"r": {Targets: []MCPRouteTarget{{Server: "legacy", Tools: map[string]string{"a": "b"}}}},
	}
	if err := cfg.validate(); err == nil || !strings.Contains(err.Error(), "legacy sse transport") {
		t.Fatalf("route targeting sse backend must fail: %v", err)
	}
}

func TestMCPStdioValidation(t *testing.T) {
	good := func() *Config {
		cfg := mcpBaseConfig()
		cfg.MCP = map[string]MCPServer{
			"vision": {
				Transport: "stdio",
				Provider:  "zhipu",
				Command:   []string{"npx", "-y", "@z_ai/mcp-server"},
				Env:       map[string]string{"Z_AI_API_KEY": "${account.api_key}", "Z_AI_MODE": "env:Z_AI_MODE"},
			},
			"local": {Transport: "stdio", Command: []string{"/usr/local/bin/mcp-fs"}},
		}
		return cfg
	}
	if err := good().validate(); err != nil {
		t.Fatalf("valid stdio config rejected: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*MCPServer)
		want   string
	}{
		{"no command", func(s *MCPServer) { s.Command = nil }, "requires command"},
		{"url set", func(s *MCPServer) { s.URL = "https://x" }, "meaningless with transport stdio"},
		{"auth_header", func(s *MCPServer) { s.AuthHeader = "X-K" }, "meaningless with transport stdio"},
		{"headers", func(s *MCPServer) { s.Headers = map[string]string{"X": "env:Y"} }, "meaningless with transport stdio"},
		{"proxy_url", func(s *MCPServer) { s.ProxyURL = "off" }, "meaningless with transport stdio"},
		{"account key without provider", func(s *MCPServer) { s.Provider = ""; s.Auth = "none" }, "no provider: pool"},
		{"account key on oauth provider", func(s *MCPServer) { s.Provider = "codex" }, "OAuth provider"},
		{"bad env key", func(s *MCPServer) { s.Env["BAD KEY"] = "x" }, "not a valid environment variable name"},
		{"empty env value", func(s *MCPServer) { s.Env["Z_AI_MODE"] = "" }, "is empty"},
		{"bad env indirection", func(s *MCPServer) { s.Env["Z_AI_MODE"] = "env:bad-var" }, "invalid env: indirection"},
		{"literal on benign env key is allowed", func(s *MCPServer) { s.Env["Z_AI_MODE"] = "ZHIPU" }, ""},
		{"literal on credential-shaped key", func(s *MCPServer) { s.Env["Z_AI_API_KEY"] = "sk-literal" }, "credential-shaped name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := good()
			s := cfg.MCP["vision"]
			tc.mutate(&s)
			cfg.MCP["vision"] = s
			err := cfg.validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want substring %q", err, tc.want)
			}
		})
	}
}

// TestResolveMCPStdioEnv: the two indirection forms resolve, benign literals
// pass through, unset variables and credential-shaped literals fail closed
// (the rule is enforced again here so a caller that skips validation still
// can't smuggle config-held credentials).
func TestResolveMCPStdioEnv(t *testing.T) {
	t.Setenv("MY_MODE", "ZHIPU")
	srv := MCPServer{Env: map[string]string{
		"Z_AI_API_KEY": "${account.api_key}",
		"Z_AI_MODE":    "env:MY_MODE",
		"PLAIN_FLAG":   "on",
	}}
	env, err := ResolveMCPStdioEnv(srv, "key-1")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "Z_AI_API_KEY=key-1") ||
		!strings.Contains(joined, "Z_AI_MODE=ZHIPU") ||
		!strings.Contains(joined, "PLAIN_FLAG=on") {
		t.Fatalf("env = %v", env)
	}
	if _, err := ResolveMCPStdioEnv(srv, ""); err == nil || !strings.Contains(err.Error(), "requires a provider account") {
		t.Fatalf("account key without account = %v", err)
	}
	if _, err := ResolveMCPStdioEnv(MCPServer{Env: map[string]string{"X": "env:DEFINITELY_UNSET_VAR_XYZ"}}, ""); err == nil || !strings.Contains(err.Error(), "not set") {
		t.Fatalf("unset env: = %v", err)
	}
	if _, err := ResolveMCPStdioEnv(MCPServer{Env: map[string]string{"MY_AUTH_TOKEN": "literal-value"}}, ""); err == nil || !strings.Contains(err.Error(), "credential-shaped keys") {
		t.Fatalf("literal credential-shaped env = %v", err)
	}
	// Empty-but-set env:VAR must resolve to "" (allowed), distinct from unset.
	t.Setenv("MCP_EMPTY_ENV", "")
	if env, err := ResolveMCPStdioEnv(MCPServer{Env: map[string]string{"EMPTY": "env:MCP_EMPTY_ENV"}}, ""); err != nil {
		t.Fatalf("empty-but-set env: = %v", err)
	} else if !strings.Contains(strings.Join(env, "\n"), "EMPTY=") {
		t.Fatalf("empty env value missing: %v", env)
	}
}
