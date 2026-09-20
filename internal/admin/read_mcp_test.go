package admin

import (
	"errors"
	"testing"

	"model-proxy/internal/appapi"
	observestats "model-proxy/internal/observe/stats"
)

// TestMCPAnalyticsProjection pins the admin-side aggregation and filtering:
// rows group by exposed name (the store kind is not surfaced), tool rows group
// by (name, tool), totals are calls/errors sums with a calls-weighted average
// latency and max last_call_at, and name/tool filters pass to the port.
func TestMCPAnalyticsProjection(t *testing.T) {
	fixed := int64(1726780800)
	service := New(Ports{
		MCPAnalytics: func(from, to int64, granularity, name, tool string) ([]observestats.MCPBucketRow, []observestats.MCPToolBucketRow, error) {
			if from != fixed || to != fixed+7200 || granularity != "day" {
				t.Fatalf("unexpected query from=%d to=%d granularity=%q", from, to, granularity)
			}
			rows := []observestats.MCPBucketRow{
				{Name: "web-search", Kind: "server", Bucket: fixed, Calls: 2, Errors: 1, LatencyMsSum: 200, LastCallAt: fixed + 10, AvgLatencyMs: 100},
				{Name: "web-search", Kind: "server", Bucket: fixed + 3600, Calls: 4, Errors: 0, LatencyMsSum: 800, LastCallAt: fixed + 3700, AvgLatencyMs: 200},
				{Name: "search-route", Kind: "route", Bucket: fixed, Calls: 5, Errors: 2, LatencyMsSum: 1000, LastCallAt: fixed + 30, AvgLatencyMs: 200},
			}
			if name != "" {
				filtered := make([]observestats.MCPBucketRow, 0, len(rows))
				for _, r := range rows {
					if r.Name == name {
						filtered = append(filtered, r)
					}
				}
				rows = filtered
			}
			toolRows := []observestats.MCPToolBucketRow{
				{Name: "web-search", Tool: "search", Kind: "server", Bucket: fixed, Calls: 3, Errors: 1, LatencyMsSum: 300, LastCallAt: fixed + 20, AvgLatencyMs: 100},
				{Name: "web-search", Tool: "search", Kind: "server", Bucket: fixed + 3600, Calls: 1, Errors: 0, LatencyMsSum: 500, LastCallAt: fixed + 3600, AvgLatencyMs: 500},
				{Name: "web-search", Tool: "fetch", Kind: "server", Bucket: fixed, Calls: 2, Errors: 0, LatencyMsSum: 200, LastCallAt: fixed + 40, AvgLatencyMs: 100},
				{Name: "search-route", Tool: "lookup", Kind: "route", Bucket: fixed, Calls: 5, Errors: 2, LatencyMsSum: 1000, LastCallAt: fixed + 30, AvgLatencyMs: 200},
			}
			if name != "" || tool != "" {
				filtered := make([]observestats.MCPToolBucketRow, 0, len(toolRows))
				for _, r := range toolRows {
					if name != "" && r.Name != name {
						continue
					}
					if tool != "" && r.Tool != tool {
						continue
					}
					filtered = append(filtered, r)
				}
				toolRows = filtered
			}
			return rows, toolRows, nil
		},
	})

	result, err := service.MCPAnalytics(appapi.MCPAnalyticsQuery{From: fixed, To: fixed + 7200, Granularity: "day"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	byName := map[string]appapi.MCPAnalyticsSeries{}
	for _, s := range result.Series {
		byName[s.Name] = s
	}
	if len(byName) != 2 {
		t.Fatalf("series = %+v", result.Series)
	}
	ws := byName["web-search"]
	if len(ws.Points) != 2 || ws.Totals.Calls != 6 || ws.Totals.Errors != 1 || ws.Totals.LastCallAt != fixed+3700 {
		t.Errorf("web-search = %+v", ws)
	}
	if ws.Totals.AvgLatencyMs != 1000.0/6.0 {
		t.Errorf("web-search avg latency = %v, want %v", ws.Totals.AvgLatencyMs, 1000.0/6.0)
	}
	rt := byName["search-route"]
	if len(rt.Points) != 1 || rt.Totals.Calls != 5 || rt.Totals.Errors != 2 || rt.Totals.LastCallAt != fixed+30 {
		t.Errorf("search-route = %+v", rt)
	}

	tools := map[[2]string]appapi.MCPToolAnalyticsSeries{}
	for _, s := range result.ToolSeries {
		tools[[2]string{s.Name, s.Tool}] = s
	}
	if len(tools) != 3 {
		t.Fatalf("tool_series = %+v", result.ToolSeries)
	}
	search := tools[[2]string{"web-search", "search"}]
	if len(search.Points) != 2 || search.Totals.Calls != 4 || search.Totals.Errors != 1 || search.Totals.LastCallAt != fixed+3600 {
		t.Errorf("web-search/search = %+v", search)
	}
	if search.Totals.AvgLatencyMs != 800.0/4.0 {
		t.Errorf("web-search/search avg latency = %v, want %v", search.Totals.AvgLatencyMs, 800.0/4.0)
	}
	if fetch := tools[[2]string{"web-search", "fetch"}]; fetch.Totals.Calls != 2 || fetch.Totals.LastCallAt != fixed+40 {
		t.Errorf("web-search/fetch = %+v", fetch)
	}
	if lookup := tools[[2]string{"search-route", "lookup"}]; lookup.Totals.Calls != 5 || lookup.Totals.Errors != 2 {
		t.Errorf("search-route/lookup = %+v", lookup)
	}

	// Name and tool filters reach the port.
	filtered, err := service.MCPAnalytics(appapi.MCPAnalyticsQuery{From: fixed, To: fixed + 7200, Granularity: "day", Name: "web-search", Tool: "fetch"})
	if err != nil {
		t.Fatalf("filtered query error: %v", err)
	}
	if len(filtered.Series) != 1 || filtered.Series[0].Name != "web-search" {
		t.Fatalf("filtered series = %+v", filtered.Series)
	}
	if len(filtered.ToolSeries) != 1 || filtered.ToolSeries[0].Tool != "fetch" {
		t.Fatalf("filtered tool_series = %+v", filtered.ToolSeries)
	}

	// Missing port behaves like a disabled store.
	disabled := New(Ports{})
	empty, err := disabled.MCPAnalytics(appapi.MCPAnalyticsQuery{From: fixed, To: fixed + 7200, Granularity: "day"})
	if err != nil {
		t.Fatalf("disabled store error: %v", err)
	}
	if len(empty.Series) != 0 || len(empty.ToolSeries) != 0 {
		t.Errorf("disabled store result = %+v", empty)
	}

	// Port errors pass through.
	errService := New(Ports{
		MCPAnalytics: func(int64, int64, string, string, string) ([]observestats.MCPBucketRow, []observestats.MCPToolBucketRow, error) {
			return nil, nil, errors.New("store down")
		},
	})
	_, err = errService.MCPAnalytics(appapi.MCPAnalyticsQuery{From: fixed, To: fixed + 7200, Granularity: "day"})
	if err == nil || err.Error() != "store down" {
		t.Fatalf("port error = %v", err)
	}
}
