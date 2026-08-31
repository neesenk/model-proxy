package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"model-proxy/internal/appapi"
	"model-proxy/internal/fusion"
	"model-proxy/internal/observe/requestlog"
	observestats "model-proxy/internal/observe/stats"
	"model-proxy/internal/presets"
	"model-proxy/internal/pricing"
	"model-proxy/internal/webauth"
)

type readAPIStub struct {
	dashboard appapi.Dashboard
	logFile   string
	logDir    string
	accounts  []appapi.ProviderAccounts
	tokens    []appapi.TokenUsage
	stats     func(appapi.StatsQuery) ([]observestats.Bucket, error)
	agents    func(appapi.AgentStatsQuery) ([]observestats.AgentBucket, error)
	analytics func(appapi.AnalyticsQuery) ([]observestats.AnalyticsBucket, error)
	pricing   appapi.PricingSnapshot
	fusion    func(string, time.Time) (map[string]fusion.WorkflowStats, []fusion.Run)
	pins      []appapi.Pin
	security  func(appapi.SecurityQuery) (appapi.SecurityResult, error)
	config    func() (appapi.ConfigDocument, error)
	presets   []presets.Preset
}

func (r *readAPIStub) Dashboard(time.Time) appapi.Dashboard { return r.dashboard }
func (r *readAPIStub) LogFile() string                      { return r.logFile }
func (r *readAPIStub) RequestLogDirectory() string          { return r.logDir }
func (r *readAPIStub) Accounts() []appapi.ProviderAccounts  { return r.accounts }
func (r *readAPIStub) Tokens() []appapi.TokenUsage          { return r.tokens }
func (r *readAPIStub) Stats(q appapi.StatsQuery) ([]observestats.Bucket, error) {
	if r.stats == nil {
		return nil, nil
	}
	return r.stats(q)
}
func (r *readAPIStub) AgentStats(q appapi.AgentStatsQuery) ([]observestats.AgentBucket, error) {
	if r.agents == nil {
		return nil, nil
	}
	return r.agents(q)
}
func (r *readAPIStub) Analytics(q appapi.AnalyticsQuery) ([]observestats.AnalyticsBucket, error) {
	if r.analytics == nil {
		return nil, nil
	}
	return r.analytics(q)
}
func (r *readAPIStub) Pricing() appapi.PricingSnapshot { return r.pricing }
func (r *readAPIStub) Fusion(workflow string, now time.Time) (map[string]fusion.WorkflowStats, []fusion.Run) {
	if r.fusion == nil {
		return nil, nil
	}
	return r.fusion(workflow, now)
}
func (r *readAPIStub) Pins() []appapi.Pin { return r.pins }
func (r *readAPIStub) Security(q appapi.SecurityQuery) (appapi.SecurityResult, error) {
	if r.security == nil {
		return appapi.SecurityResult{}, nil
	}
	return r.security(q)
}
func (r *readAPIStub) ConfigDocument() (appapi.ConfigDocument, error) {
	if r.config == nil {
		return appapi.ConfigDocument{}, nil
	}
	return r.config()
}

func newReadServer(t *testing.T, reads *readAPIStub, opts ...func(*Options)) *Server {
	t.Helper()
	options := Options{
		Reads:    reads,
		Commands: testCommandAPI{},
		Assets: fstest.MapFS{
			"assets/index.html": {Data: []byte("INDEX")},
			"assets/styles.css": {Data: []byte("CSS")},
		},
	}
	for _, opt := range opts {
		opt(&options)
	}
	s, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// serveWebRequest dispatches one request through the mux routes Register
// installs — the same dispatch production uses.
func serveWebRequest(s *Server, w http.ResponseWriter, r *http.Request) {
	mux := http.NewServeMux()
	s.Register(mux)
	mux.ServeHTTP(w, r)
}

func serveRead(t *testing.T, s *Server, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	serveWebRequest(s, recorder, httptest.NewRequest(method, path, nil))
	return recorder
}

func decodeReadJSON(t *testing.T, recorder *httptest.ResponseRecorder, out any) {
	t.Helper()
	if got := recorder.Header().Get("content-type"); got != "application/json" {
		t.Fatalf("content-type = %q, want application/json", got)
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), out); err != nil {
		t.Fatalf("decode JSON %q: %v", recorder.Body.String(), err)
	}
}

func writeReadLog(t *testing.T, dir string, records ...requestlog.Record) {
	t.Helper()
	path := filepath.Join(dir, "requests-20260729-120000.log")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range records {
		if err := json.NewEncoder(file).Encode(record); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReadUIAndAPIRouter(t *testing.T) {
	s := newReadServer(t, &readAPIStub{})

	index := serveRead(t, s, http.MethodGet, "/ui/")
	if index.Code != http.StatusOK || index.Body.String() != "INDEX" {
		t.Fatalf("index = (%d, %q), want (200, INDEX)", index.Code, index.Body.String())
	}
	if got := index.Header().Get("content-type"); got != "text/html; charset=utf-8" {
		t.Fatalf("index content-type = %q", got)
	}
	css := serveRead(t, s, http.MethodGet, "/ui/styles.css")
	if css.Code != http.StatusOK || css.Header().Get("content-type") != "text/css; charset=utf-8" {
		t.Fatalf("css = (%d, %q)", css.Code, css.Header().Get("content-type"))
	}
	if missing := serveRead(t, s, http.MethodGet, "/ui/missing.js"); missing.Code != http.StatusNotFound {
		t.Fatalf("missing UI status = %d, want 404", missing.Code)
	}
	if root := serveRead(t, s, http.MethodGet, "/elsewhere"); root.Code != http.StatusNotFound {
		t.Fatalf("non-Web route status = %d, want 404", root.Code)
	}

	wrongMethod := serveRead(t, s, http.MethodPost, "/api/status")
	var routeError struct {
		Error string `json:"error"`
	}
	decodeReadJSON(t, wrongMethod, &routeError)
	if wrongMethod.Code != http.StatusNotFound || routeError.Error != "no api route for /api/status" {
		t.Fatalf("wrong method = (%d, %#v)", wrongMethod.Code, routeError)
	}
	viaMux := http.NewServeMux()
	s.Register(viaMux)
	recorder := httptest.NewRecorder()
	viaMux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/ui/", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "INDEX" {
		t.Fatalf("registered UI = (%d, %q)", recorder.Code, recorder.Body.String())
	}

	id := s.sessions.Create("p", "verify")
	if got, ok := s.sessions.Snapshot(id); !ok || got != (appapi.LoginUpdate{State: "pending", Detail: "verify"}) {
		t.Fatalf("session snapshot = (%#v, %v)", got, ok)
	}
}

func TestReadStatusAccountsTokensFusionPinsAndConfig(t *testing.T) {
	expires := time.Date(2026, 7, 30, 1, 2, 3, 0, time.FixedZone("test", 8*3600))
	reads := &readAPIStub{
		dashboard: appapi.Dashboard{
			Uptime: "1m", Listen: "127.0.0.1:8317", Health: map[string]any{"p": "healthy"},
			ModelLocks: map[string][]map[string]any{"r": {{"provider": "p"}}}, Quota: map[string]any{"p": 99},
			Schedule: json.RawMessage(`{"enabled":true}`), Counters: map[string]appapi.Metrics{"p": {Requests: 2}},
			Cache: map[string]any{"hits": 1}, Warnings: []string{"watch quota"},
		},
		accounts: []appapi.ProviderAccounts{{Name: "upstream", ProviderID: "p", Billing: "metered", Accounts: []appapi.Account{{ID: "a", Label: "primary"}}}},
		tokens:   []appapi.TokenUsage{{Provider: "p", Model: "m", Input: 3, Output: 4, Requests: 1}},
		fusion: func(workflow string, _ time.Time) (map[string]fusion.WorkflowStats, []fusion.Run) {
			if workflow != "judge" {
				t.Fatalf("fusion workflow = %q, want judge", workflow)
			}
			return map[string]fusion.WorkflowStats{"judge": {Runs: 2}}, []fusion.Run{{RunID: "run-1", Workflow: "judge"}}
		},
		pins: []appapi.Pin{{Route: "chat", Provider: "p", ExpiresAt: expires}, {Route: "all", Provider: "fallback"}},
		config: func() (appapi.ConfigDocument, error) {
			return appapi.ConfigDocument{YAML: "listen: :8317\n", Summary: appapi.ConfigSummary{Listen: ":8317", ProviderCount: 1, RouteCount: 2}, ProviderModels: map[string][]string{"p": {"m"}}, Routes: map[string][]appapi.ConfigRouteTarget{"chat": {{Provider: "p", Model: "m", Priority: 3}}}}, nil
		},
	}
	s := newReadServer(t, reads, func(o *Options) { o.Version = "v-test" })

	status := serveRead(t, s, http.MethodGet, "/api/status")
	var gotStatus struct {
		Uptime   string                      `json:"uptime"`
		Version  string                      `json:"version"`
		Listen   string                      `json:"listen"`
		Warnings []string                    `json:"warnings"`
		Counters map[string]appapi.Metrics   `json:"counters"`
		Schedule json.RawMessage             `json:"schedule"`
		Health   map[string]any              `json:"health"`
		Cache    map[string]any              `json:"cache"`
		Quota    map[string]any              `json:"quota"`
		Locks    map[string][]map[string]any `json:"model_locks"`
	}
	decodeReadJSON(t, status, &gotStatus)
	if status.Code != http.StatusOK || gotStatus.Uptime != "1m" || gotStatus.Version != "v-test" || gotStatus.Listen != "127.0.0.1:8317" || gotStatus.Counters["p"].Requests != 2 || gotStatus.Health["p"] != "healthy" || gotStatus.Quota["p"] != float64(99) || gotStatus.Cache["hits"] != float64(1) || len(gotStatus.Locks["r"]) != 1 || string(gotStatus.Schedule) != `{"enabled":true}` || strings.Join(gotStatus.Warnings, ",") != "watch quota" {
		t.Fatalf("unexpected status projection: %#v", gotStatus)
	}

	accounts := serveRead(t, s, http.MethodGet, "/api/accounts")
	var gotAccounts struct {
		Providers []appapi.ProviderAccounts `json:"providers"`
	}
	decodeReadJSON(t, accounts, &gotAccounts)
	if accounts.Code != http.StatusOK || len(gotAccounts.Providers) != 1 || gotAccounts.Providers[0].Accounts[0].ID != "a" {
		t.Fatalf("accounts = %#v", gotAccounts)
	}
	tokens := serveRead(t, s, http.MethodGet, "/api/tokens")
	var gotTokens struct {
		Usage []appapi.TokenUsage `json:"usage"`
	}
	decodeReadJSON(t, tokens, &gotTokens)
	if tokens.Code != http.StatusOK || len(gotTokens.Usage) != 1 || gotTokens.Usage[0].Output != 4 {
		t.Fatalf("tokens = %#v", gotTokens)
	}

	fusionResponse := serveRead(t, s, http.MethodGet, "/api/fusion?workflow=judge")
	var gotFusion struct {
		Workflows map[string]fusion.WorkflowStats `json:"workflows"`
		Runs      []fusion.Run                    `json:"runs"`
	}
	decodeReadJSON(t, fusionResponse, &gotFusion)
	if fusionResponse.Code != http.StatusOK || gotFusion.Workflows["judge"].Runs != 2 || len(gotFusion.Runs) != 1 || gotFusion.Runs[0].RunID != "run-1" {
		t.Fatalf("fusion = %#v", gotFusion)
	}

	pins := serveRead(t, s, http.MethodGet, "/api/pin")
	var gotPins struct {
		Pins []struct {
			Route     string `json:"route"`
			Provider  string `json:"provider"`
			ExpiresAt string `json:"expires_at"`
		} `json:"pins"`
	}
	decodeReadJSON(t, pins, &gotPins)
	if pins.Code != http.StatusOK || len(gotPins.Pins) != 2 || gotPins.Pins[0].ExpiresAt != "2026-07-29T17:02:03Z" || gotPins.Pins[1].ExpiresAt != "" {
		t.Fatalf("pins = %#v", gotPins)
	}

	config := serveRead(t, s, http.MethodGet, "/api/config")
	var gotConfig appapi.ConfigDocument
	decodeReadJSON(t, config, &gotConfig)
	if config.Code != http.StatusOK || gotConfig.Summary.RouteCount != 2 || gotConfig.ProviderModels["p"][0] != "m" || gotConfig.Routes["chat"][0].Priority != 3 {
		t.Fatalf("config = %#v", gotConfig)
	}

	reads.config = func() (appapi.ConfigDocument, error) {
		return appapi.ConfigDocument{}, errors.New("config unavailable")
	}
	failed := serveRead(t, s, http.MethodGet, "/api/config")
	var routeError struct {
		Error string `json:"error"`
	}
	decodeReadJSON(t, failed, &routeError)
	if failed.Code != http.StatusInternalServerError || routeError.Error != "config unavailable" {
		t.Fatalf("config failure = (%d, %#v)", failed.Code, routeError)
	}
}

func TestReadLogsAndRequestLogEndpoints(t *testing.T) {
	tmp := t.TempDir()
	logPath := filepath.Join(tmp, "daemon.log")
	if err := os.WriteFile(logPath, []byte("one\ntwo\nthree\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reads := &readAPIStub{logFile: "ignored", logDir: tmp}
	s := newReadServer(t, reads, func(o *Options) { o.LogFile = func() string { return logPath } })
	logs := serveRead(t, s, http.MethodGet, "/api/logs?tail=2")
	var gotLogs struct {
		Lines []string `json:"lines"`
	}
	decodeReadJSON(t, logs, &gotLogs)
	if logs.Code != http.StatusOK || strings.Join(gotLogs.Lines, ",") != "two,three" {
		t.Fatalf("logs = %#v", gotLogs)
	}
	var largeLog strings.Builder
	for line := 1; line <= 1002; line++ {
		_, _ = fmt.Fprintf(&largeLog, "line-%d\n", line)
	}
	if err := os.WriteFile(logPath, []byte(largeLog.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	capped := serveRead(t, s, http.MethodGet, "/api/logs?tail=5000")
	var gotCapped struct {
		Lines []string `json:"lines"`
	}
	decodeReadJSON(t, capped, &gotCapped)
	if capped.Code != http.StatusOK || len(gotCapped.Lines) != 1000 {
		t.Fatalf("capped logs status=%d count=%d", capped.Code, len(gotCapped.Lines))
	}
	if gotCapped.Lines[0] != "line-3" || gotCapped.Lines[999] != "line-1002" {
		t.Fatalf("capped logs first=%q last=%q", gotCapped.Lines[0], gotCapped.Lines[999])
	}

	writeReadLog(t, tmp,
		requestlog.Record{Ts: "2026-07-29T12:00:00Z", RequestID: "old", CalledModel: "m", Provider: "p", Status: 200, RequestBody: "secret", ResponseBody: "reply", ResponseHeaders: `{"x-request-id":"x"}`},
		requestlog.Record{Ts: "2026-07-29T12:01:00Z", RequestID: "wanted", Shadow: true, CalledModel: "Model-X", Provider: "Provider-X", Status: 500, RequestBody: "secret", ResponseBody: "reply", ResponseHeaders: `{"x-request-id":"x"}`},
	)
	list := serveRead(t, s, http.MethodGet, "/api/requests?model=model-x&provider=provider-x&errors=1&shadow=only&status=500&limit=5000&from=2026-07-29T12:00:30Z&to=2026-07-29T12:02:00Z")
	var gotList struct {
		Enabled bool                 `json:"enabled"`
		Records []requestlog.Summary `json:"records"`
	}
	decodeReadJSON(t, list, &gotList)
	if list.Code != http.StatusOK || !gotList.Enabled || len(gotList.Records) != 1 || gotList.Records[0].RequestID != "wanted" || gotList.Records[0].Status != 500 {
		t.Fatalf("request list = %#v", gotList)
	}
	if strings.Contains(list.Body.String(), "secret") || strings.Contains(list.Body.String(), "reply") || strings.Contains(list.Body.String(), "x-request-id") {
		t.Fatalf("request list leaked detail data: %s", list.Body.String())
	}

	detail := serveRead(t, s, http.MethodGet, "/api/requests/wanted")
	var gotDetail struct {
		Records []requestlog.Record `json:"records"`
	}
	decodeReadJSON(t, detail, &gotDetail)
	if detail.Code != http.StatusOK || len(gotDetail.Records) != 1 || gotDetail.Records[0].RequestBody != "secret" || gotDetail.Records[0].ResponseHeaders != `{"x-request-id":"x"}` {
		t.Fatalf("request detail = %#v", gotDetail)
	}
	missing := serveRead(t, s, http.MethodGet, "/api/requests/nope")
	var routeError struct {
		Error string `json:"error"`
	}
	decodeReadJSON(t, missing, &routeError)
	if missing.Code != http.StatusNotFound || routeError.Error != "no record for request id nope" {
		t.Fatalf("missing detail = (%d, %#v)", missing.Code, routeError)
	}

	reads.logDir = ""
	disabled := serveRead(t, s, http.MethodGet, "/api/requests")
	var gotDisabled struct {
		Enabled bool  `json:"enabled"`
		Records []any `json:"records"`
	}
	decodeReadJSON(t, disabled, &gotDisabled)
	// The contract is a NON-NULL empty array: `len(...) != 0` alone would also
	// pass a JSON null (nil slice), so pin the raw bytes too.
	if disabled.Code != http.StatusOK || gotDisabled.Enabled || gotDisabled.Records == nil {
		t.Fatalf("disabled request logging = %#v (records must be a non-null empty array)", gotDisabled)
	}
	if body := disabled.Body.String(); !strings.Contains(body, `"records":[]`) {
		t.Fatalf("disabled request logging body must serialize records as []: %s", body)
	}
	noDetail := serveRead(t, s, http.MethodGet, "/api/requests/")
	decodeReadJSON(t, noDetail, &routeError)
	if noDetail.Code != http.StatusNotFound || routeError.Error != "request logging is off or no id given" {
		t.Fatalf("disabled detail = (%d, %#v)", noDetail.Code, routeError)
	}
	reads.logDir = filepath.Join(t.TempDir(), "missing")
	listFailure := serveRead(t, s, http.MethodGet, "/api/requests")
	decodeReadJSON(t, listFailure, &routeError)
	if listFailure.Code != http.StatusInternalServerError || !strings.HasPrefix(routeError.Error, "request query: ") {
		t.Fatalf("request list error = (%d, %#v)", listFailure.Code, routeError)
	}
	detailFailure := serveRead(t, s, http.MethodGet, "/api/requests/record")
	decodeReadJSON(t, detailFailure, &routeError)
	if detailFailure.Code != http.StatusInternalServerError || !strings.HasPrefix(routeError.Error, "request query: ") {
		t.Fatalf("request detail error = (%d, %#v)", detailFailure.Code, routeError)
	}
}

func TestReadShadowReportAndErrors(t *testing.T) {
	tmp := t.TempDir()
	writeReadLog(t, tmp,
		requestlog.Record{Ts: "2026-07-29T12:00:00Z", RequestID: "pair", Exposed: "chat", Provider: "primary", Status: 200, LatencyMs: 11, ResponseSize: 100},
		requestlog.Record{Ts: "2026-07-29T12:00:01Z", RequestID: "shadow-pair", Shadow: true, Exposed: "chat", Provider: "shadow", Status: 500, LatencyMs: 18, ResponseSize: 70},
	)
	reads := &readAPIStub{logDir: tmp}
	s := newReadServer(t, reads)
	report := serveRead(t, s, http.MethodGet, "/api/shadow-report?from=2026-07-29T11:59:00Z&to=2026-07-29T12:02:00Z")
	var got struct {
		Enabled bool                           `json:"enabled"`
		Entries []requestlog.ShadowReportEntry `json:"entries"`
	}
	decodeReadJSON(t, report, &got)
	if report.Code != http.StatusOK || !got.Enabled || len(got.Entries) != 1 || got.Entries[0].Route != "chat" || got.Entries[0].Samples != 1 || got.Entries[0].StatusMatchRate != 0 || got.Entries[0].LatencyDiffMs != 7 {
		t.Fatalf("shadow report = %#v", got)
	}

	reads.logDir = ""
	disabled := serveRead(t, s, http.MethodGet, "/api/shadow-report")
	var empty struct {
		Enabled bool  `json:"enabled"`
		Entries []any `json:"entries"`
	}
	decodeReadJSON(t, disabled, &empty)
	if disabled.Code != http.StatusOK || empty.Enabled || empty.Entries == nil {
		t.Fatalf("disabled report = %#v (entries must be a non-null empty array)", empty)
	}
	if body := disabled.Body.String(); !strings.Contains(body, `"entries":[]`) {
		t.Fatalf("disabled report body must serialize entries as []: %s", body)
	}

	reads.logDir = filepath.Join(t.TempDir(), "missing")
	failed := serveRead(t, s, http.MethodGet, "/api/shadow-report")
	var routeError struct {
		Error string `json:"error"`
	}
	decodeReadJSON(t, failed, &routeError)
	if failed.Code != http.StatusInternalServerError || !strings.HasPrefix(routeError.Error, "shadow report: ") {
		t.Fatalf("shadow error = (%d, %#v)", failed.Code, routeError)
	}
}

func TestReadStatsAgentsAndAnalyticsQueries(t *testing.T) {
	var statsQuery appapi.StatsQuery
	var agentQuery appapi.AgentStatsQuery
	var analyticsQuery appapi.AnalyticsQuery
	reads := &readAPIStub{
		stats: func(q appapi.StatsQuery) ([]observestats.Bucket, error) {
			statsQuery = q
			return []observestats.Bucket{{Provider: "p", Model: "m", Minute: 120, Requests: 2}}, nil
		},
		agents: func(q appapi.AgentStatsQuery) ([]observestats.AgentBucket, error) {
			agentQuery = q
			return []observestats.AgentBucket{{Agent: "a", Provider: "p", Model: "m", Minute: 120, Requests: 3}}, nil
		},
		analytics: func(q appapi.AnalyticsQuery) ([]observestats.AnalyticsBucket, error) {
			analyticsQuery = q
			return []observestats.AnalyticsBucket{
				{Provider: "p", Model: "priced", Bucket: 100, Requests: 2, Input: 10, Output: 5, CacheRead: 2, CacheCreation: 1},
				{Provider: "p", Model: "unknown", Bucket: 200, Requests: 1, Input: 7, Output: 3},
			}, nil
		},
		pricing: appapi.PricingSnapshot{Catalog: &pricing.Catalog{ByModel: map[string]pricing.Entry{"priced": {Prompt: .001, Completion: .002, CacheRead: .003, CacheWrite: .004}}}},
	}
	s := newReadServer(t, reads)
	stats := serveRead(t, s, http.MethodGet, "/api/stats?from=100&to=1970-01-01T00:03:20Z&provider=p&model=m&bucket=61s")
	var statsResponse struct {
		From    int64                 `json:"from"`
		To      int64                 `json:"to"`
		Bucket  int64                 `json:"bucket"`
		Buckets []observestats.Bucket `json:"buckets"`
	}
	decodeReadJSON(t, stats, &statsResponse)
	if stats.Code != http.StatusOK || statsQuery != (appapi.StatsQuery{From: 100, To: 200, Provider: "p", Model: "m", BucketSecs: 120}) || statsResponse.Bucket != 120 || len(statsResponse.Buckets) != 1 || statsResponse.Buckets[0].Requests != 2 {
		t.Fatalf("stats query=%#v response=%#v", statsQuery, statsResponse)
	}
	agents := serveRead(t, s, http.MethodGet, "/api/agents?from=100&to=200&agent=a&provider=p&model=m&bucket=bad")
	var agentsResponse struct {
		Bucket  int64                      `json:"bucket"`
		Buckets []observestats.AgentBucket `json:"buckets"`
	}
	decodeReadJSON(t, agents, &agentsResponse)
	if agents.Code != http.StatusOK || agentQuery != (appapi.AgentStatsQuery{From: 100, To: 200, Agent: "a", Provider: "p", Model: "m", BucketSecs: 60}) || agentsResponse.Bucket != 60 || agentsResponse.Buckets[0].Requests != 3 {
		t.Fatalf("agent query=%#v response=%#v", agentQuery, agentsResponse)
	}

	analytics := serveRead(t, s, http.MethodGet, "/api/analytics?from=100&to=200&provider=p&model=all&granularity=month")
	var analyticsResponse struct {
		Granularity string `json:"granularity"`
		Totals      struct {
			Input  uint64   `json:"input"`
			Output uint64   `json:"output"`
			Cost   *float64 `json:"cost"`
		} `json:"totals"`
		Coverage struct {
			Priced   []string `json:"priced"`
			Unpriced []string `json:"unpriced"`
		} `json:"price_coverage"`
		Series []struct {
			Model  string `json:"model"`
			Points []struct {
				Cost   *float64 `json:"cost"`
				Priced bool     `json:"priced"`
			} `json:"points"`
		} `json:"series"`
	}
	decodeReadJSON(t, analytics, &analyticsResponse)
	if analytics.Code != http.StatusOK || analyticsQuery != (appapi.AnalyticsQuery{From: 100, To: 200, Provider: "p", Model: "all", Granularity: "month"}) || analyticsResponse.Granularity != "month" || analyticsResponse.Totals.Input != 17 || analyticsResponse.Totals.Output != 8 || analyticsResponse.Totals.Cost == nil || math.Abs(*analyticsResponse.Totals.Cost-.03) > 1e-12 || strings.Join(analyticsResponse.Coverage.Priced, ",") != "priced" || strings.Join(analyticsResponse.Coverage.Unpriced, ",") != "unknown" || len(analyticsResponse.Series) != 2 || !analyticsResponse.Series[0].Points[0].Priced || analyticsResponse.Series[1].Points[0].Cost != nil {
		t.Fatalf("analytics query=%#v response=%#v", analyticsQuery, analyticsResponse)
	}

	invalid := serveRead(t, s, http.MethodGet, "/api/analytics?granularity=hour")
	var routeError struct {
		Error string `json:"error"`
	}
	decodeReadJSON(t, invalid, &routeError)
	if invalid.Code != http.StatusBadRequest || routeError.Error != "granularity must be day or month" {
		t.Fatalf("invalid analytics = (%d, %#v)", invalid.Code, routeError)
	}

	reads.stats = func(appapi.StatsQuery) ([]observestats.Bucket, error) { return nil, errors.New("store down") }
	statsFailure := serveRead(t, s, http.MethodGet, "/api/stats?from=1&to=2")
	decodeReadJSON(t, statsFailure, &routeError)
	if statsFailure.Code != http.StatusInternalServerError || routeError.Error != "stats query: store down" {
		t.Fatalf("stats failure = (%d, %#v)", statsFailure.Code, routeError)
	}
	reads.agents = func(appapi.AgentStatsQuery) ([]observestats.AgentBucket, error) {
		return nil, errors.New("agent store down")
	}
	agentFailure := serveRead(t, s, http.MethodGet, "/api/agents?from=1&to=2")
	decodeReadJSON(t, agentFailure, &routeError)
	if agentFailure.Code != http.StatusInternalServerError || routeError.Error != "agent stats query: agent store down" {
		t.Fatalf("agent failure = (%d, %#v)", agentFailure.Code, routeError)
	}
	reads.analytics = func(appapi.AnalyticsQuery) ([]observestats.AnalyticsBucket, error) {
		return nil, errors.New("analytics store down")
	}
	analyticsFailure := serveRead(t, s, http.MethodGet, "/api/analytics?from=1&to=2")
	decodeReadJSON(t, analyticsFailure, &routeError)
	if analyticsFailure.Code != http.StatusInternalServerError || routeError.Error != "analytics query: analytics store down" {
		t.Fatalf("analytics failure = (%d, %#v)", analyticsFailure.Code, routeError)
	}
}

func TestReadHelperBranches(t *testing.T) {
	if err := appapi.RequirePorts(nil, testCommandAPI{}); err == nil || err.Error() != "appapi ReadAPI is nil" {
		t.Fatalf("nil reads error = %v", err)
	}
	if err := appapi.RequirePorts(testReadAPI{}, nil); err == nil || err.Error() != "appapi CommandAPI is nil" {
		t.Fatalf("nil commands error = %v", err)
	}
	for _, test := range []struct {
		in   string
		want int64
	}{
		{"", 60}, {"0", 60}, {"-1", 60}, {"60", 60}, {"61", 120}, {"90s", 120}, {"2m", 120}, {"bad", 60},
	} {
		if got := observestats.NormalizeBucket(test.in); got != test.want {
			t.Errorf("normalizeBucket(%q) = %d, want %d", test.in, got, test.want)
		}
	}
	if got := mapKeys(nil); len(got) != 0 || got == nil {
		t.Fatalf("mapKeys(nil) = %#v, want non-nil empty", got)
	}
	if got := strings.Join(mapKeys(map[string]bool{"z": true, "a": true}), ","); got != "a,z" {
		t.Fatalf("sorted map keys = %q", got)
	}

	file := filepath.Join(t.TempDir(), "tail.log")
	if err := os.WriteFile(file, []byte("a\nb\nc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := tailFile(file, 20); err != nil || strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("full tail = %#v, %v", got, err)
	}
	if got, err := tailFile(file, 1); err != nil || strings.Join(got, ",") != "c" {
		t.Fatalf("limited tail = %#v, %v", got, err)
	}
	if _, err := tailFile(filepath.Join(t.TempDir(), "none"), 1); err == nil {
		t.Fatal("missing tail file unexpectedly succeeded")
	}
	for name, contents := range map[string][]byte{"empty file": nil, "newline only": []byte("\n")} {
		empty := filepath.Join(t.TempDir(), name+".log")
		if err := os.WriteFile(empty, contents, 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := tailFile(empty, 20)
		if err != nil {
			t.Fatalf("tailFile(%s): %v", name, err)
		}
		if len(got) != 0 {
			t.Fatalf("tailFile(%s) = %#v, want empty line list", name, got)
		}
	}

	recorder := httptest.NewRecorder()
	writePortErr(recorder, http.StatusInternalServerError, errors.New("plain"))
	if recorder.Code != http.StatusInternalServerError || recorder.Body.String() != `{"error":"plain"}` {
		t.Fatalf("unclassified port error = (%d, %q)", recorder.Code, recorder.Body.String())
	}
	zeroStatus := httptest.NewRecorder()
	writePortErr(zeroStatus, http.StatusTeapot, &appapi.HTTPError{Message: "zero"})
	if zeroStatus.Code != http.StatusTeapot || zeroStatus.Body.String() != `{"error":"zero"}` {
		t.Fatalf("zero-status port error = (%d, %q)", zeroStatus.Code, zeroStatus.Body.String())
	}

	unavailable := httptest.NewRecorder()
	writeJSONErr(unavailable, http.StatusServiceUnavailable, "catalog is unavailable")
	if unavailable.Code != http.StatusServiceUnavailable || unavailable.Body.String() != `{"error":"catalog is unavailable"}` {
		t.Fatalf("unavailable response = (%d, %q)", unavailable.Code, unavailable.Body.String())
	}
	// An unmarshalable value (NaN) must degrade to a well-formed 500, NOT a
	// panic: net/http would recover it per-connection, leaving the client an
	// empty/truncated body instead of an explicit error.
	bad := httptest.NewRecorder()
	writeJSON(bad, http.StatusOK, math.NaN())
	if bad.Code != http.StatusInternalServerError || bad.Body.String() == "" {
		t.Fatalf("unmarshalable value = (%d, %q), want 500 with an error body", bad.Code, bad.Body.String())
	}
}

func TestReadLogsUnavailableAndFailure(t *testing.T) {
	reads := &readAPIStub{}
	s := newReadServer(t, reads)
	noLog := serveRead(t, s, http.MethodGet, "/api/logs")
	var routeError struct {
		Error string `json:"error"`
	}
	decodeReadJSON(t, noLog, &routeError)
	if noLog.Code != http.StatusNotFound || routeError.Error != "no log_file configured" {
		t.Fatalf("no log = (%d, %#v)", noLog.Code, routeError)
	}
	reads.logFile = filepath.Join(t.TempDir(), "missing.log")
	missing := serveRead(t, s, http.MethodGet, "/api/logs?tail=-2")
	decodeReadJSON(t, missing, &routeError)
	if missing.Code != http.StatusInternalServerError || !strings.Contains(routeError.Error, "missing.log") {
		t.Fatalf("missing log = (%d, %#v)", missing.Code, routeError)
	}
}

func TestReadSecurityEndpoint(t *testing.T) {
	var securityQuery appapi.SecurityQuery
	reads := &readAPIStub{
		security: func(q appapi.SecurityQuery) (appapi.SecurityResult, error) {
			securityQuery = q
			return appapi.SecurityResult{
				Enabled: true,
				Records: []appapi.SecurityRecord{
					{Ts: 1700000000123, Kind: "secret", RequestID: "r1", Agent: "codex", Protocol: "anthropic", Exposed: "gpt-x", Names: []string{"aws-access-key"}, Action: "blocked"},
				},
				Skipped: 2,
			}, nil
		},
	}
	s := newReadServer(t, reads)

	// Full query: kind passes through, from/to parse like /api/stats (unix
	// seconds or RFC3339) and convert to the audit log's millisecond domain
	// (to inclusive to the end of the named second); the response mirrors the
	// read port's DTO verbatim.
	ok := serveRead(t, s, http.MethodGet, "/api/security?kind=secret&from=1700000000&to=2023-11-14T22:13:30Z&limit=5")
	var response struct {
		Enabled bool                    `json:"enabled"`
		Records []appapi.SecurityRecord `json:"records"`
		Skipped int                     `json:"skipped"`
	}
	decodeReadJSON(t, ok, &response)
	if ok.Code != http.StatusOK || securityQuery != (appapi.SecurityQuery{Kind: "secret", From: 1700000000000, To: 1700000010999, Limit: 5}) {
		t.Fatalf("security = (%d, query=%#v)", ok.Code, securityQuery)
	}
	if !response.Enabled || response.Skipped != 2 || len(response.Records) != 1 ||
		response.Records[0].Ts != 1700000000123 || response.Records[0].Kind != "secret" ||
		response.Records[0].Agent != "codex" || response.Records[0].Exposed != "gpt-x" ||
		len(response.Records[0].Names) != 1 || response.Records[0].Names[0] != "aws-access-key" ||
		response.Records[0].Action != "blocked" {
		t.Fatalf("security response = %#v", response)
	}

	// Defaults and caps: no params -> limit 100, unbounded time; limit is
	// capped at 1000.
	serveRead(t, s, http.MethodGet, "/api/security")
	if securityQuery != (appapi.SecurityQuery{Limit: 100}) {
		t.Fatalf("default security query = %#v", securityQuery)
	}
	serveRead(t, s, http.MethodGet, "/api/security?limit=5000")
	if securityQuery != (appapi.SecurityQuery{Limit: 1000}) {
		t.Fatalf("capped security query = %#v", securityQuery)
	}

	var routeError struct {
		Error string `json:"error"`
	}
	invalid := serveRead(t, s, http.MethodGet, "/api/security?kind=tokens")
	decodeReadJSON(t, invalid, &routeError)
	if invalid.Code != http.StatusBadRequest || routeError.Error != "kind must be secret, path or drift" {
		t.Fatalf("invalid kind = (%d, %#v)", invalid.Code, routeError)
	}

	reads.security = func(appapi.SecurityQuery) (appapi.SecurityResult, error) {
		return appapi.SecurityResult{}, errors.New("audit dir unreadable")
	}
	failure := serveRead(t, s, http.MethodGet, "/api/security")
	decodeReadJSON(t, failure, &routeError)
	// The underlying error (which may embed local paths) must NOT reach the
	// client — only the generic message is exposed.
	if failure.Code != http.StatusInternalServerError || routeError.Error != "failed to query security log" {
		t.Fatalf("security failure = (%d, %#v)", failure.Code, routeError)
	}
}

func TestReadServerStartStops(t *testing.T) {
	s := newReadServer(t, &readAPIStub{})
	if !s.Start() {
		t.Fatal("first Start was rejected")
	}
	s.Close()
	if s.Start() {
		t.Fatal("Start admitted a task after Close")
	}
}

var _ appapi.CommandAPI = testCommandAPI{}

// --- GET /metrics (Prometheus text exposition, S7) ---

// newMetricsTestServer builds a Server with stubbed reads carrying two
// provider counters (one with traffic, one idle).
func newMetricsTestServer(t *testing.T) *Server {
	t.Helper()
	reads := &readAPIStub{dashboard: appapi.Dashboard{
		Counters: map[string]appapi.Metrics{
			"zhipu":    {Requests: 5, Failures: 1, LatencySum: 1000, TTFTSum: 400},
			"deepseek": {}, // idle: must not emit series
		},
	}}
	s, err := New(Options{Reads: reads, Commands: &testCommandAPI{}, Version: "t"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func TestMetricsEndpointPrometheusExposition(t *testing.T) {
	s := newMetricsTestServer(t)
	rec := httptest.NewRecorder()
	serveWebRequest(s, rec, httptest.NewRequest("GET", "/metrics", nil))
	if rec.Code != 200 {
		t.Fatalf("/metrics status=%d body=%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("content-type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("content-type = %q, want text/plain exposition", ct)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`# TYPE model_proxy_requests_total counter`,
		`model_proxy_requests_total{provider="zhipu"} 5`,
		`model_proxy_failures_total{provider="zhipu"} 1`,
		`model_proxy_latency_milliseconds_sum{provider="zhipu"} 1000`,
		`model_proxy_ttft_milliseconds_sum{provider="zhipu"} 400`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("exposition missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `provider="deepseek"`) {
		t.Errorf("idle provider must not emit series:\n%s", body)
	}
}

func TestMetricsEndpointRejectsNonGet(t *testing.T) {
	s := newMetricsTestServer(t)
	rec := httptest.NewRecorder()
	serveWebRequest(s, rec, httptest.NewRequest("POST", "/metrics", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /metrics status=%d, want 405", rec.Code)
	}
}

func TestMetricsLabelEscaping(t *testing.T) {
	v := appapi.Dashboard{Counters: map[string]appapi.Metrics{
		`we"ird`: {Requests: 1},
	}}
	body := renderPrometheus(v)
	if !strings.Contains(body, `provider="we\"ird"`) {
		t.Errorf("label value not escaped:\n%s", body)
	}
}

// --- S2 admin-surface auth on the web transport ---

func newAuthedServer(t *testing.T, enabled bool) (*Server, *webauth.Source) {
	t.Helper()
	src := webauth.NewSource()
	if enabled {
		// Enabled-but-empty token file: accepts nothing until a token is added.
		path := filepath.Join(t.TempDir(), "admin.tok")
		if err := os.WriteFile(path, []byte("# admin tokens\nadm-secret\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		src = webauth.NewSource(path)
	}
	reads := &readAPIStub{dashboard: appapi.Dashboard{Counters: map[string]appapi.Metrics{}}}
	s, err := New(Options{Reads: reads, Commands: &testCommandAPI{}, Version: "t",
		AdminAuth: func() *webauth.Source { return src }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, src
}

func TestAdminAuthGatesDataButServesUIBootstrap(t *testing.T) {
	s, _ := newAuthedServer(t, true)
	// Embedded UI assets contain no runtime data and must load so a browser can
	// establish its HttpOnly API session. Data and metrics stay fail-closed.
	ui := httptest.NewRecorder()
	serveWebRequest(s, ui, httptest.NewRequest(http.MethodGet, "/ui/", nil))
	if ui.Code != http.StatusOK {
		t.Fatalf("GET /ui/ without token = %d, want bootstrap 200", ui.Code)
	}
	for _, path := range []string{"/api/status", "/metrics"} {
		rec := httptest.NewRecorder()
		serveWebRequest(s, rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without token = %d, want 401", path, rec.Code)
		}
		rec = httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer adm-secret")
		serveWebRequest(s, rec, req)
		// Exact 200: `!= 401` would also pass a 500 from a broken handler.
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s with valid token = %d, want 200 (body=%s)", path, rec.Code, rec.Body.String())
		}
	}
	// Wrong token rejected.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/status", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	serveWebRequest(s, rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong token = %d, want 401", rec.Code)
	}
	// x-api-key is accepted symmetrically with Bearer.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("GET", "/api/status", nil)
	req.Header.Set("x-api-key", "adm-secret")
	serveWebRequest(s, rec, req)
	if rec.Code == http.StatusUnauthorized {
		t.Error("x-api-key admin token rejected")
	}
}

func TestAdminBrowserSessionAuthenticatesAPIAndCanBeCleared(t *testing.T) {
	s, _ := newAuthedServer(t, true)

	// Session creation is itself same-origin guarded and requires an explicit
	// bearer token; no ambient cookie can bootstrap a new credential.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/session", nil)
	req.Host = "192.0.2.10:8123"
	req.Header.Set("Origin", "http://192.0.2.10:8123")
	serveWebRequest(s, rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("session without bearer = %d, want 401", rec.Code)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/auth/session", nil)
	req.Host = "192.0.2.10:8123"
	req.Header.Set("Origin", "http://192.0.2.10:8123")
	req.Header.Set("x-api-key", "adm-secret")
	serveWebRequest(s, rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("session with x-api-key = %d, want explicit bearer rejection", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/auth/session", nil)
	req.Host = "192.0.2.10:8123"
	req.Header.Set("Origin", "http://192.0.2.10:8123")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Authorization", "Bearer adm-secret")
	serveWebRequest(s, rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("session create = %d, want 204 (body=%s)", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("session cookies = %d, want 1", len(cookies))
	}
	session := cookies[0]
	if session.Name != adminSessionCookie || session.Value == "adm-secret" {
		t.Fatalf("session cookie name/value shape = %q/%q", session.Name, session.Value)
	}
	if !session.HttpOnly || session.Path != adminSessionCookiePath || session.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie attributes = %+v", session)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("session Cache-Control = %q, want no-store", got)
	}

	// The browser cookie authorizes ordinary fetch and EventSource requests
	// under /api without exposing the token to JavaScript.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Host = "192.0.2.10:8123"
	req.Header.Set("Origin", "http://192.0.2.10:8123")
	req.AddCookie(session)
	serveWebRequest(s, rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("cookie-authenticated status = %d, want 200", rec.Code)
	}

	// The same cookie is deliberately not an authentication mechanism outside
	// /api, even when a non-browser client manually violates Cookie.Path.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/metrics", nil)
	req.AddCookie(session)
	serveWebRequest(s, rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("session cookie on /metrics = %d, want explicit bearer 401", rec.Code)
	}

	// Explicit credentials take precedence over ambient cookies, so a bad
	// header cannot be hidden by a good session.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	req.AddCookie(session)
	serveWebRequest(s, rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bad header plus good cookie = %d, want 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodDelete, "/api/auth/session", nil)
	req.Host = "192.0.2.10:8123"
	req.Header.Set("Origin", "http://192.0.2.10:8123")
	req.AddCookie(session)
	serveWebRequest(s, rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("session clear = %d, want 204", rec.Code)
	}
	cleared := rec.Result().Cookies()
	if len(cleared) != 1 || cleared[0].MaxAge >= 0 || cleared[0].Path != adminSessionCookiePath {
		t.Fatalf("cleared cookie = %+v", cleared)
	}
}

func TestAdminAuthAllowsSameOriginLANIPButRejectsDNSRebindingHost(t *testing.T) {
	s, _ := newAuthedServer(t, true)
	for _, path := range []string{"/ui/", "/api/status"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Host = "192.0.2.10:8123"
		req.Header.Set("Origin", "http://192.0.2.10:8123")
		if path != "/ui/" {
			req.Header.Set("Authorization", "Bearer adm-secret")
		}
		serveWebRequest(s, rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("LAN GET %s = %d, want 200 (body=%s)", path, rec.Code, rec.Body.String())
		}
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Host = "attacker.rebound:8123"
	req.Header.Set("Origin", "http://attacker.rebound:8123")
	req.Header.Set("Authorization", "Bearer adm-secret")
	serveWebRequest(s, rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("authenticated rebinding host = %d, want 403", rec.Code)
	}
}

func TestAdminAuthAllowsConfiguredListenHostnameOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin.tok")
	if err := os.WriteFile(path, []byte("adm-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := webauth.NewSource(path)
	s, err := New(Options{
		Reads:         &readAPIStub{dashboard: appapi.Dashboard{Counters: map[string]appapi.Metrics{}}},
		Commands:      &testCommandAPI{},
		AdminAuth:     func() *webauth.Source { return src },
		BrowserListen: "proxy.team.test:8123",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	for _, tc := range []struct {
		host string
		want int
	}{
		{host: "proxy.team.test:8123", want: http.StatusOK},
		{host: "attacker.rebound:8123", want: http.StatusForbidden},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
		req.Host = tc.host
		req.Header.Set("Origin", "http://"+tc.host)
		req.Header.Set("Authorization", "Bearer adm-secret")
		rec := httptest.NewRecorder()
		serveWebRequest(s, rec, req)
		if rec.Code != tc.want {
			t.Errorf("Host %s = %d, want %d", tc.host, rec.Code, tc.want)
		}
	}
}

func TestAdminAuthSourceCapturedOncePerRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin.tok")
	if err := os.WriteFile(path, []byte("adm-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	src := webauth.NewSource(path)
	calls := 0
	s, err := New(Options{
		Reads:    &readAPIStub{dashboard: appapi.Dashboard{Counters: map[string]appapi.Metrics{}}},
		Commands: &testCommandAPI{},
		AdminAuth: func() *webauth.Source {
			calls++
			return src
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
	req.Header.Set("Authorization", "Bearer adm-secret")
	req.Header.Set("Origin", "http://192.0.2.10:8123")
	req.Host = "192.0.2.10:8123"
	rec := httptest.NewRecorder()
	serveWebRequest(s, rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if calls != 1 {
		t.Fatalf("AdminAuth closure calls = %d, want one request snapshot", calls)
	}
}

func TestAdminAuthDisabledKeepsLoopbackTrust(t *testing.T) {
	s, _ := newAuthedServer(t, false)
	rec := httptest.NewRecorder()
	serveWebRequest(s, rec, httptest.NewRequest("GET", "/api/status", nil))
	if rec.Code != 200 {
		t.Fatalf("auth off: /api/status = %d, want 200 (loopback-trust default)", rec.Code)
	}
}

func (r *readAPIStub) Presets() []presets.Preset { return r.presets }
