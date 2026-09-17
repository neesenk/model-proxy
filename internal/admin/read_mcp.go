package admin

import (
	"sort"

	"model-proxy/internal/appapi"
)

// read_mcp.go — the /api/mcp read projection: config mcp:/mcp_routes: plus
// live session gauges, detached via the MCPState port. admin never touches
// the session table or reload-owned state directly — the composition root
// injects everything as plain values.

// MCPState is the detached input for the MCP surface projection: config shape
// plus live gauges, all plain values (no credentials anywhere — auth_header
// names and env indirections are also excluded on purpose).
type MCPState struct {
	Servers []MCPServerState
	Routes  []MCPRouteState
}

// MCPServerState is one mcp: server's detached facts.
type MCPServerState struct {
	Name      string
	Enabled   bool
	Transport string // streamable | sse | stdio
	Auth      string // provider | none
	Provider  string
	URL       string
	Command   string // stdio argv joined for display
	Accounts  int    // live pool accounts (provider-backed)
	Sessions  int    // live gateway sessions bound to this server
	// Call counters from the MCP gateway's own stats channel (process-lifetime).
	Calls        uint64
	Errors       uint64
	AvgLatencyMs uint64
}

// MCPRouteState is one mcp_routes: route's detached facts.
type MCPRouteState struct {
	Name     string
	Enabled  bool
	Targets  []MCPRouteTargetState
	Sessions int
	// Call counters from the MCP gateway's own stats channel (process-lifetime).
	Calls        uint64
	Errors       uint64
	AvgLatencyMs uint64
}

// MCPRouteTargetState is one route target (member server + tool count).
type MCPRouteTargetState struct {
	Server string
	Tools  int
}

// MCPSurface projects the MCP gateway state into the read API DTO (sorted,
// non-nil slices for a stable JSON shape).
func (s *Service) MCPSurface() appapi.MCPSurface {
	state := MCPState{}
	if s.ports.MCPState != nil {
		state = s.ports.MCPState()
	}
	out := appapi.MCPSurface{
		Servers: make([]appapi.MCPServerInfo, 0, len(state.Servers)),
		Routes:  make([]appapi.MCPRouteInfo, 0, len(state.Routes)),
	}
	for _, srv := range state.Servers {
		out.Servers = append(out.Servers, appapi.MCPServerInfo{
			Name:         srv.Name,
			Enabled:      srv.Enabled,
			Transport:    srv.Transport,
			Auth:         srv.Auth,
			Provider:     srv.Provider,
			URL:          srv.URL,
			Command:      srv.Command,
			Accounts:     srv.Accounts,
			Sessions:     srv.Sessions,
			Calls:        srv.Calls,
			Errors:       srv.Errors,
			AvgLatencyMs: srv.AvgLatencyMs,
		})
	}
	sort.Slice(out.Servers, func(i, j int) bool { return out.Servers[i].Name < out.Servers[j].Name })
	for _, route := range state.Routes {
		info := appapi.MCPRouteInfo{
			Name:         route.Name,
			Enabled:      route.Enabled,
			Targets:      make([]appapi.MCPRouteTargetInfo, 0, len(route.Targets)),
			Sessions:     route.Sessions,
			Calls:        route.Calls,
			Errors:       route.Errors,
			AvgLatencyMs: route.AvgLatencyMs,
		}
		for _, t := range route.Targets {
			info.Targets = append(info.Targets, appapi.MCPRouteTargetInfo{Server: t.Server, Tools: t.Tools})
		}
		out.Routes = append(out.Routes, info)
	}
	sort.Slice(out.Routes, func(i, j int) bool { return out.Routes[i].Name < out.Routes[j].Name })
	return out
}
