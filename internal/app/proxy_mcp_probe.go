// proxy_mcp_probe.go — the CommandAPI MCP handshake probe (Web 测活 button):
// the daemon twin of `model-proxy mcp test <name>`. One runtime snapshot per
// probe (red line); network/process I/O runs after the snapshot, never under
// a lock. Route names are rejected — aggregated surfaces are probed through
// the gateway itself, not here.
package app

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"model-proxy/internal/appapi"
	configdomain "model-proxy/internal/config"
	mcpkg "model-proxy/internal/mcp"
	"model-proxy/internal/provider"
)

// probeMCP implements admin.Ports.ProbeMCP.
func (p *Proxy) probeMCP(ctx context.Context, name string) (appapi.MCPProbeResult, error) {
	snap := p.SnapshotRuntime()
	if _, isRoute := snap.Cfg.MCPRoutes[name]; isRoute {
		return appapi.MCPProbeResult{OK: false, Error: fmt.Sprintf("%q is an aggregated route — probe member servers individually, or call the gateway at /mcp/%s", name, name)}, nil
	}
	srv, ok := snap.Cfg.MCP[name]
	if !ok {
		return appapi.MCPProbeResult{}, fmt.Errorf("no mcp server %q in config", name)
	}
	if !srv.MCPEffectiveEnabled() {
		return appapi.MCPProbeResult{OK: false, Error: fmt.Sprintf("mcp server %q is disabled (enabled: false)", name)}, nil
	}
	timeout := srv.MCPTimeoutDuration()
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if srv.MCPStdio() {
		return p.probeMCPStdio(probeCtx, snap, srv), nil
	}
	return p.probeMCPHTTP(probeCtx, snap, srv), nil
}

// probeMCPHTTP handshakes a streamable-HTTP server through its configured
// credentials (first live pool account, static env headers on top).
func (p *Proxy) probeMCPHTTP(ctx context.Context, snap RuntimeSnapshot, srv configdomain.MCPServer) appapi.MCPProbeResult {
	auth, err := p.mcpProbeAuth(snap, srv)
	if err != nil {
		return appapi.MCPProbeResult{OK: false, Error: err.Error()}
	}
	client := p.mcpClientFor(snap, srv, p.mcpProbeAccount(snap, srv))
	start := time.Now()
	res, err := mcpkg.Probe(ctx, client, srv.URL, auth)
	if err != nil {
		return appapi.MCPProbeResult{OK: false, Error: err.Error(), LatencyMs: time.Since(start).Milliseconds()}
	}
	return appapi.MCPProbeResult{
		OK:            true,
		ServerName:    res.ServerName,
		ServerVersion: res.ServerVersion,
		Protocol:      res.Protocol,
		Sessionful:    res.Sessionful,
		Tools:         res.Tools,
		LatencyMs:     time.Since(start).Milliseconds(),
	}
}

// probeMCPStdio spawns the child (same env resolution as the gateway) and
// handshakes over the child conn.
func (p *Proxy) probeMCPStdio(ctx context.Context, snap RuntimeSnapshot, srv configdomain.MCPServer) appapi.MCPProbeResult {
	accountKey := ""
	if srv.MCPAuthMode() == "provider" {
		accounts := mcpAccounts(snap, srv)
		if len(accounts) == 0 {
			return appapi.MCPProbeResult{OK: false, Error: fmt.Sprintf("provider %q has no logged-in account — run `model-proxy login %s`", srv.Provider, srv.Provider)}
		}
		prov := snap.Providers[accounts[0]]
		kr, ok := prov.(provider.KeyReporter)
		if !ok {
			return appapi.MCPProbeResult{OK: false, Error: fmt.Sprintf("provider %q cannot supply a raw API key (apikey providers only)", srv.Provider)}
		}
		key, err := kr.ReportKey()
		if err != nil {
			return appapi.MCPProbeResult{OK: false, Error: err.Error()}
		}
		accountKey = key
	}
	env, err := configdomain.ResolveMCPStdioEnv(srv, accountKey)
	if err != nil {
		return appapi.MCPProbeResult{OK: false, Error: err.Error()}
	}
	start := time.Now()
	conn, err := mcpkg.StartStdio(srv.Command, env)
	if err != nil {
		return appapi.MCPProbeResult{OK: false, Error: fmt.Sprintf("stdio start: %v", err), LatencyMs: time.Since(start).Milliseconds()}
	}
	defer conn.Close()
	result := appapi.MCPProbeResult{Stdio: true}
	initResp, err := conn.Call(mcpRouteInitBody)
	if err != nil {
		result.Error = fmt.Sprintf("initialize: %v", err)
		result.LatencyMs = time.Since(start).Milliseconds()
		return result
	}
	proto, serverName, serverVer, err := mcpkg.ParseInitializeResult(initResp)
	if err != nil || proto == "" {
		result.Error = "initialize: no protocolVersion in response"
		result.LatencyMs = time.Since(start).Milliseconds()
		return result
	}
	conn.Call(mcpNotifInitBody) //nolint — best-effort
	listResp, err := conn.Call([]byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`))
	if err != nil {
		result.Error = fmt.Sprintf("tools/list: %v", err)
		result.LatencyMs = time.Since(start).Milliseconds()
		return result
	}
	tools, err := mcpkg.ParseToolsListResult(listResp)
	if err != nil {
		result.Error = fmt.Sprintf("tools/list: %v", err)
		result.LatencyMs = time.Since(start).Milliseconds()
		return result
	}
	toolNames := make([]string, 0, len(tools))
	for _, tool := range tools {
		toolNames = append(toolNames, tool.Name)
	}
	result.OK = true
	result.ServerName = serverName
	result.ServerVersion = serverVer
	result.Protocol = proto
	result.Tools = toolNames
	result.LatencyMs = time.Since(start).Milliseconds()
	return result
}

// mcpProbeAccount picks the probe's account (first live pool account; "" for
// auth: none).
func (p *Proxy) mcpProbeAccount(snap RuntimeSnapshot, srv configdomain.MCPServer) string {
	accounts := mcpAccounts(snap, srv)
	if len(accounts) == 0 {
		return ""
	}
	return accounts[0]
}

// mcpProbeAuth builds the credential injector for one probe (provider
// AuthHeaders / custom header via KeyReporter, static env headers first).
func (p *Proxy) mcpProbeAuth(snap RuntimeSnapshot, srv configdomain.MCPServer) (func(*http.Request) error, error) {
	static, err := configdomain.ResolveMCPHeaders(srv)
	if err != nil {
		return nil, err
	}
	var account string
	var prov provider.Provider
	if srv.MCPAuthMode() == "provider" {
		accounts := mcpAccounts(snap, srv)
		if len(accounts) == 0 {
			return nil, fmt.Errorf("provider %q has no logged-in account — run `model-proxy login %s`", srv.Provider, srv.Provider)
		}
		account = accounts[0]
		prov = snap.Providers[account]
		if prov == nil {
			return nil, fmt.Errorf("provider %q account %q is not runnable", srv.Provider, account)
		}
	}
	return func(req *http.Request) error {
		for k, v := range static {
			req.Header.Set(k, v)
		}
		if prov != nil {
			return mcpInjectAuth(req, prov, srv)
		}
		return nil
	}, nil
}
