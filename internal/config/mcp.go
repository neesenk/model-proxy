package config

import (
	"fmt"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"model-proxy/internal/upstreamproxy"
)

// MCPServer declares one remote MCP (Model Context Protocol) backend exposed
// by the gateway at /mcp/<name>. Credential-bearing servers reference a
// provider's account pool (`provider:`); anonymous public servers set
// `auth: none`. See docs/research/design-mcp-gateway.md.
type MCPServer struct {
	// URL is the upstream streamable-HTTP MCP endpoint. Required for http
	// transports; must be EMPTY for transport: stdio.
	URL string `yaml:"url"`
	// Transport selects the upstream wire transport: "" or "streamable"
	// (default, the 2025 streamable-HTTP transport), "sse" (the legacy
	// 2024-11-05 HTTP+SSE transport: GET opens the event stream, the upstream
	// names a POST endpoint in the first endpoint event, the gateway rewrites
	// it back to /mcp/<name> and binds both channels to one account), or
	// "stdio" (a LOCAL child process: newline-delimited JSON-RPC on
	// stdin/stdout, one process per client session, killed with the session).
	// Legacy sse servers cannot be mcp_routes: targets (the aggregated
	// handshake is streamable-only); stdio servers can.
	Transport string `yaml:"transport"`
	// Command is the stdio child command (transport: stdio only), argv form
	// — e.g. [npx, -y, "@z_ai/mcp-server"]. Resolved against PATH.
	Command []string `yaml:"command"`
	// Env sets extra child environment (transport: stdio only). Indirect forms:
	// "${account.api_key}" injects the picked pool account's raw key (needs
	// provider:, apikey providers only), "env:VAR" copies a process environment
	// variable. Literal values are allowed for benign keys (mode flags like
	// Z_AI_MODE) but REJECTED for credential-shaped names (KEY/TOKEN/SECRET/
	// PASSWORD/AUTH/CREDENTIAL segments — red line 3: credentials never land
	// in config; headers: stays literal-free altogether).
	// The child gets a MINIMAL base env (PATH/HOME/TMPDIR/LANG), never the
	// daemon's full environment.
	Env map[string]string `yaml:"env"`
	// Provider names a providers: entry whose account pool supplies the
	// credential injected on upstream requests. Empty requires `auth: none`.
	Provider string `yaml:"provider"`
	// Auth selects credential injection: "" defaults to "provider" when
	// Provider is set, "none" otherwise. "none" with provider set (or
	// "provider" without one) is a validation error — the combination must
	// be unambiguous so a typo can never silently drop or invent auth.
	Auth string `yaml:"auth"`
	// AuthHeader overrides the credential header: empty or "Authorization"
	// injects `Authorization: Bearer <key>` via the provider's AuthHeaders;
	// any other name injects the RAW key under that header (e.g.
	// volcengine's `X-Agent-Plan-Key`) via provider.KeyReporter — apikey
	// providers only, OAuth providers fail closed.
	AuthHeader string `yaml:"auth_header"`
	// Timeout bounds one upstream POST exchange as a duration string
	// (default 300s — search/scrape tools can run for minutes). GET (SSE
	// server-stream) and DELETE are not time-capped by this value.
	Timeout string `yaml:"timeout"`
	// ProxyURL overrides the upstream proxy chain for this server's traffic
	// (same semantics as providers.<name>.proxy_url); empty follows the
	// provider's proxy_url when provider-backed, else the global chain.
	ProxyURL string `yaml:"proxy_url"`
	// Headers are static additional headers on every upstream request. Values
	// MUST use env:VAR indirection (resolved per request at send time) —
	// literal values are rejected at load, so credentials can never land in
	// config (red line 3). For third-party keyed services (Tavily etc.) where
	// no provider pool exists.
	Headers map[string]string `yaml:"headers"`
	// Enabled gates exposure: nil = enabled, explicit false = the endpoint
	// answers 404 and the server is hidden from `mcp list`. A declared but
	// disabled server keeps its config for later re-enable.
	Enabled *bool `yaml:"enabled"`
}

// MCPEffectiveEnabled reports whether the server is exposed (default true).
func (s MCPServer) MCPEffectiveEnabled() bool {
	return s.Enabled == nil || *s.Enabled
}

// MCPAuthMode resolves the effective credential mode ("provider" or "none").
func (s MCPServer) MCPAuthMode() string {
	if s.Auth != "" {
		return s.Auth
	}
	if s.Provider != "" {
		return "provider"
	}
	return "none"
}

// MCPTimeoutDuration parses Timeout (default 300s; malformed values are a
// validate-time error, this accessor never fails).
func (s MCPServer) MCPTimeoutDuration() time.Duration {
	if d, err := time.ParseDuration(s.Timeout); err == nil && d > 0 {
		return d
	}
	return 300 * time.Second
}

// mcpNameRE limits server names to a single safe URL path segment.
var mcpNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// mcpToolNameRE follows the MCP tool-name grammar for canonical route tools.
var mcpToolNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// MCPRoute aggregates several mcp: servers behind one canonical tool surface,
// exposed at /mcp/<name> in the same namespace as plain servers. The proxy
// owns the client session, merges tools/list from the backends under the
// declared canonical names, and fails tools/call over the targets in order.
type MCPRoute struct {
	// Targets is the ordered failover list; the first target also wins each
	// canonical tool's exposed schema.
	Targets []MCPRouteTarget `yaml:"targets"`
	// Enabled gates exposure like MCPServer.Enabled (nil = enabled).
	Enabled *bool `yaml:"enabled"`
}

// MCPRouteTarget binds one backend server and its tool-name equivalence.
type MCPRouteTarget struct {
	// Server references an mcp: server name (yaml key "mcp").
	Server string `yaml:"mcp"`
	// Tools maps canonical (client-visible) tool names to the backend's
	// actual tool names. Equivalence is a DECLARED fact: v1 passes arguments
	// through unchanged, so the operator asserts schema compatibility.
	Tools map[string]string `yaml:"tools"`
}

// MCPRouteEffectiveEnabled reports whether the route is exposed (default true).
func (r MCPRoute) MCPRouteEffectiveEnabled() bool {
	return r.Enabled == nil || *r.Enabled
}

// validateMCPRoutes checks the mcp_routes: section. Called from
// Config.validate after validateMCP (server references must already exist).
func (c *Config) validateMCPRoutes() error {
	for name, r := range c.MCPRoutes {
		if !mcpNameRE.MatchString(name) {
			return fmt.Errorf("mcp_routes %q: invalid name — use 1-64 chars of [A-Za-z0-9._-], starting with alphanumeric (it becomes the /mcp/<name> path segment)", name)
		}
		if _, clash := c.MCP[name]; clash {
			return fmt.Errorf("mcp_routes %q: name collides with an mcp: server — they share the /mcp/<name> namespace, rename one of them", name)
		}
		if len(r.Targets) == 0 {
			return fmt.Errorf("mcp_routes %q: no targets — add at least one {mcp, tools} entry", name)
		}
		for i, t := range r.Targets {
			if t.Server == "" {
				return fmt.Errorf("mcp_routes %q target %d: mcp is empty — reference an mcp: server name", name, i)
			}
			if _, ok := c.MCP[t.Server]; !ok {
				return fmt.Errorf("mcp_routes %q target %d: mcp %q is not defined under mcp:", name, i, t.Server)
			}
			if c.MCP[t.Server].Transport == "sse" {
				return fmt.Errorf("mcp_routes %q target %d: mcp %q uses the legacy sse transport — aggregated routes support streamable backends only", name, i, t.Server)
			}
			if len(t.Tools) == 0 {
				return fmt.Errorf("mcp_routes %q target %d: tools is empty — map canonical names to the backend's tool names", name, i)
			}
			for canonical, backend := range t.Tools {
				if !mcpToolNameRE.MatchString(canonical) {
					return fmt.Errorf("mcp_routes %q target %d: canonical tool %q invalid — use [A-Za-z0-9_-]{1,64} per the MCP spec", name, i, canonical)
				}
				if backend == "" {
					return fmt.Errorf("mcp_routes %q target %d: backend tool for canonical %q is empty", name, i, canonical)
				}
			}
		}
	}
	return nil
}

// validateMCP checks the mcp: section. Called from Config.validate.
func (c *Config) validateMCP() error {
	for name, s := range c.MCP {
		if !mcpNameRE.MatchString(name) {
			return fmt.Errorf("mcp %q: invalid name — use 1-64 chars of [A-Za-z0-9._-], starting with alphanumeric (it becomes the /mcp/<name> path segment)", name)
		}
		if s.MCPStdio() {
			if err := c.validateMCPStdio(name, s); err != nil {
				return err
			}
			continue
		}
		if s.URL == "" {
			return fmt.Errorf("mcp %q: url is empty — set the upstream MCP endpoint (streamable HTTP)", name)
		}
		u, err := url.Parse(s.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("mcp %q: url %q is not a valid http(s) URL", name, s.URL)
		}
		switch s.Transport {
		case "", "streamable", "sse":
		default:
			return fmt.Errorf("mcp %q: transport %q invalid — use `streamable` (default), `sse` (legacy HTTP+SSE) or `stdio` (local child process)", name, s.Transport)
		}
		switch s.Auth {
		case "", "none", "provider":
		default:
			return fmt.Errorf("mcp %q: auth %q invalid — use \"provider\" or \"none\" (or omit)", name, s.Auth)
		}
		if s.Provider == "" && s.MCPAuthMode() == "provider" {
			return fmt.Errorf("mcp %q: auth \"provider\" requires provider: — set a providers: entry or use auth: none", name)
		}
		if s.Provider != "" && s.MCPAuthMode() == "none" {
			return fmt.Errorf("mcp %q: provider %q is set but auth is \"none\" — drop one of them (the combination must be unambiguous)", name, s.Provider)
		}
		if s.Provider != "" {
			if _, ok := c.Providers[s.Provider]; !ok {
				return fmt.Errorf("mcp %q: provider %q is not defined under providers:", name, s.Provider)
			}
			// Custom auth headers need the raw key, which only apikey-backed
			// providers expose (provider.KeyReporter); OAuth providers (aqp,
			// codex) fail closed at request time — reject at load instead.
			if s.customAuthHeader() {
				if id := c.Providers[s.Provider].Provider; id == "aqp" || id == "codex" {
					return fmt.Errorf("mcp %q: auth_header %q needs the raw API key, which OAuth provider %q cannot supply — custom auth headers only work with apikey providers", name, s.AuthHeader, id)
				}
			}
		}
		if s.AuthHeader != "" {
			if !headerNameValid(s.AuthHeader) {
				return fmt.Errorf("mcp %q: auth_header %q is not a valid HTTP header name", name, s.AuthHeader)
			}
			if s.MCPAuthMode() != "provider" {
				return fmt.Errorf("mcp %q: auth_header is meaningless with auth: none — drop it", name)
			}
		}
		if s.Timeout != "" {
			d, err := time.ParseDuration(s.Timeout)
			if err != nil || d <= 0 {
				return fmt.Errorf("mcp %q: timeout %q is not a valid duration (e.g. 120s, 5m)", name, s.Timeout)
			}
		}
		if err := upstreamproxy.ValidateSetting(s.ProxyURL); err != nil {
			return fmt.Errorf("mcp %q: proxy_url: %w", name, err)
		}
		for h, v := range s.Headers {
			if !headerNameValid(h) {
				return fmt.Errorf("mcp %q: headers key %q is not a valid HTTP header name", name, h)
			}
			if !envRefValid(v) {
				return fmt.Errorf("mcp %q: headers[%q] must use env:VAR indirection (e.g. env:TAVILY_API_KEY) — literal header values are rejected so credentials never land in config", name, h)
			}
		}
	}
	return nil
}

// envRefValid enforces the env:VAR indirection form for static header values.
func envRefValid(v string) bool {
	if !strings.HasPrefix(v, "env:") {
		return false
	}
	name := strings.TrimPrefix(v, "env:")
	if name == "" {
		return false
	}
	for i, r := range name {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_' || (i > 0 && r >= '0' && r <= '9')
		if !ok {
			return false
		}
	}
	return true
}

// ResolveMCPStdioEnv builds the child environment for a transport:stdio
// server: a MINIMAL base (PATH/HOME/TMPDIR/LANG — never the daemon's full
// environment) plus the configured entries with ${account.api_key} replaced
// by accountKey (the picked pool account's raw key, "" when the server has
// no provider credential) and env:VAR copied from the process environment.
// Literal values are allowed for benign keys (mode flags etc.) but rejected
// for credential-shaped names (see envKeySensitive) — enforced fail-closed
// at resolve time too, so credentials never land in config.
// Shared by the daemon (internal/app) and the `mcp test` CLI.
func ResolveMCPStdioEnv(s MCPServer, accountKey string) ([]string, error) {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"TMPDIR=" + os.Getenv("TMPDIR"),
		"LANG=" + os.Getenv("LANG"),
	}
	for k, v := range s.Env {
		switch {
		case v == "${account.api_key}":
			if accountKey == "" {
				return nil, fmt.Errorf("env[%s]: ${account.api_key} requires a provider account", k)
			}
			env = append(env, k+"="+accountKey)
		case strings.HasPrefix(v, "env:"):
			varName := strings.TrimPrefix(v, "env:")
			value := os.Getenv(varName)
			if value == "" {
				return nil, fmt.Errorf("env[%s]: environment variable %s is not set", k, varName)
			}
			env = append(env, k+"="+value)
		case envKeySensitive(k):
			return nil, fmt.Errorf("env[%s]: literal values are rejected for credential-shaped keys — use ${account.api_key} or env:VAR indirection", k)
		default:
			env = append(env, k+"="+v)
		}
	}
	return env, nil
}

// envKeySensitive reports whether an environment variable name looks like it
// carries a credential (per-segment match, so Z_AI_MODE is benign while
// OPENAI_API_KEY or AUTH_TOKEN are not). Best-effort guardrail: a credential
// under a deliberately benign name is a config-authoring decision, not
// something validation can prove.
func envKeySensitive(name string) bool {
	for _, seg := range strings.FieldsFunc(strings.ToUpper(name), func(r rune) bool {
		return !('A' <= r && r <= 'Z') && !('0' <= r && r <= '9')
	}) {
		switch seg {
		case "KEY", "APIKEY", "TOKEN", "SECRET", "PASSWORD", "PASSWD", "AUTH", "CREDENTIAL", "CREDENTIALS", "PRIVATE":
			return true
		}
	}
	return false
}

// ResolveMCPHeaders resolves the server's static headers against the process
// environment. An unset variable is a send-time configuration error
// (fail-closed: silently dropping an auth header would surface as confusing
// upstream 401s).
func ResolveMCPHeaders(s MCPServer) (map[string]string, error) {
	if len(s.Headers) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(s.Headers))
	for h, ref := range s.Headers {
		varName := strings.TrimPrefix(ref, "env:")
		value := os.Getenv(varName)
		if value == "" {
			return nil, fmt.Errorf("headers[%q]: environment variable %s is not set", h, varName)
		}
		out[h] = value
	}
	return out, nil
}

// customAuthHeader reports whether auth injection uses a non-Authorization
// header carrying the raw key.
func (s MCPServer) customAuthHeader() bool {
	return s.AuthHeader != "" && !strings.EqualFold(s.AuthHeader, "Authorization")
}

// MCPStdio reports whether the server is a local stdio child process.
func (s MCPServer) MCPStdio() bool { return s.Transport == "stdio" }

// validateMCPStdio checks a transport: stdio server: command/env shape and
// the rejection of every http-only knob.
func (c *Config) validateMCPStdio(name string, s MCPServer) error {
	if len(s.Command) == 0 || s.Command[0] == "" {
		return fmt.Errorf("mcp %q: transport stdio requires command: (e.g. [npx, -y, \"@z_ai/mcp-server\"])", name)
	}
	if s.URL != "" {
		return fmt.Errorf("mcp %q: url is meaningless with transport stdio — the backend is a local command, drop url", name)
	}
	if s.AuthHeader != "" {
		return fmt.Errorf("mcp %q: auth_header is meaningless with transport stdio (credentials go through env: templates)", name)
	}
	if len(s.Headers) > 0 {
		return fmt.Errorf("mcp %q: headers is meaningless with transport stdio (no HTTP hop; use env:)", name)
	}
	if s.ProxyURL != "" {
		return fmt.Errorf("mcp %q: proxy_url is meaningless with transport stdio (no network hop)", name)
	}
	switch s.MCPAuthMode() {
	case "provider":
		if _, ok := c.Providers[s.Provider]; !ok {
			return fmt.Errorf("mcp %q: provider %q is not defined under providers:", name, s.Provider)
		}
	case "none":
		if s.Provider != "" {
			return fmt.Errorf("mcp %q: provider %q is set but auth is \"none\" — drop one of them", name, s.Provider)
		}
	}
	for k, v := range s.Env {
		if !envNameValid(k) {
			return fmt.Errorf("mcp %q: env key %q is not a valid environment variable name", name, k)
		}
		if v == "" {
			return fmt.Errorf("mcp %q: env[%q] is empty", name, k)
		}
		switch {
		case v == "${account.api_key}":
			if s.MCPAuthMode() != "provider" {
				return fmt.Errorf("mcp %q: env[%q] uses ${account.api_key} but the server has no provider: pool to take the key from", name, k)
			}
			if id := c.Providers[s.Provider].Provider; id == "aqp" || id == "codex" {
				return fmt.Errorf("mcp %q: env[%q] uses ${account.api_key}, which OAuth provider %q cannot supply (apikey providers only)", name, k, id)
			}
		case strings.HasPrefix(v, "env:"):
			if !envRefValid(v) {
				return fmt.Errorf("mcp %q: env[%q] has invalid env: indirection %q", name, k, v)
			}
		case envKeySensitive(k):
			return fmt.Errorf("mcp %q: env[%q] has a credential-shaped name — literal values are rejected here, use ${account.api_key} or env:VAR indirection (same rule as headers:)", name, k)
		}
	}
	return nil
}

// envNameValid checks an environment variable name.
func envNameValid(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_' || (i > 0 && r >= '0' && r <= '9')
		if !ok {
			return false
		}
	}
	return true
}

// headerNameValid enforces the RFC 7230 token grammar for header names.
func headerNameValid(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' ||
			strings.ContainsRune("!#$%&'*+-.^_|~", r)
		if !ok {
			return false
		}
	}
	return http.CanonicalHeaderKey(name) != ""
}
