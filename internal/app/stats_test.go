package app

import (
	"encoding/json"
	"fmt"
	"math"
	"model-proxy/internal/appapi"
	configdomain "model-proxy/internal/config"
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
			Requests: 3, Input: 1000, Output: 200, Failures: 1, LatencySum: 900, TTFTSum: 90,
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
	// A bucket one comparison-window back drives the compare totals.
	prevMinute := minute - 36*3600
	if err := p.stats.Flush(prevMinute, map[observestats.Key]observestats.Counters{
		{Provider: "deepseek", Model: "deepseek-v4-pro"}: {Requests: 5, Input: 500, Output: 100},
	}); err != nil {
		t.Fatal(err)
	}
	// The agent dimension aggregates agent_buckets the same way.
	if err := p.stats.FlushAgents(minute, map[observestats.AgentKey]observestats.AgentCounters{
		{Agent: "codex", Provider: "deepseek", Model: "deepseek-v4-pro"}: {Requests: 2, Input: 600, Output: 120, LatencySum: 400},
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
		Totals      struct {
			Tokens       uint64   `json:"tokens"`
			AvgLatencyMs *float64 `json:"avg_latency_ms"`
			TokSec       *float64 `json:"tok_sec"`
		} `json:"totals"`
		Series []struct {
			Provider string `json:"provider"`
			Model    string `json:"model"`
			Totals   struct {
				Requests uint64 `json:"requests"`
				Tokens   uint64 `json:"tokens"`
			} `json:"totals"`
			Points []struct {
				Requests     uint64   `json:"requests"`
				Input        uint64   `json:"input"`
				Output       uint64   `json:"output"`
				Failures     uint64   `json:"failures"`
				Tokens       uint64   `json:"tokens"`
				AvgLatencyMs float64  `json:"avg_latency_ms"`
				AvgTtftMs    float64  `json:"avg_ttft_ms"`
				Cost         *float64 `json:"cost"`
				Priced       bool     `json:"priced"`
			} `json:"points"`
		} `json:"series"`
		PriceCoverage struct {
			Priced []struct {
				Provider string `json:"provider"`
				Model    string `json:"model"`
			} `json:"priced"`
			Unpriced []struct {
				Provider string `json:"provider"`
				Model    string `json:"model"`
			} `json:"unpriced"`
		} `json:"price_coverage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal analytics: %v\n%s", err, rec.Body.String())
	}
	if got.Granularity != "day" {
		t.Errorf("granularity = %q, want day", got.Granularity)
	}
	// The unified totals block carries the derived metrics: tokens =
	// 1000+200 (four-bucket), avg latency = 900ms/3 = 300 (requests-
	// weighted), tok/s = null (no duration recorded in the fixture).
	if got.Totals.Tokens != 1200 || got.Totals.AvgLatencyMs == nil || *got.Totals.AvgLatencyMs != 300 || got.Totals.TokSec != nil {
		t.Errorf("totals = %+v, want tokens=1200 avg_latency=300 tok_sec=null", got.Totals)
	}
	if len(got.Series) != 1 ||
		got.Series[0].Provider != "deepseek" ||
		got.Series[0].Model != "deepseek-v4-pro" ||
		got.Series[0].Totals.Requests != 3 || got.Series[0].Totals.Tokens != 1200 {
		t.Fatalf("series = %+v, want one deepseek/deepseek-v4-pro with totals reqs=3 tokens=1200", got.Series)
	}
	points := got.Series[0].Points
	if len(points) != 1 ||
		points[0].Requests != 3 ||
		points[0].Input != 1000 ||
		points[0].Output != 200 {
		t.Errorf("point = %+v, want reqs=3 input=1000 output=200", points)
	}
	// Widened counters: failures + requests-weighted latency/ttft averages.
	if points[0].Failures != 1 || points[0].AvgLatencyMs != 300 || points[0].AvgTtftMs != 30 {
		t.Errorf("widened point = %+v, want failures=1 avg_latency=300 avg_ttft=30", points[0])
	}
	// Never-fabricate: no catalog + no override on the test Proxy → unpriced.
	if points[0].Priced {
		t.Error("point must be unpriced without a catalog")
	}
	if points[0].Cost != nil {
		t.Errorf("unpriced point cost must be nil, got %v", *points[0].Cost)
	}
	found := false
	for _, pm := range got.PriceCoverage.Unpriced {
		found = found || (pm.Provider == "deepseek" && pm.Model == "deepseek-v4-pro")
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
			Unpriced []struct {
				Provider string `json:"provider"`
				Model    string `json:"model"`
			} `json:"unpriced"`
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
		if m.Model == "ssh" || m.Model == "ok" || m.Model == "decision" {
			t.Errorf("virtual model %q leaked into price_coverage.unpriced", m.Model)
		}
	}

	bad := httptest.NewRecorder()
	mux.ServeHTTP(bad, httptest.NewRequest("GET", "/api/analytics?granularity=year", nil))
	if bad.Code != http.StatusBadRequest {
		t.Errorf("bad granularity status=%d want 400", bad.Code)
	}
	badBy := httptest.NewRecorder()
	mux.ServeHTTP(badBy, httptest.NewRequest("GET", "/api/analytics?by=route", nil))
	if badBy.Code != http.StatusBadRequest {
		t.Errorf("bad by status=%d want 400", badBy.Code)
	}

	// hour granularity + by=agent + comparison window: one end-to-end query
	// exercising the widened store path through the admin projection.
	wide := httptest.NewRecorder()
	mux.ServeHTTP(wide, httptest.NewRequest(http.MethodGet, fmt.Sprintf(
		"/api/analytics?from=%d&to=%d&granularity=hour&by=agent",
		minute-48*3600, minute,
	), nil))
	if wide.Code != http.StatusOK {
		t.Fatalf("agent/hour status=%d want 200; body=%s", wide.Code, wide.Body.String())
	}
	var wideResp struct {
		Granularity string `json:"granularity"`
		By          string `json:"by"`
		Compare     struct {
			From     int64  `json:"from"`
			To       int64  `json:"to"`
			Requests uint64 `json:"requests"`
		} `json:"compare"`
		Series []struct {
			Agent    string `json:"agent"`
			Provider string `json:"provider"`
			Points   []struct {
				Requests     uint64  `json:"requests"`
				AvgLatencyMs float64 `json:"avg_latency_ms"`
			} `json:"points"`
		} `json:"series"`
	}
	if err := json.Unmarshal(wide.Body.Bytes(), &wideResp); err != nil {
		t.Fatalf("unmarshal agent/hour analytics: %v\n%s", err, wide.Body.String())
	}
	if wideResp.Granularity != "hour" || wideResp.By != "agent" {
		t.Errorf("agent/hour echo = %q/%q, want hour/agent", wideResp.Granularity, wideResp.By)
	}
	if len(wideResp.Series) != 1 || wideResp.Series[0].Agent != "codex" ||
		wideResp.Series[0].Provider != "deepseek" ||
		len(wideResp.Series[0].Points) != 1 || wideResp.Series[0].Points[0].Requests != 2 ||
		wideResp.Series[0].Points[0].AvgLatencyMs != 200 {
		t.Fatalf("agent/hour series = %+v", wideResp.Series)
	}
	// The comparison window is the equal-length span before `from`; the
	// prevMinute bucket (36h back) falls inside [from-48h-60, from-60] and the
	// agent table has no rows there, so compare.requests is 0 but present.
	if wideResp.Compare.From != minute-96*3600-60 || wideResp.Compare.To != minute-48*3600-60 {
		t.Errorf("agent/hour compare window = [%d,%d]", wideResp.Compare.From, wideResp.Compare.To)
	}

	// Provider-dimension compare: the 36h-old bucket lands in the previous
	// window and shows up in compare.requests/input.
	cmp := httptest.NewRecorder()
	mux.ServeHTTP(cmp, httptest.NewRequest(http.MethodGet, fmt.Sprintf(
		"/api/analytics?from=%d&to=%d&granularity=day",
		minute-24*3600, minute,
	), nil))
	if cmp.Code != http.StatusOK {
		t.Fatalf("compare status=%d want 200; body=%s", cmp.Code, cmp.Body.String())
	}
	var cmpResp struct {
		Compare struct {
			From     int64  `json:"from"`
			To       int64  `json:"to"`
			Requests uint64 `json:"requests"`
			Input    uint64 `json:"input"`
		} `json:"compare"`
	}
	if err := json.Unmarshal(cmp.Body.Bytes(), &cmpResp); err != nil {
		t.Fatalf("unmarshal compare analytics: %v\n%s", err, cmp.Body.String())
	}
	if cmpResp.Compare.From != minute-48*3600-60 || cmpResp.Compare.To != minute-24*3600-60 ||
		cmpResp.Compare.Requests != 5 || cmpResp.Compare.Input != 500 {
		t.Errorf("compare = %+v, want prev-window requests=5 input=500", cmpResp.Compare)
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
			cfg: &configdomain.Config{Pricing: configdomain.PricingConfig{
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
				Priced []struct {
					Provider string `json:"provider"`
					Model    string `json:"model"`
				} `json:"priced"`
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
		for _, pm := range response.PriceCoverage.Priced {
			covered = covered || pm.Model == "glm-4.6"
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
	nextConfig.Prices = map[string]configdomain.PriceConfig{
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

// TestAPIMCPAnalyticsHandler pins the persisted MCP analytics endpoint:
// bucketed reads, (kind, name) grouping, calls/errors totals, weighted-average
// latency, last_call_at max, and name/kind filters. A disabled stats store
// fails closed rather than returning empty buckets.
func TestAPIMCPAnalyticsHandler(t *testing.T) {
	p := &Proxy{
		processServices: processServices{
			metrics: obscounters.NewMetricsStore(),
			stats:   newTestStatsStore(t),
		},
	}
	base := time.Date(2026, 9, 20, 0, 0, 0, 0, time.Local).Unix()
	// Seed two servers and one route across two hours so day aggregation folds
	// them into one bucket per (kind, name).
	if err := p.stats.FlushMCPBuckets(base, []observestats.MCPBucketDelta{
		{Name: "web-search", Kind: observestats.MCPKindServer, Calls: 2, Errors: 1, LatencyMsSum: 200, LastCallAt: base + 10},
		{Name: "exa", Kind: observestats.MCPKindServer, Calls: 3, Errors: 0, LatencyMsSum: 900, LastCallAt: base + 20},
		{Name: "search-route", Kind: observestats.MCPKindRoute, Calls: 5, Errors: 2, LatencyMsSum: 1000, LastCallAt: base + 30},
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.stats.FlushMCPBuckets(base+3600, []observestats.MCPBucketDelta{
		{Name: "web-search", Kind: observestats.MCPKindServer, Calls: 4, Errors: 0, LatencyMsSum: 800, LastCallAt: base + 3700},
		{Name: "exa", Kind: observestats.MCPKindServer, Calls: 1, Errors: 1, LatencyMsSum: 100, LastCallAt: base + 3800},
	}); err != nil {
		t.Fatal(err)
	}

	w := NewWebServer(p, "test-config.yaml")
	mux := http.NewServeMux()
	w.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(
		http.MethodGet,
		fmt.Sprintf("/api/mcp/analytics?from=%d&to=%d&granularity=day", base, base+7200),
		nil,
	))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d want 200; body=%s", rec.Code, rec.Body.String())
	}
	var got appapi.MCPAnalyticsResult
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, rec.Body.String())
	}
	if got.Granularity != "day" {
		t.Errorf("granularity = %q, want day", got.Granularity)
	}
	byName := map[string]appapi.MCPAnalyticsSeries{}
	for _, s := range got.Series {
		byName[s.Name] = s
	}
	if len(byName) != 3 {
		t.Fatalf("series = %+v", got.Series)
	}
	ws := byName["web-search"]
	if ws.Kind != "server" || len(ws.Points) != 1 || ws.Points[0].Calls != 6 || ws.Points[0].Errors != 1 {
		t.Errorf("web-search point = %+v", ws.Points)
	}
	// weighted avg = (200+800) / (2+4) = 166.666...
	if ws.Totals.Calls != 6 || ws.Totals.Errors != 1 || math.Abs(ws.Totals.AvgLatencyMs-1000.0/6.0) > 1e-9 || ws.Totals.LastCallAt != base+3700 {
		t.Errorf("web-search totals = %+v", ws.Totals)
	}
	exa := byName["exa"]
	if exa.Kind != "server" || exa.Totals.Calls != 4 || exa.Totals.Errors != 1 || exa.Totals.LastCallAt != base+3800 {
		t.Errorf("exa totals = %+v", exa.Totals)
	}
	rt := byName["search-route"]
	if rt.Kind != "route" || rt.Totals.Calls != 5 || rt.Totals.Errors != 2 || rt.Totals.LastCallAt != base+30 {
		t.Errorf("search-route totals = %+v", rt.Totals)
	}

	// Kind filter limits to servers.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(
		http.MethodGet,
		fmt.Sprintf("/api/mcp/analytics?from=%d&to=%d&granularity=day&kind=server", base, base+7200),
		nil,
	))
	if rec.Code != http.StatusOK {
		t.Fatalf("kind filter status=%d", rec.Code)
	}
	got = appapi.MCPAnalyticsResult{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, s := range got.Series {
		if s.Kind != "server" {
			t.Errorf("kind filter leaked route: %+v", s)
		}
	}

	// Name filter isolates one server.
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(
		http.MethodGet,
		fmt.Sprintf("/api/mcp/analytics?from=%d&to=%d&granularity=day&name=web-search", base, base+7200),
		nil,
	))
	if rec.Code != http.StatusOK {
		t.Fatalf("name filter status=%d", rec.Code)
	}
	got = appapi.MCPAnalyticsResult{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Series) != 1 || got.Series[0].Name != "web-search" {
		t.Errorf("name filter series = %+v", got.Series)
	}

	// Disabled stats store: fail-closed 500.
	p.processServices.stats = nil
	w = NewWebServer(p, "test-config.yaml")
	mux = http.NewServeMux()
	w.Register(mux)
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/mcp/analytics?from=1&to=2", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("nil stats store status=%d want 500; body=%s", rec.Code, rec.Body.String())
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
