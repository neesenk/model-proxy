// Package climcp owns the `mcp` command domain: `mcp list` prints the mcp:
// gateway servers from config, `mcp test <name>` runs the MCP handshake
// (initialize + tools/list) against one server through its configured
// credentials. Both are offline diagnostics (no daemon needed), mirroring
// `models`/`test`. The handshake itself lives in internal/mcp.Probe; this
// package only adapts config + credential pools to it.
package climcp

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"model-proxy/internal/accounts"
	cliframework "model-proxy/internal/cli/framework"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/display"
	mcpkg "model-proxy/internal/mcp"
	"model-proxy/internal/provider"
	"model-proxy/internal/providerbuild"
	"model-proxy/internal/upstreamproxy"
)

// RunMCP is the process entry for `model-proxy mcp`.
func RunMCP(args []string) {
	cfg := cliframework.LoadCmdConfig(args)
	pos := cliframework.PositionalArgs(args)
	sub := "list"
	if len(pos) > 0 {
		sub = pos[0]
	}
	switch sub {
	case "list":
		cmdList(cfg)
	case "test":
		if len(pos) < 2 {
			fmt.Fprintf(os.Stderr, "%s usage: model-proxy mcp test <name> [--config PATH]\n", display.Red("✗"))
			os.Exit(1)
		}
		cmdTest(cfg, pos[1])
	default:
		fmt.Fprintf(os.Stderr, "%s unknown mcp subcommand %q — usage: model-proxy mcp [list|test <name>]\n", display.Red("✗"), sub)
		os.Exit(1)
	}
}

func cmdList(cfg *configdomain.Config) {
	if len(cfg.MCP) == 0 && len(cfg.MCPRoutes) == 0 {
		fmt.Println("no mcp servers configured — add an mcp: section to config.yaml")
		return
	}
	names := make([]string, 0, len(cfg.MCP))
	for name := range cfg.MCP {
		names = append(names, name)
	}
	sort.Strings(names)
	fmt.Printf("%-20s %-9s %-9s %-12s %s\n", "NAME", "ENABLED", "AUTH", "PROVIDER", "URL")
	for _, name := range names {
		s := cfg.MCP[name]
		enabled := "yes"
		if !s.MCPEffectiveEnabled() {
			enabled = display.Dim("no")
		}
		prov := s.Provider
		if prov == "" {
			prov = "-"
		}
		fmt.Printf("%-20s %-9s %-9s %-12s %s\n", name, enabled, s.MCPAuthMode(), prov, s.URL)
	}
	if len(cfg.MCPRoutes) == 0 {
		return
	}
	rnames := make([]string, 0, len(cfg.MCPRoutes))
	for name := range cfg.MCPRoutes {
		rnames = append(rnames, name)
	}
	sort.Strings(rnames)
	fmt.Printf("\n%-20s %-9s %s\n", "ROUTE", "ENABLED", "TARGETS (failover order)")
	for _, name := range rnames {
		r := cfg.MCPRoutes[name]
		enabled := "yes"
		if !r.MCPRouteEffectiveEnabled() {
			enabled = display.Dim("no")
		}
		targets := make([]string, 0, len(r.Targets))
		for _, t := range r.Targets {
			targets = append(targets, fmt.Sprintf("%s (%d tools)", t.Server, len(t.Tools)))
		}
		fmt.Printf("%-20s %-9s %s\n", name, enabled, strings.Join(targets, " → "))
	}
}

func cmdTest(cfg *configdomain.Config, name string) {
	if route, isRoute := cfg.MCPRoutes[name]; isRoute {
		members := make([]string, 0, len(route.Targets))
		for _, t := range route.Targets {
			members = append(members, t.Server)
		}
		fmt.Fprintf(os.Stderr, "%s %q is an aggregated route over [%s] — test member servers individually (`mcp test <server>`), or probe the route through the running daemon at /mcp/%s\n", display.Red("✗"), name, strings.Join(members, ", "), name)
		os.Exit(1)
	}
	srv, ok := cfg.MCP[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "%s no mcp server %q in config; configured: %v\n", display.Red("✗"), name, mcpNames(cfg))
		os.Exit(1)
	}
	if !srv.MCPEffectiveEnabled() {
		fmt.Fprintf(os.Stderr, "%s mcp server %q is disabled (enabled: false)\n", display.Red("✗"), name)
		os.Exit(1)
	}
	if srv.MCPStdio() {
		cmdTestStdio(cfg, name, srv)
		return
	}
	auth, account, err := mcpAuthFor(cfg, srv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s %v\n", display.Red("✗"), err)
		os.Exit(1)
	}
	client := &http.Client{Transport: upstreamproxy.AutoTransport(), CheckRedirect: mcpkg.CheckNoCrossOriginRedirect}
	ctx, cancel := context.WithTimeout(context.Background(), srv.MCPTimeoutDuration())
	defer cancel()
	start := time.Now()
	res, err := mcpkg.Probe(ctx, client, srv.URL, auth)
	lat := time.Since(start).Round(time.Millisecond)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s %s — %v (%s)\n", display.Red("✗"), name, err, lat)
		os.Exit(1)
	}
	session := "stateless"
	if res.Sessionful {
		session = "sessionful"
	}
	via := ""
	if account != "" {
		via = fmt.Sprintf(" via %s", account)
	}
	fmt.Printf("%s %s → %s %s (%s, %s)%s — %d tools (%s)\n",
		display.Green("✓"), name, res.ServerName, res.ServerVersion, res.Protocol, session, via, len(res.Tools), lat)
	const maxShow = 12
	for i, tool := range res.Tools {
		if i >= maxShow {
			fmt.Printf("  … and %d more\n", len(res.Tools)-maxShow)
			break
		}
		fmt.Printf("  - %s\n", tool)
	}
}

// cmdTestStdio probes a stdio server by spawning it locally (same env
// resolution as the daemon) and running the handshake over the child conn.
func cmdTestStdio(cfg *configdomain.Config, name string, srv configdomain.MCPServer) {
	accountKey, account, err := mcpStdioKeyFor(cfg, srv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s %v\n", display.Red("✗"), err)
		os.Exit(1)
	}
	env, err := configdomain.ResolveMCPStdioEnv(srv, accountKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s %v\n", display.Red("✗"), err)
		os.Exit(1)
	}
	start := time.Now()
	conn, err := mcpkg.StartStdio(srv.Command, env)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s %s — stdio start: %v\n", display.Red("✗"), name, err)
		os.Exit(1)
	}
	defer conn.Close()
	probeCtx, probeCancel := context.WithTimeout(context.Background(), srv.MCPTimeoutDuration())
	defer probeCancel()
	lat := func() time.Duration { return time.Since(start).Round(time.Millisecond) }
	initResp, err := conn.Call(probeCtx, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"model-proxy-probe","version":"1.0"}}}`))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s %s — initialize: %v (%s)\n", display.Red("✗"), name, err, lat())
		os.Exit(1)
	}
	proto, serverName, serverVer, err := mcpkg.ParseInitializeResult(initResp)
	if err != nil || proto == "" {
		fmt.Fprintf(os.Stderr, "%s %s — initialize: no protocolVersion in response (%s)\n", display.Red("✗"), name, lat())
		os.Exit(1)
	}
	conn.Call(probeCtx, []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)) //nolint — best-effort
	listResp, err := conn.Call(probeCtx, []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`))
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s %s — tools/list: %v (%s)\n", display.Red("✗"), name, err, lat())
		os.Exit(1)
	}
	tools, err := mcpkg.ParseToolsListResult(listResp)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s %s — tools/list: %v (%s)\n", display.Red("✗"), name, err, lat())
		os.Exit(1)
	}
	via := ""
	if account != "" {
		via = fmt.Sprintf(" via %s", account)
	}
	toolNames := make([]string, 0, len(tools))
	for _, tool := range tools {
		toolNames = append(toolNames, tool.Name)
	}
	fmt.Printf("%s %s → %s %s (%s, stdio)%s — %d tools (%s)\n",
		display.Green("✓"), name, serverName, serverVer, proto, via, len(tools), lat())
	const maxShow = 12
	for i, tool := range toolNames {
		if i >= maxShow {
			fmt.Printf("  … and %d more\n", len(toolNames)-maxShow)
			break
		}
		fmt.Printf("  - %s\n", tool)
	}
}

// firstProviderAccount builds the provider set once and resolves the first
// live account for a provider-backed MCP server: pool virtual ids first, the
// plain provider name otherwise.
func firstProviderAccount(cfg *configdomain.Config, srv configdomain.MCPServer) (provider.Provider, string, error) {
	built := providerbuild.BuildProviders(cfg, accounts.NewStore(accounts.HomeDir()), providerbuild.BuildOpts())
	ids := built.PoolIndex[srv.Provider]
	if len(ids) == 0 {
		if _, ok := built.Providers[srv.Provider]; ok {
			ids = []string{srv.Provider}
		}
	}
	if len(ids) == 0 {
		return nil, "", fmt.Errorf("provider %q has no logged-in account — run `model-proxy login %s`", srv.Provider, srv.Provider)
	}
	prov := built.Providers[ids[0]]
	if prov == nil {
		return nil, "", fmt.Errorf("provider %q account %q is not runnable", srv.Provider, ids[0])
	}
	return prov, ids[0], nil
}

// mcpStdioKeyFor resolves the account key for a provider-backed stdio server
// (first live pool account; "" for auth: none).
func mcpStdioKeyFor(cfg *configdomain.Config, srv configdomain.MCPServer) (string, string, error) {
	if srv.MCPAuthMode() != "provider" {
		return "", "", nil
	}
	prov, id, err := firstProviderAccount(cfg, srv)
	if err != nil {
		return "", "", err
	}
	kr, ok := prov.(provider.KeyReporter)
	if !ok {
		return "", "", fmt.Errorf("provider %q cannot supply a raw API key (apikey providers only)", srv.Provider)
	}
	key, err := kr.ReportKey()
	if err != nil {
		return "", "", err
	}
	return key, id, nil
}

// mcpAuthFor builds the credential injector for one server. Provider-backed
// servers bind the first live pool account (the daemon spreads/rotates; the
// diagnostic only needs one working credential). Static env-referenced
// headers apply on top of either auth mode.
func mcpAuthFor(cfg *configdomain.Config, srv configdomain.MCPServer) (func(*http.Request) error, string, error) {
	static, err := configdomain.ResolveMCPHeaders(srv)
	if err != nil {
		return nil, "", err
	}
	withStatic := func(next func(*http.Request) error) func(*http.Request) error {
		return func(req *http.Request) error {
			for k, v := range static {
				req.Header.Set(k, v)
			}
			if next != nil {
				return next(req)
			}
			return nil
		}
	}
	if srv.MCPAuthMode() != "provider" {
		if len(static) == 0 {
			return nil, "", nil
		}
		return withStatic(nil), "", nil
	}
	prov, id, err := firstProviderAccount(cfg, srv)
	if err != nil {
		return nil, "", err
	}
	if srv.AuthHeader == "" || srv.AuthHeader == "Authorization" {
		return withStatic(func(req *http.Request) error { return prov.AuthHeaders(req) }), id, nil
	}
	kr, ok := prov.(provider.KeyReporter)
	if !ok {
		return nil, "", fmt.Errorf("provider %q cannot supply a raw API key for auth_header %q (apikey providers only)", srv.Provider, srv.AuthHeader)
	}
	header := srv.AuthHeader
	return withStatic(func(req *http.Request) error {
		key, err := kr.ReportKey()
		if err != nil {
			return err
		}
		req.Header.Set(header, key)
		return nil
	}), id, nil
}

func mcpNames(cfg *configdomain.Config) []string {
	names := make([]string, 0, len(cfg.MCP))
	for name := range cfg.MCP {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
