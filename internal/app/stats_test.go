package app

import (
	"encoding/json"
	"fmt"
	"model-proxy/internal/appapi"
	obscounters "model-proxy/internal/observe/counters"
	observestats "model-proxy/internal/observe/stats"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

// ---- stats_http_test.go ----

// TestAPITokensWindowSelector: ?window= switches /api/tokens from the
// cumulative hot counters to the persisted minute buckets, for both the usage
// rows and the nested agent breakdown; the default keeps the hot counters.
func TestAPITokensWindowSelector(t *testing.T) {
	minute := time.Now().Unix() / 60 * 60
	p := &Proxy{
		processServices: processServices{
			metrics: obscounters.NewMetricsStore(),
			tokens:  obscounters.NewTokenCounter(),
			agents:  obscounters.NewAgentCounter(),
			stats:   newTestStatsStore(t),
		},
	}
	// Persisted history: one bucket inside the 1h window, one far outside it.
	if err := p.stats.Flush(minute, map[observestats.Key]observestats.Counters{
		{Provider: "zhipu", Model: "glm-5"}: {Input: 100, Output: 20, CacheCreation: 5, CacheRead: 6, TokenRequests: 4},
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.stats.Flush(minute-2*3600, map[observestats.Key]observestats.Counters{
		{Provider: "zhipu", Model: "glm-5"}: {Input: 900, Output: 90, TokenRequests: 9},
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.stats.FlushAgents(minute, map[observestats.AgentKey]observestats.AgentCounters{
		{Agent: "codex", Provider: "zhipu", Model: "glm-5"}: {Requests: 4, Input: 100, Output: 20, CacheRead: 6},
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.stats.FlushAgents(minute-2*3600, map[observestats.AgentKey]observestats.AgentCounters{
		{Agent: "codex", Provider: "zhipu", Model: "glm-5"}: {Requests: 9, Input: 900, Output: 90},
	}); err != nil {
		t.Fatal(err)
	}
	// The hot counters deliberately differ so the two views are distinguishable.
	p.tokens.Commit(obscounters.TokenKey{Provider: "zhipu", Model: "glm-5"}, obscounters.TokenUsage{Input: 7, Output: 3})
	p.agents.AddTokens("codex", "zhipu", "glm-5", obscounters.TokenUsage{Input: 7, Output: 3})

	mux := http.NewServeMux()
	NewWebServer(p, "test-config.yaml").Register(mux)
	get := func(path string) (int, struct {
		Usage  []appapi.TokenUsage `json:"usage"`
		Agents []appapi.AgentUsage `json:"agents"`
		Window string              `json:"window"`
	}) {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		var body struct {
			Usage  []appapi.TokenUsage `json:"usage"`
			Agents []appapi.AgentUsage `json:"agents"`
			Window string              `json:"window"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %s: %v; body=%s", path, err, rec.Body.String())
		}
		return rec.Code, body
	}

	code, all := get("/api/tokens")
	if code != http.StatusOK || all.Window != "all" {
		t.Fatalf("default /api/tokens = (%d, window %q)", code, all.Window)
	}
	if len(all.Usage) != 1 || all.Usage[0].Input != 7 {
		t.Errorf("default usage = %+v, want the hot counters (input 7)", all.Usage)
	}

	code, windowed := get("/api/tokens?window=1h")
	if code != http.StatusOK || windowed.Window != "1h" {
		t.Fatalf("windowed /api/tokens = (%d, window %q)", code, windowed.Window)
	}
	wantUsage := appapi.TokenUsage{
		Provider: "zhipu", Model: "glm-5",
		Input: 100, Output: 20, CacheCreation: 5, CacheRead: 6, Total: 131, Requests: 4,
	}
	if len(windowed.Usage) != 1 || windowed.Usage[0] != wantUsage {
		t.Errorf("windowed usage = %+v, want [%+v] (old bucket excluded)", windowed.Usage, wantUsage)
	}
	wantAgents := []appapi.AgentUsage{{
		Agent: "codex", Requests: 4, Input: 100, Output: 20, CacheRead: 6, Total: 126,
		Models: []appapi.AgentModelUsage{
			{Provider: "zhipu", Model: "glm-5", Requests: 4, Input: 100, Output: 20, CacheRead: 6, Total: 126},
		},
	}}
	if !reflect.DeepEqual(windowed.Agents, wantAgents) {
		t.Errorf("windowed agents = %+v, want %+v", windowed.Agents, wantAgents)
	}

	// A window covering both buckets aggregates them (boundary inclusion).
	_, wide := get("/api/tokens?window=24h")
	if len(wide.Usage) != 1 || wide.Usage[0].Input != 1000 || wide.Usage[0].Requests != 13 {
		t.Errorf("24h usage = %+v, want both buckets (input 1000, requests 13)", wide.Usage)
	}
	if rec := httptest.NewRecorder(); true {
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tokens?window=bogus", nil))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("bogus window status = %d, want 400; body=%s", rec.Code, rec.Body.String())
		}
	}
}

// TestAPITokensClosedRange: from/to gives a closed range (the 昨天/yesterday
// preset's shape) — buckets are excluded on BOTH sides, for the usage rows
// and the agent breakdown alike.
func TestAPITokensClosedRange(t *testing.T) {
	now := time.Now()
	midnight := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	yesterdayNoon := midnight.Add(-12*time.Hour).Unix() / 60 * 60
	todayMinute := midnight.Add(12*time.Hour).Unix() / 60 * 60
	twoDaysAgo := midnight.Add(-36*time.Hour).Unix() / 60 * 60
	from := midnight.Add(-24 * time.Hour).Unix() // yesterday 00:00 local
	to := midnight.Unix() - 1                    // yesterday 23:59:59 local

	p := &Proxy{
		processServices: processServices{
			metrics: obscounters.NewMetricsStore(),
			tokens:  obscounters.NewTokenCounter(),
			agents:  obscounters.NewAgentCounter(),
			stats:   newTestStatsStore(t),
		},
	}
	flush := func(minute int64, input uint64) {
		t.Helper()
		if err := p.stats.Flush(minute, map[observestats.Key]observestats.Counters{
			{Provider: "zhipu", Model: "glm-5"}: {Input: input, Output: 1, CacheRead: 2, TokenRequests: 1},
		}); err != nil {
			t.Fatal(err)
		}
		if err := p.stats.FlushAgents(minute, map[observestats.AgentKey]observestats.AgentCounters{
			{Agent: "codex", Provider: "zhipu", Model: "glm-5"}: {Requests: 1, Input: input, Output: 1, CacheRead: 2},
		}); err != nil {
			t.Fatal(err)
		}
	}
	flush(twoDaysAgo, 900)    // before from — excluded
	flush(yesterdayNoon, 100) // inside the closed range
	flush(todayMinute, 500)   // after to — excluded

	mux := http.NewServeMux()
	NewWebServer(p, "test-config.yaml").Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
		fmt.Sprintf("/api/tokens?from=%d&to=%d", from, to), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Usage  []appapi.TokenUsage `json:"usage"`
		Agents []appapi.AgentUsage `json:"agents"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	wantUsage := appapi.TokenUsage{
		Provider: "zhipu", Model: "glm-5",
		Input: 100, Output: 1, CacheRead: 2, Total: 103, Requests: 1,
	}
	if len(body.Usage) != 1 || body.Usage[0] != wantUsage {
		t.Errorf("closed-range usage = %+v, want [%+v] (both sides excluded)", body.Usage, wantUsage)
	}
	wantAgents := []appapi.AgentUsage{{
		Agent: "codex", Requests: 1, Input: 100, Output: 1, CacheRead: 2, Total: 103,
		Models: []appapi.AgentModelUsage{
			{Provider: "zhipu", Model: "glm-5", Requests: 1, Input: 100, Output: 1, CacheRead: 2, Total: 103},
		},
	}}
	if !reflect.DeepEqual(body.Agents, wantAgents) {
		t.Errorf("closed-range agents = %+v, want %+v", body.Agents, wantAgents)
	}
}
func TestAPIStatsHandler(t *testing.T) {
	p := &Proxy{
		processServices: processServices{
			metrics: obscounters.NewMetricsStore(),
			tokens:  obscounters.NewTokenCounter(),
			stats:   newTestStatsStore(t),
		},
	}
	minute := time.Now().Unix() / 60 * 60
	counters := observestats.Counters{
		Requests: 4, Failovers: 1, RateLimited429: 2, Failures: 3,
		Input: 100, Output: 20, CacheCreation: 5, CacheRead: 6,
		TokenRequests: 4, LastRequestAt: minute + 5,
		LatencySum: 1000, TTFTSum: 200,
	}
	if err := p.stats.Flush(minute, map[observestats.Key]observestats.Counters{
		{Provider: "zhipu", Model: "glm-5"}: counters,
		{Provider: "other", Model: "other"}: {Requests: 99},
	}); err != nil {
		t.Fatal(err)
	}
	w := NewWebServer(p, "test-config.yaml")
	mux := http.NewServeMux()
	w.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(
		http.MethodGet,
		fmt.Sprintf(
			"/api/stats?from=%d&to=%d&provider=zhipu&model=glm-5",
			minute,
			minute,
		),
		nil,
	))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	var response struct {
		From    int64                 `json:"from"`
		To      int64                 `json:"to"`
		Bucket  int64                 `json:"bucket"`
		Buckets []observestats.Bucket `json:"buckets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
	if response.Bucket != 60 || response.From > minute || response.To < minute {
		t.Errorf("range envelope = %+v, want bucket=60 containing minute %d", response, minute)
	}
	if len(response.Buckets) != 1 {
		t.Fatalf("buckets = %d, want exactly 1: %+v", len(response.Buckets), response.Buckets)
	}
	got := response.Buckets[0]
	if got.Provider != "zhipu" || got.Model != "glm-5" || got.Minute != minute ||
		got.Requests != counters.Requests || got.Failovers != counters.Failovers ||
		got.RateLimited429 != counters.RateLimited429 || got.Failures != counters.Failures ||
		got.Input != counters.Input || got.Output != counters.Output ||
		got.CacheCreation != counters.CacheCreation || got.CacheRead != counters.CacheRead ||
		got.TokenRequests != counters.TokenRequests || got.LastRequestAt != counters.LastRequestAt ||
		got.LatencySum != counters.LatencySum || got.TTFTSum != counters.TTFTSum ||
		got.AvgLatencyMs != 250 || got.AvgTtftMs != 50 {
		t.Errorf("bucket = %+v, want exact persisted fields and 250/50 averages", got)
	}

	// ?bucket=10m aggregates the same row and floors its display bucket.
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, httptest.NewRequest(
		http.MethodGet,
		fmt.Sprintf(
			"/api/stats?from=%d&to=%d&provider=zhipu&model=glm-5&bucket=10m",
			minute,
			minute,
		),
		nil,
	))
	if rec2.Code != http.StatusOK {
		t.Fatalf("bucket status=%d want 200", rec2.Code)
	}
	var aggregated struct {
		Bucket  int64                 `json:"bucket"`
		Buckets []observestats.Bucket `json:"buckets"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &aggregated); err != nil {
		t.Fatal(err)
	}
	if aggregated.Bucket != 600 || len(aggregated.Buckets) != 1 ||
		aggregated.Buckets[0].Minute != minute/600*600 ||
		aggregated.Buckets[0].Requests != counters.Requests {
		t.Errorf("bucket=10m response = %+v", aggregated)
	}

	nilMux := http.NewServeMux()
	NewWebServer(&Proxy{
		processServices: processServices{
			metrics: obscounters.NewMetricsStore(),
			tokens:  obscounters.NewTokenCounter(),
		},
	}, "test-config.yaml").Register(nilMux)
	nilRecorder := httptest.NewRecorder()
	nilMux.ServeHTTP(nilRecorder, httptest.NewRequest(http.MethodGet, "/api/stats", nil))
	if nilRecorder.Code != http.StatusOK {
		t.Fatalf("nil store status=%d want 200; body=%s", nilRecorder.Code, nilRecorder.Body.String())
	}
	var nilResponse struct {
		Buckets []observestats.Bucket `json:"buckets"`
	}
	if err := json.Unmarshal(nilRecorder.Body.Bytes(), &nilResponse); err != nil {
		t.Fatal(err)
	}
	if nilResponse.Buckets == nil || len(nilResponse.Buckets) != 0 {
		t.Errorf("nil store buckets = %#v, want a non-nil empty JSON array", nilResponse.Buckets)
	}

	closedStore := newTestStatsStore(t)
	if err := closedStore.Close(); err != nil {
		t.Fatal(err)
	}
	closedMux := http.NewServeMux()
	NewWebServer(&Proxy{
		processServices: processServices{
			metrics: obscounters.NewMetricsStore(),
			tokens:  obscounters.NewTokenCounter(),
			stats:   closedStore,
		},
	}, "test-config.yaml").Register(closedMux)
	closedRecorder := httptest.NewRecorder()
	closedMux.ServeHTTP(closedRecorder, httptest.NewRequest(http.MethodGet, "/api/stats", nil))
	if closedRecorder.Code != http.StatusInternalServerError {
		t.Errorf("closed store status=%d want 500; body=%s",
			closedRecorder.Code, closedRecorder.Body.String())
	}
}

func TestAPIAgentsHandlerFiltersAggregatesAndReportsStoreStates(t *testing.T) {
	base := time.Now().Unix() / 120 * 120
	store := newTestStatsStore(t)
	if err := store.FlushAgents(base, map[observestats.AgentKey]observestats.AgentCounters{
		{Agent: "codex", Provider: "zhipu", Model: "glm-5"}: {
			Requests: 2, Input: 10, Output: 5, LatencySum: 200, Failures: 1,
		},
		{Agent: "other", Provider: "zhipu", Model: "glm-5"}: {
			Requests: 99, Input: 999,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FlushAgents(base+60, map[observestats.AgentKey]observestats.AgentCounters{
		{Agent: "codex", Provider: "zhipu", Model: "glm-5"}: {
			Requests: 3, Input: 20, Output: 7, LatencySum: 400, Failures: 2,
		},
	}); err != nil {
		t.Fatal(err)
	}

	proxy := &Proxy{
		processServices: processServices{
			metrics: obscounters.NewMetricsStore(),
			tokens:  obscounters.NewTokenCounter(),
			stats:   store,
		},
	}
	mux := http.NewServeMux()
	NewWebServer(proxy, "test-config.yaml").Register(mux)
	url := fmt.Sprintf(
		"/api/agents?from=%d&to=%d&agent=codex&provider=zhipu&model=glm-5&bucket=2m",
		base, base+60,
	)
	recorder := httptest.NewRecorder()
	mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, url, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		From    int64                      `json:"from"`
		To      int64                      `json:"to"`
		Bucket  int64                      `json:"bucket"`
		Buckets []observestats.AgentBucket `json:"buckets"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, recorder.Body.String())
	}
	if response.From != base || response.To != base+60 || response.Bucket != 120 {
		t.Errorf("range envelope = %+v, want from=%d to=%d bucket=120", response, base, base+60)
	}
	if len(response.Buckets) != 1 {
		t.Fatalf("buckets = %+v, want one filtered aggregate", response.Buckets)
	}
	got := response.Buckets[0]
	want := observestats.AgentBucket{
		Agent: "codex", Provider: "zhipu", Model: "glm-5", Minute: base,
		Requests: 5, Input: 30, Output: 12, LatencySum: 600, Failures: 3,
	}
	if got != want {
		t.Errorf("agent aggregate = %+v, want %+v", got, want)
	}

	nilMux := http.NewServeMux()
	NewWebServer(&Proxy{
		processServices: processServices{
			metrics: obscounters.NewMetricsStore(),
			tokens:  obscounters.NewTokenCounter(),
		},
	}, "test-config.yaml").Register(nilMux)
	nilRecorder := httptest.NewRecorder()
	nilMux.ServeHTTP(nilRecorder, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
	if nilRecorder.Code != http.StatusOK {
		t.Fatalf("nil store status=%d want 200; body=%s", nilRecorder.Code, nilRecorder.Body.String())
	}
	var nilResponse struct {
		Buckets []observestats.AgentBucket `json:"buckets"`
	}
	if err := json.Unmarshal(nilRecorder.Body.Bytes(), &nilResponse); err != nil {
		t.Fatal(err)
	}
	if nilResponse.Buckets == nil || len(nilResponse.Buckets) != 0 {
		t.Errorf("nil store buckets = %#v, want a non-nil empty JSON array", nilResponse.Buckets)
	}

	closedStore, err := observestats.Open(observestats.Options{
		Path: fmt.Sprintf("%s/closed.db", t.TempDir()),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := closedStore.Close(); err != nil {
		t.Fatal(err)
	}
	closedMux := http.NewServeMux()
	NewWebServer(&Proxy{
		processServices: processServices{
			metrics: obscounters.NewMetricsStore(),
			tokens:  obscounters.NewTokenCounter(),
			stats:   closedStore,
		},
	}, "test-config.yaml").Register(closedMux)
	closedRecorder := httptest.NewRecorder()
	closedMux.ServeHTTP(closedRecorder, httptest.NewRequest(http.MethodGet, "/api/agents", nil))
	if closedRecorder.Code != http.StatusInternalServerError {
		t.Errorf("closed store status=%d want 500; body=%s",
			closedRecorder.Code, closedRecorder.Body.String())
	}
}

func TestAPIAnalyticsHandler(t *testing.T) {
	p := &Proxy{
		processServices: processServices{
			metrics: obscounters.NewMetricsStore(),
			tokens:  obscounters.NewTokenCounter(),
			stats:   newTestStatsStore(t),
			// pricing: nil → resolver falls back to unpriced (cost null), proving the
			// handler never fabricates a price and never panics on a nil catalog.
		},
	}
	minute := time.Now().Unix() / 60 * 60
	if err := p.stats.Flush(minute, map[observestats.Key]observestats.Counters{
		{Provider: "deepseek", Model: "deepseek-v4-pro"}: {
			Requests: 3, Input: 1000, Output: 200,
		},
		{Provider: "other", Model: "other"}: {Requests: 99},
		// Virtual counter namespaces share the (provider, model) key space but
		// are not upstream usage; the projection must drop them.
		{Provider: "guard", Model: "ssh"}:        {Requests: 9},
		{Provider: "attempts", Model: "ok"}:      {Requests: 9},
		{Provider: "routing", Model: "decision"}: {Requests: 9},
	}); err != nil {
		t.Fatal(err)
	}
	w := NewWebServer(p, "test-config.yaml")
	mux := http.NewServeMux()
	w.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(
		http.MethodGet,
		fmt.Sprintf(
			"/api/analytics?from=%d&to=%d&provider=deepseek&model=deepseek-v4-pro&granularity=day",
			minute,
			minute,
		),
		nil,
	))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Granularity string `json:"granularity"`
		Series      []struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
			Points   []struct {
				Requests uint64   `json:"requests"`
				Input    uint64   `json:"input"`
				Output   uint64   `json:"output"`
				Cost     *float64 `json:"cost"`
				Priced   bool     `json:"priced"`
			} `json:"points"`
		} `json:"series"`
		PriceCoverage struct {
			Priced   []string `json:"priced"`
			Unpriced []string `json:"unpriced"`
		} `json:"price_coverage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal analytics: %v\n%s", err, rec.Body.String())
	}
	if got.Granularity != "day" {
		t.Errorf("granularity = %q, want day", got.Granularity)
	}
	if len(got.Series) != 1 ||
		got.Series[0].Provider != "deepseek" ||
		got.Series[0].Model != "deepseek-v4-pro" {
		t.Fatalf("series = %+v, want one deepseek/deepseek-v4-pro", got.Series)
	}
	points := got.Series[0].Points
	if len(points) != 1 ||
		points[0].Requests != 3 ||
		points[0].Input != 1000 ||
		points[0].Output != 200 {
		t.Errorf("point = %+v, want reqs=3 input=1000 output=200", points)
	}
	// Never-fabricate: no catalog + no override on the test Proxy → unpriced.
	if points[0].Priced {
		t.Error("point must be unpriced without a catalog")
	}
	if points[0].Cost != nil {
		t.Errorf("unpriced point cost must be nil, got %v", *points[0].Cost)
	}
	found := false
	for _, model := range got.PriceCoverage.Unpriced {
		found = found || model == "deepseek-v4-pro"
	}
	if !found {
		t.Errorf("price_coverage.unpriced must list deepseek-v4-pro: %+v",
			got.PriceCoverage.Unpriced)
	}
	if len(got.PriceCoverage.Priced) != 0 {
		t.Errorf("price_coverage.priced must be empty, got %+v", got.PriceCoverage.Priced)
	}

	// Unfiltered query: the virtual namespaces (guard/attempts/routing) must not
	// surface as series or as unpriced "models".
	all := httptest.NewRecorder()
	mux.ServeHTTP(all, httptest.NewRequest(
		http.MethodGet,
		fmt.Sprintf("/api/analytics?from=%d&to=%d&granularity=day", minute, minute),
		nil,
	))
	if all.Code != http.StatusOK {
		t.Fatalf("unfiltered status=%d want 200; body=%s", all.Code, all.Body.String())
	}
	var allResp struct {
		Series []struct {
			Provider string `json:"provider"`
		} `json:"series"`
		PriceCoverage struct {
			Unpriced []string `json:"unpriced"`
		} `json:"price_coverage"`
	}
	if err := json.Unmarshal(all.Body.Bytes(), &allResp); err != nil {
		t.Fatalf("unmarshal unfiltered analytics: %v\n%s", err, all.Body.String())
	}
	for _, s := range allResp.Series {
		if obscounters.IsVirtualProvider(s.Provider) {
			t.Errorf("virtual provider %q leaked into analytics series", s.Provider)
		}
	}
	for _, m := range allResp.PriceCoverage.Unpriced {
		if m == "ssh" || m == "ok" || m == "decision" {
			t.Errorf("virtual model %q leaked into price_coverage.unpriced", m)
		}
	}

	bad := httptest.NewRecorder()
	mux.ServeHTTP(bad, httptest.NewRequest("GET", "/api/analytics?granularity=hour", nil))
	if bad.Code != http.StatusBadRequest {
		t.Errorf("bad granularity status=%d want 400", bad.Code)
	}

	nilMux := http.NewServeMux()
	NewWebServer(&Proxy{
		processServices: processServices{
			metrics: obscounters.NewMetricsStore(),
			tokens:  obscounters.NewTokenCounter(),
		},
	}, "test-config.yaml").Register(nilMux)
	nilRecorder := httptest.NewRecorder()
	nilMux.ServeHTTP(nilRecorder, httptest.NewRequest(http.MethodGet, "/api/analytics", nil))
	if nilRecorder.Code != http.StatusOK {
		t.Fatalf("nil store status=%d want 200; body=%s", nilRecorder.Code, nilRecorder.Body.String())
	}
	var nilResponse struct {
		Series []json.RawMessage `json:"series"`
	}
	if err := json.Unmarshal(nilRecorder.Body.Bytes(), &nilResponse); err != nil {
		t.Fatal(err)
	}
	if nilResponse.Series == nil || len(nilResponse.Series) != 0 {
		t.Errorf("nil store series = %#v, want a non-nil empty JSON array", nilResponse.Series)
	}

	closedStore := newTestStatsStore(t)
	if err := closedStore.Close(); err != nil {
		t.Fatal(err)
	}
	closedMux := http.NewServeMux()
	NewWebServer(&Proxy{
		processServices: processServices{
			metrics: obscounters.NewMetricsStore(),
			tokens:  obscounters.NewTokenCounter(),
			stats:   closedStore,
		},
	}, "test-config.yaml").Register(closedMux)
	closedRecorder := httptest.NewRecorder()
	closedMux.ServeHTTP(
		closedRecorder,
		httptest.NewRequest(http.MethodGet, "/api/analytics", nil),
	)
	if closedRecorder.Code != http.StatusInternalServerError {
		t.Errorf("closed store status=%d want 500; body=%s",
			closedRecorder.Code, closedRecorder.Body.String())
	}
}

func TestAPIAnalyticsUsesCatalogThenDetachedOverride(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var catalogRequests atomic.Int32
	priceServer := httptest.NewServer(http.HandlerFunc(func(
		response http.ResponseWriter,
		request *http.Request,
	) {
		catalogRequests.Add(1)
		response.Header().Set("ETag", `"prices-v1"`)
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write([]byte(pricingIntegrationFixture))
	}))
	defer priceServer.Close()

	p := &Proxy{
		generationState: generationState{
			cfg: &Config{Pricing: PricingConfig{
				Enabled:   true,
				TTL:       "24h",
				SourceURL: priceServer.URL,
			}},
		},
		processServices: processServices{
			metrics: obscounters.NewMetricsStore(),
			tokens:  obscounters.NewTokenCounter(),
			stats:   newTestStatsStore(t),
		},
	}
	minute := time.Now().Unix() / 60 * 60
	if err := p.stats.Flush(minute, map[observestats.Key]observestats.Counters{
		{Provider: "zhipu", Model: "glm-4.6"}: {
			Requests: 1, Input: 1_000_000, Output: 500_000,
			CacheRead: 200_000, CacheCreation: 100_000,
		},
	}); err != nil {
		t.Fatal(err)
	}

	web := NewWebServer(p, "test-config.yaml")
	mux := http.NewServeMux()
	web.Register(mux)
	readCost := func() (pointCost, totalCost float64, priced, covered bool) {
		t.Helper()
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(
			recorder,
			httptest.NewRequest("GET", "/api/analytics?granularity=day", nil),
		)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status=%d want 200; body=%s", recorder.Code, recorder.Body.String())
		}
		var response struct {
			Series []struct {
				Points []struct {
					Cost   *float64 `json:"cost"`
					Priced bool     `json:"priced"`
				} `json:"points"`
			} `json:"series"`
			Totals struct {
				Cost *float64 `json:"cost"`
			} `json:"totals"`
			PriceCoverage struct {
				Priced []string `json:"priced"`
			} `json:"price_coverage"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if len(response.Series) != 1 || len(response.Series[0].Points) != 1 {
			t.Fatalf("analytics series = %+v", response.Series)
		}
		point := response.Series[0].Points[0]
		if point.Cost == nil || response.Totals.Cost == nil {
			t.Fatalf("priced response has nil cost: %+v", response)
		}
		for _, model := range response.PriceCoverage.Priced {
			covered = covered || model == "glm-4.6"
		}
		return *point.Cost, *response.Totals.Cost, point.Priced, covered
	}

	pointCost, totalCost, priced, covered := readCost()
	if pointCost < 1.839999 || pointCost > 1.840001 ||
		totalCost < 1.839999 || totalCost > 1.840001 || !priced || !covered {
		t.Errorf(
			"catalog pricing = point=%v total=%v priced=%v covered=%v, want 1.84/1.84/true/true",
			pointCost, totalCost, priced, covered,
		)
	}

	p.mu.Lock()
	nextConfig := *p.cfg
	nextConfig.Prices = map[string]PriceConfig{
		"glm-4.6": {Input: 2, Output: 4, CacheRead: 0.5, CacheWrite: 1},
	}
	p.cfg = &nextConfig
	p.mu.Unlock()
	pointCost, totalCost, priced, covered = readCost()
	if pointCost < 4.199999 || pointCost > 4.200001 ||
		totalCost < 4.199999 || totalCost > 4.200001 || !priced || !covered {
		t.Errorf(
			"override pricing = point=%v total=%v priced=%v covered=%v, want 4.2/4.2/true/true",
			pointCost, totalCost, priced, covered,
		)
	}
	if got := catalogRequests.Load(); got != 1 {
		t.Errorf("catalog requests = %d, want one cached refresh", got)
	}
}

// ---- stats_test_support_test.go ----

func openTestStatsStore(path string, retention time.Duration) (*observestats.Store, error) {
	return observestats.Open(observestats.Options{Path: path, Retention: retention})
}

// newTestStatsStore opens a fresh observestats.Store in a temp dir with no retention.
func newTestStatsStore(t *testing.T) *observestats.Store {
	t.Helper()
	ss, err := openTestStatsStore(filepath.Join(t.TempDir(), "stats.db"), 0)
	if err != nil {
		t.Fatalf("openTestStatsStore: %v", err)
	}
	t.Cleanup(func() { ss.Close() })
	return ss
}
