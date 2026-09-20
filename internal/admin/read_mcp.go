package admin

import (
	"net/http"
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

// MCPAnalytics projects persisted MCP usage buckets for the
// /api/mcp/analytics endpoint. Kind is validated here so an unknown value is a
// client error; the store's QueryMCPBuckets validates granularity.
func (s *Service) MCPAnalytics(query appapi.MCPAnalyticsQuery) (appapi.MCPAnalyticsResult, error) {
	result := appapi.MCPAnalyticsResult{
		Granularity: query.Granularity,
		From:        query.From,
		To:          query.To,
		Series:      []appapi.MCPAnalyticsSeries{},
	}
	if query.Kind != "" && query.Kind != "server" && query.Kind != "route" {
		return result, appapi.NewHTTPError(http.StatusBadRequest, "kind must be server or route")
	}
	if s.ports.MCPAnalytics == nil {
		return result, nil
	}
	rows, err := s.ports.MCPAnalytics(query.From, query.To, query.Granularity, query.Name, query.Kind)
	if err != nil {
		return result, err
	}
	// Group rows by (kind, name), preserving a stable encounter order.
	type key struct{ kind, name string }
	type group struct {
		points []appapi.MCPAnalyticsPoint
		calls  uint64
		errors uint64
		// latencyMsSum accumulates the bucket's raw latency sum for a precise
		// calls-weighted series average.
		latencyMsSum uint64
		lastCallAt   int64
	}
	groups := map[key]*group{}
	order := []key{}
	for _, r := range rows {
		k := key{kind: r.Kind, name: r.Name}
		g, ok := groups[k]
		if !ok {
			g = &group{points: []appapi.MCPAnalyticsPoint{}}
			groups[k] = g
			order = append(order, k)
		}
		g.points = append(g.points, appapi.MCPAnalyticsPoint{
			Ts:           r.Bucket,
			Calls:        r.Calls,
			Errors:       r.Errors,
			AvgLatencyMs: r.AvgLatencyMs,
		})
		g.calls += r.Calls
		g.errors += r.Errors
		g.latencyMsSum += r.LatencyMsSum
		if r.LastCallAt > g.lastCallAt {
			g.lastCallAt = r.LastCallAt
		}
	}
	for _, k := range order {
		g := groups[k]
		avgLatency := 0.0
		if g.calls > 0 {
			avgLatency = float64(g.latencyMsSum) / float64(g.calls)
		}
		result.Series = append(result.Series, appapi.MCPAnalyticsSeries{
			Kind:   k.kind,
			Name:   k.name,
			Points: g.points,
			Totals: appapi.MCPAnalyticsTotals{
				Calls:        g.calls,
				Errors:       g.errors,
				AvgLatencyMs: avgLatency,
				LastCallAt:   g.lastCallAt,
			},
		})
	}
	return result, nil
}
