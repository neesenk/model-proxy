// proxy_mcp_probe.go — the CommandAPI MCP handshake probe (Web 测活 button):
// the daemon twin of `model-proxy mcp test <name>`. One runtime snapshot per
// probe (red line); network/process I/O runs after the snapshot, never under
// a lock. Server names probe that one upstream; route names aggregate their
// enabled member servers through the gateway's own MergeCanonicalTools, so
// the Web Routes detail shows the canonical surface a client would see.
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

// probeMCP implements admin.Ports.ProbeMCP. Server names probe the upstream
// directly; route names aggregate the route's enabled members (see
// probeMCPRoute). Disabled surfaces (server or route) fail closed with the
// same disabled report — mirroring the live gateway, which 404s disabled
// routes instead of serving their aggregated surface.
func (p *Proxy) probeMCP(ctx context.Context, name string) (appapi.MCPProbeResult, error) {
	snap := p.SnapshotRuntime()
	if route, isRoute := snap.Cfg.MCPRoutes[name]; isRoute {
		if !route.MCPRouteEffectiveEnabled() {
			return appapi.MCPProbeResult{
				OK:    false,
				Route: true,
				Error: fmt.Sprintf("mcp route %q is disabled (enabled: false)", name),
			}, nil
		}
		return p.probeMCPRoute(ctx, snap, name, route), nil
	}
	srv, ok := snap.Cfg.MCP[name]
	if !ok {
		return appapi.MCPProbeResult{}, fmt.Errorf("no mcp server %q in config", name)
	}
	if !srv.MCPEffectiveEnabled() {
		return appapi.MCPProbeResult{OK: false, Error: fmt.Sprintf("mcp server %q is disabled (enabled: false)", name)}, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, srv.MCPTimeoutDuration())
	defer cancel()
	if srv.MCPStdio() {
		res, _ := p.probeMCPStdio(probeCtx, snap, srv)
		return res, nil
	}
	res, _ := p.probeMCPHTTP(probeCtx, snap, srv)
	return res, nil
}

// probeMCPRoute aggregates a route's canonical tool surface: probe every
// enabled member server in target order, merge the per-backend tool lists
// through the same MergeCanonicalTools the live gateway uses, and project the
// result. A member that fails is skipped — the route degrades to the
// remaining targets, exactly like the live surface (canonical names whose
// backend tool is missing are dropped by the merge).
func (p *Proxy) probeMCPRoute(ctx context.Context, snap RuntimeSnapshot, name string, route configdomain.MCPRoute) appapi.MCPProbeResult {
	start := time.Now()
	toolsByServer := map[string][]mcpkg.ToolSpec{}
	mappings := make([]mcpkg.TargetMapping, 0, len(route.Targets))
	var firstErr error
	probed := 0
	for _, t := range route.Targets {
		srv, ok := snap.Cfg.MCP[t.Server]
		if !ok || !srv.MCPEffectiveEnabled() {
			continue
		}
		mappings = append(mappings, mcpkg.TargetMapping{Server: t.Server, Tools: t.Tools})
		probeCtx, cancel := context.WithTimeout(ctx, srv.MCPTimeoutDuration())
		var res appapi.MCPProbeResult
		var specs []mcpkg.ToolSpec
		if srv.MCPStdio() {
			res, specs = p.probeMCPStdio(probeCtx, snap, srv)
		} else {
			res, specs = p.probeMCPHTTP(probeCtx, snap, srv)
		}
		cancel()
		if res.OK {
			toolsByServer[t.Server] = specs
			probed++
			continue
		}
		if firstErr == nil {
			firstErr = fmt.Errorf("%s: %s", t.Server, res.Error)
		}
	}
	total := len(mappings)
	if probed == 0 {
		msg := "no enabled targets to probe"
		if firstErr != nil {
			msg = firstErr.Error()
		}
		return appapi.MCPProbeResult{OK: false, Route: true, TargetsProbed: 0, TargetsTotal: total, Error: msg}
	}
	toolNames, toolDetails := probeTools(mcpkg.MergeCanonicalTools(toolsByServer, mappings))
	return appapi.MCPProbeResult{
		OK:            true,
		Route:         true,
		ServerName:    name,
		TargetsProbed: probed,
		TargetsTotal:  total,
		Tools:         toolNames,
		ToolDetails:   toolDetails,
		LatencyMs:     time.Since(start).Milliseconds(),
	}
}

// probeTools projects a probe's tool specs into the web-facing name list +
// detail pairs (name/description — schemas stay out of the API surface).
func probeTools(tools []mcpkg.ToolSpec) ([]string, []appapi.MCPToolDetail) {
	names := make([]string, 0, len(tools))
	details := make([]appapi.MCPToolDetail, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
		details = append(details, appapi.MCPToolDetail{Name: t.Name, Description: t.Description})
	}
	return names, details
}

// probeMCPHTTP handshakes a streamable-HTTP server through its configured
// credentials (first live pool account, static env headers on top). The raw
// tool specs are returned alongside the projection so route aggregation can
// merge them without a second handshake.
func (p *Proxy) probeMCPHTTP(ctx context.Context, snap RuntimeSnapshot, srv configdomain.MCPServer) (appapi.MCPProbeResult, []mcpkg.ToolSpec) {
	auth, err := p.mcpProbeAuth(snap, srv)
	if err != nil {
		return appapi.MCPProbeResult{OK: false, Error: err.Error()}, nil
	}
	client := p.mcpClientFor(snap, srv, p.mcpProbeAccount(snap, srv))
	start := time.Now()
	res, err := mcpkg.Probe(ctx, client, srv.URL, auth)
	if err != nil {
		return appapi.MCPProbeResult{OK: false, Error: err.Error(), LatencyMs: time.Since(start).Milliseconds()}, nil
	}
	toolNames, toolDetails := probeTools(res.Tools)
	return appapi.MCPProbeResult{
		OK:            true,
		ServerName:    res.ServerName,
		ServerVersion: res.ServerVersion,
		Protocol:      res.Protocol,
		Sessionful:    res.Sessionful,
		Tools:         toolNames,
		ToolDetails:   toolDetails,
		LatencyMs:     time.Since(start).Milliseconds(),
	}, res.Tools
}

// probeMCPStdio spawns the child (same env resolution as the gateway) and
// handshakes over the child conn. The raw tool specs are returned alongside
// the projection for route aggregation.
func (p *Proxy) probeMCPStdio(ctx context.Context, snap RuntimeSnapshot, srv configdomain.MCPServer) (appapi.MCPProbeResult, []mcpkg.ToolSpec) {
	accountKey := ""
	if srv.MCPAuthMode() == "provider" {
		accounts := mcpAccounts(snap, srv)
		if len(accounts) == 0 {
			return appapi.MCPProbeResult{OK: false, Error: fmt.Sprintf("provider %q has no logged-in account — run `model-proxy login %s`", srv.Provider, srv.Provider)}, nil
		}
		prov := snap.Providers[accounts[0]]
		kr, ok := prov.(provider.KeyReporter)
		if !ok {
			return appapi.MCPProbeResult{OK: false, Error: fmt.Sprintf("provider %q cannot supply a raw API key (apikey providers only)", srv.Provider)}, nil
		}
		key, err := kr.ReportKey()
		if err != nil {
			return appapi.MCPProbeResult{OK: false, Error: err.Error()}, nil
		}
		accountKey = key
	}
	env, err := configdomain.ResolveMCPStdioEnv(srv, accountKey)
	if err != nil {
		return appapi.MCPProbeResult{OK: false, Error: err.Error()}, nil
	}
	start := time.Now()
	conn, err := mcpkg.StartStdio(srv.Command, env)
	if err != nil {
		return appapi.MCPProbeResult{OK: false, Error: fmt.Sprintf("stdio start: %v", err), LatencyMs: time.Since(start).Milliseconds()}, nil
	}
	defer conn.Close()
	result := appapi.MCPProbeResult{Stdio: true}
	initResp, err := conn.Call(ctx, mcpRouteInitBody)
	if err != nil {
		result.Error = fmt.Sprintf("initialize: %v", err)
		result.LatencyMs = time.Since(start).Milliseconds()
		return result, nil
	}
	proto, serverName, serverVer, err := mcpkg.ParseInitializeResult(initResp)
	if err != nil || proto == "" {
		result.Error = "initialize: no protocolVersion in response"
		result.LatencyMs = time.Since(start).Milliseconds()
		return result, nil
	}
	conn.Call(ctx, mcpNotifInitBody) //nolint — best-effort
	listResp, err := conn.Call(ctx, []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`))
	if err != nil {
		result.Error = fmt.Sprintf("tools/list: %v", err)
		result.LatencyMs = time.Since(start).Milliseconds()
		return result, nil
	}
	tools, err := mcpkg.ParseToolsListResult(listResp)
	if err != nil {
		result.Error = fmt.Sprintf("tools/list: %v", err)
		result.LatencyMs = time.Since(start).Milliseconds()
		return result, nil
	}
	toolNames, toolDetails := probeTools(tools)
	result.OK = true
	result.ServerName = serverName
	result.ServerVersion = serverVer
	result.Protocol = proto
	result.Tools = toolNames
	result.ToolDetails = toolDetails
	result.LatencyMs = time.Since(start).Milliseconds()
	return result, tools
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
