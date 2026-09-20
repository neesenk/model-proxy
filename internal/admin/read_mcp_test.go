package admin

import (
	"errors"
	"testing"

	"model-proxy/internal/appapi"
	observestats "model-proxy/internal/observe/stats"
)

// TestMCPAnalyticsProjection pins the admin-side aggregation and filtering:
// rows group by (kind, name), totals are calls/errors sums with a calls-
// weighted average latency and max last_call_at, and invalid kinds are 400s.
func TestMCPAnalyticsProjection(t *testing.T) {
	fixed := int64(1726780800)
	service := New(Ports{
		MCPAnalytics: func(from, to int64, granularity, name, kind string) ([]observestats.MCPBucketRow, error) {
			if from != fixed || to != fixed+7200 || granularity != "day" {
				t.Fatalf("unexpected query from=%d to=%d granularity=%q", from, to, granularity)
			}
			rows := []observestats.MCPBucketRow{
				{Name: "web-search", Kind: "server", Bucket: fixed, Calls: 2, Errors: 1, LatencyMsSum: 200, LastCallAt: fixed + 10, AvgLatencyMs: 100},
				{Name: "web-search", Kind: "server", Bucket: fixed + 3600, Calls: 4, Errors: 0, LatencyMsSum: 800, LastCallAt: fixed + 3700, AvgLatencyMs: 200},
				{Name: "search-route", Kind: "route", Bucket: fixed, Calls: 5, Errors: 2, LatencyMsSum: 1000, LastCallAt: fixed + 30, AvgLatencyMs: 200},
			}
			if name != "" || kind != "" {
				filtered := make([]observestats.MCPBucketRow, 0, len(rows))
				for _, r := range rows {
					if name != "" && r.Name != name {
						continue
					}
					if kind != "" && r.Kind != kind {
						continue
					}
					filtered = append(filtered, r)
				}
				return filtered, nil
			}
			return rows, nil
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
	if ws.Kind != "server" || len(ws.Points) != 2 || ws.Totals.Calls != 6 || ws.Totals.Errors != 1 || ws.Totals.LastCallAt != fixed+3700 {
		t.Errorf("web-search = %+v", ws)
	}
	if ws.Totals.AvgLatencyMs != 1000.0/6.0 {
		t.Errorf("web-search avg latency = %v, want %v", ws.Totals.AvgLatencyMs, 1000.0/6.0)
	}
	rt := byName["search-route"]
	if rt.Kind != "route" || len(rt.Points) != 1 || rt.Totals.Calls != 5 || rt.Totals.Errors != 2 || rt.Totals.LastCallAt != fixed+30 {
		t.Errorf("search-route = %+v", rt)
	}

	// Invalid kind is a 400-class error.
	_, err = service.MCPAnalytics(appapi.MCPAnalyticsQuery{Kind: "tool"})
	if err == nil {
		t.Fatal("expected error for invalid kind")
	}
	var httpErr *appapi.HTTPError
	if !errors.As(err, &httpErr) || httpErr.Status != 400 {
		t.Fatalf("invalid kind error = %v", err)
	}

	// Missing port behaves like a disabled store.
	disabled := New(Ports{})
	empty, err := disabled.MCPAnalytics(appapi.MCPAnalyticsQuery{From: fixed, To: fixed + 7200, Granularity: "day"})
	if err != nil {
		t.Fatalf("disabled store error: %v", err)
	}
	if len(empty.Series) != 0 {
		t.Errorf("disabled store series = %+v", empty.Series)
	}

	// Port errors pass through.
	errService := New(Ports{
		MCPAnalytics: func(int64, int64, string, string, string) ([]observestats.MCPBucketRow, error) {
			return nil, errors.New("store down")
		},
	})
	_, err = errService.MCPAnalytics(appapi.MCPAnalyticsQuery{From: fixed, To: fixed + 7200, Granularity: "day"})
	if err == nil || err.Error() != "store down" {
		t.Fatalf("port error = %v", err)
	}
}
