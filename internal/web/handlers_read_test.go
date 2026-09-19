package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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
	dashboard         appapi.Dashboard
	logFile           string
	logDir            string
	queries           appapi.RequestLogQueries
	accounts          []appapi.ProviderAccounts
	tokens            []appapi.TokenUsage
	agentRows         []appapi.AgentUsage
	tokensFrom        int64
	agentsFrom        int64
	statsSince        int64
	stats             func(appapi.StatsQuery) ([]observestats.Bucket, error)
	agents            func(appapi.AgentStatsQuery) ([]observestats.AgentBucket, error)
	analytics         func(appapi.AnalyticsQuery) ([]observestats.AnalyticsBucket, error)
	analyticsAgents   func(appapi.AnalyticsQuery) []string
	pricing           appapi.PricingSnapshot
	fusion            func(string, time.Time) (map[string]fusion.WorkflowStats, []fusion.Run)
	pins              []appapi.Pin
	security          func(appapi.SecurityQuery) (appapi.SecurityResult, error)
	explain           func(string, string, []string) (appapi.SecurityExplainResult, error)
	blocks            []appapi.SecurityBlock
	recent            []appapi.SecurityAdjudication
	adjudicationStats appapi.SecurityAdjudicationStats
	config            func() (appapi.ConfigDocument, error)
	presets           []presets.Preset
	mcpSurface        appapi.MCPSurface
	models            appapi.ModelsDocument
	takeover          func(string) (appapi.TakeoverSurface, error)
	takeoverPreview   func(appapi.TakeoverRunRequest, bool) (appapi.TakeoverPreview, error)
	takeoverTemplate  func(string) (appapi.TakeoverTemplateDoc, error)
}

func (r *readAPIStub) Dashboard(time.Time) appapi.Dashboard { return r.dashboard }
func (r *readAPIStub) LogFile() string                      { return r.logFile }
func (r *readAPIStub) RequestLogDirectory() string          { return r.logDir }
func (r *readAPIStub) RequestLogQueries() appapi.RequestLogQueries {
	return r.queries
}

// dirRequestLogQueries adapts a raw request-log directory to the query port
// through the scan functions — the handler tests pin JSON shapes, the
// index-backed equivalence lives in internal/observe/requestlog.
type dirRequestLogQueries struct{ dir string }

func (d dirRequestLogQueries) SummariesWithFacets(f requestlog.Filter) ([]requestlog.Summary, requestlog.Facets, error) {
	return requestlog.QuerySummariesWithFacets(d.dir, f)
}
func (d dirRequestLogQueries) ShadowReport(f requestlog.Filter) ([]requestlog.ShadowReportEntry, error) {
	return requestlog.ShadowReport(d.dir, f)
}
func (d dirRequestLogQueries) Detail(id, stream string) ([]requestlog.Record, error) {
	if stream == "mcp" {
		return nil, nil
	}
	return requestlog.QueryRecords(d.dir, requestlog.Filter{RequestID: id, Limit: 50})
}
func (d dirRequestLogQueries) SessionSummaries(scanLimit, limit int, costOf func(string, string, requestlog.Usage) float64) ([]requestlog.SessionSummary, error) {
	return requestlog.SessionSummaries(d.dir, scanLimit, limit, costOf)
}

// The raw directory adapter has no guard sources; the correlation join lives
// in the admin-decorated port.
func (d dirRequestLogQueries) GuardAnnotations([]string) map[string][]requestlog.GuardMark {
	return nil
}
func (r *readAPIStub) Accounts() []appapi.ProviderAccounts { return r.accounts }
func (r *readAPIStub) Tokens(from, to int64) ([]appapi.TokenUsage, error) {
	r.tokensFrom = from
	return r.tokens, nil
}
func (r *readAPIStub) Agents(from, to int64) ([]appapi.AgentUsage, error) {
	r.agentsFrom = from
	return r.agentRows, nil
}
func (r *readAPIStub) StatsSince() int64 { return r.statsSince }
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
func (r *readAPIStub) AnalyticsAgentNames(q appapi.AnalyticsQuery) []string {
	if r.analyticsAgents == nil {
		return nil
	}
	return r.analyticsAgents(q)
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
func (r *readAPIStub) SecurityExplain(requestID, kind string, names []string) (appapi.SecurityExplainResult, error) {
	if r.explain == nil {
		return appapi.SecurityExplainResult{}, nil
	}
	return r.explain(requestID, kind, names)
}
func (r *readAPIStub) SecurityAllowed() []appapi.SecurityAllowed { return nil }
func (r *readAPIStub) SecurityBlocks() []appapi.SecurityBlock {
	if r.blocks == nil {
		return []appapi.SecurityBlock{}
	}
	return r.blocks
}
func (r *readAPIStub) SecurityAdjudications() appapi.SecurityAdjudicationFeed {
	recent := r.recent
	if recent == nil {
		recent = []appapi.SecurityAdjudication{}
	}
	return appapi.SecurityAdjudicationFeed{Adjudications: recent, Stats: r.adjudicationStats}
}
func (r *readAPIStub) ConfigDocument() (appapi.ConfigDocument, error) {
	if r.config == nil {
		return appapi.ConfigDocument{}, nil
	}
	return r.config()
}

func newReadServerWithCommands(t *testing.T, reads *readAPIStub, commands *commandFake) *Server {
	t.Helper()
	return newReadServer(t, reads, func(o *Options) { o.Commands = commands })
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

// TestReadModelsEndpoint pins GET /api/models: 200 with the documented
// {providers:{name:{fingerprint,probed_at,models:{id:{chat,anthropic,responses}}}}}
// shape, and an empty store projecting {"providers":{}} (never null).
func TestReadModelsEndpoint(t *testing.T) {
	probed := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	reads := &readAPIStub{models: appapi.ModelsDocument{Providers: map[string]appapi.ProviderModelCaps{
		"up": {
			Fingerprint: "0123456789abcdef",
			ProbedAt:    probed,
			Models: map[string]appapi.ModelProtocols{
				"m1": {Chat: "yes", Anthropic: "no", Responses: "unknown"},
			},
		},
	}}}
	s := newReadServer(t, reads)

	rec := serveRead(t, s, http.MethodGet, "/api/models")
	var got struct {
		Providers map[string]struct {
			Fingerprint string `json:"fingerprint"`
			ProbedAt    string `json:"probed_at"`
			Models      map[string]struct {
				Chat      string `json:"chat"`
				Anthropic string `json:"anthropic"`
				Responses string `json:"responses"`
			} `json:"models"`
		} `json:"providers"`
	}
	decodeReadJSON(t, rec, &got)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/models = %d, want 200", rec.Code)
	}
	up, ok := got.Providers["up"]
	if !ok || up.Fingerprint != "0123456789abcdef" || up.ProbedAt != "2026-09-01T10:00:00Z" {
		t.Fatalf("providers[up] = %+v", got.Providers["up"])
	}
	if m := up.Models["m1"]; m.Chat != "yes" || m.Anthropic != "no" || m.Responses != "unknown" {
		t.Errorf("models[m1] = %+v, want yes/no/unknown", m)
	}

	// Empty store → {"providers":{}} (non-null object).
	reads.models = appapi.ModelsDocument{}
	rec = serveRead(t, s, http.MethodGet, "/api/models")
	if rec.Code != http.StatusOK {
		t.Fatalf("empty store: GET /api/models = %d, want 200", rec.Code)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != `{"providers":{}}` {
		t.Errorf("empty store body = %s, want {\"providers\":{}}", body)
	}

	// GET-only route: POST falls through to the JSON 404 like every read endpoint.
	if rec := serveRead(t, s, http.MethodPost, "/api/models"); rec.Code != http.StatusNotFound {
		t.Errorf("POST /api/models = %d, want 404 (no route for this method)", rec.Code)
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
		accounts:   []appapi.ProviderAccounts{{Name: "upstream", ProviderID: "p", Billing: "metered", Accounts: []appapi.Account{{ID: "a", Label: "primary"}}}},
		tokens:     []appapi.TokenUsage{{Provider: "p", Model: "m", Input: 3, Output: 4, CacheCreation: 1, CacheRead: 2, Total: 10, Requests: 1}},
		agentRows:  []appapi.AgentUsage{{Agent: "pi", Requests: 1, Input: 3, Output: 4, CacheCreation: 1, CacheRead: 2, Total: 10, Models: []appapi.AgentModelUsage{{Provider: "p", Model: "m", Requests: 1, Input: 3, Output: 4, CacheCreation: 1, CacheRead: 2, Total: 10}}}},
		statsSince: 1788874500,
		fusion: func(workflow string, _ time.Time) (map[string]fusion.WorkflowStats, []fusion.Run) {
			if workflow != "judge" {
				t.Fatalf("fusion workflow = %q, want judge", workflow)
			}
			return map[string]fusion.WorkflowStats{"judge": {Runs: 2}}, []fusion.Run{{RunID: "run-1", Workflow: "judge"}}
		},
		pins: []appapi.Pin{{Route: "chat", Provider: "p", ExpiresAt: expires}, {Route: "all", Provider: "fallback"}},
		config: func() (appapi.ConfigDocument, error) {
			threshold := 7
			return appapi.ConfigDocument{YAML: "listen: :8317\n", Summary: appapi.ConfigSummary{Listen: ":8317", ProviderCount: 1, RouteCount: 2}, ProviderModels: map[string][]string{"p": {"m"}}, Routes: map[string][]appapi.ConfigRouteTarget{"chat": {{Provider: "p", Model: "m", Priority: 3}}}, Settings: appapi.ConfigSettings{LogLevel: "warn", Scheduling: appapi.ConfigScheduling{CircuitThreshold: &threshold}}}, nil
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
		Usage  []appapi.TokenUsage `json:"usage"`
		Agents []appapi.AgentUsage `json:"agents"`
		Since  int64               `json:"since"`
		Window string              `json:"window"`
	}
	decodeReadJSON(t, tokens, &gotTokens)
	if tokens.Code != http.StatusOK || len(gotTokens.Usage) != 1 ||
		gotTokens.Usage[0] != (appapi.TokenUsage{Provider: "p", Model: "m", Input: 3, Output: 4, CacheCreation: 1, CacheRead: 2, Total: 10, Requests: 1}) {
		t.Fatalf("tokens = %#v", gotTokens)
	}
	wantAgent := appapi.AgentUsage{Agent: "pi", Requests: 1, Input: 3, Output: 4, CacheCreation: 1, CacheRead: 2, Total: 10,
		Models: []appapi.AgentModelUsage{{Provider: "p", Model: "m", Requests: 1, Input: 3, Output: 4, CacheCreation: 1, CacheRead: 2, Total: 10}}}
	if len(gotTokens.Agents) != 1 || !reflect.DeepEqual(gotTokens.Agents[0], wantAgent) {
		t.Fatalf("tokens.agents = %#v", gotTokens.Agents)
	}
	if gotTokens.Window != "all" || reads.tokensFrom != 0 || reads.agentsFrom != 0 {
		t.Errorf("default tokens window = %q (from %d/%d), want \"all\" with from=0",
			gotTokens.Window, reads.tokensFrom, reads.agentsFrom)
	}

	// ?window=1h switches both projections to the persisted-bucket view
	// (from > 0, minute-truncated) and echoes the window; ?window=all is the
	// explicit form of the default; an unknown window is rejected 400.
	windowed := serveRead(t, s, http.MethodGet, "/api/tokens?window=1h")
	var gotWindowed struct {
		Window string `json:"window"`
	}
	decodeReadJSON(t, windowed, &gotWindowed)
	if windowed.Code != http.StatusOK || gotWindowed.Window != "1h" {
		t.Fatalf("windowed tokens = (%d, %#v)", windowed.Code, gotWindowed)
	}
	if reads.tokensFrom <= 0 || reads.agentsFrom <= 0 {
		t.Errorf("window=1h from = %d/%d, want > 0", reads.tokensFrom, reads.agentsFrom)
	}
	if reads.tokensFrom%60 != 0 {
		t.Errorf("window start %d not truncated to a minute boundary", reads.tokensFrom)
	}
	if rec := serveRead(t, s, http.MethodGet, "/api/tokens?window=all"); rec.Code != http.StatusOK || reads.tokensFrom != 0 {
		t.Errorf("window=all = (%d, from %d), want 200 with from=0", rec.Code, reads.tokensFrom)
	}
	badWindow := serveRead(t, s, http.MethodGet, "/api/tokens?window=fortnight")
	var badWindowBody struct {
		Error string `json:"error"`
	}
	decodeReadJSON(t, badWindow, &badWindowBody)
	if badWindow.Code != http.StatusBadRequest || !strings.Contains(badWindowBody.Error, "window") {
		t.Errorf("unknown window = (%d, %#v), want 400 naming the window contract", badWindow.Code, badWindowBody)
	}

	// from/to is the range form of the selector: unix seconds or RFC3339
	// (same parser as /api/stats), from truncated down to the minute
	// boundary. Unparseable bounds, from > to, and window+from/to combos are
	// all 400.
	ranged := serveRead(t, s, http.MethodGet, "/api/tokens?from=1788874517&to=1788878100")
	var rangedBody struct {
		From int64 `json:"from"`
		To   int64 `json:"to"`
	}
	decodeReadJSON(t, ranged, &rangedBody)
	if ranged.Code != http.StatusOK {
		t.Fatalf("from/to tokens = %d", ranged.Code)
	}
	if rangedBody.From != 1788874500 || rangedBody.To != 1788878100 {
		t.Errorf("range echo = (%d, %d), want (1788874500, 1788878100)", rangedBody.From, rangedBody.To)
	}
	if reads.tokensFrom != 1788874500 || reads.agentsFrom != 1788874500 {
		t.Errorf("from=1788874517 truncated to %d/%d, want minute boundary 1788874500",
			reads.tokensFrom, reads.agentsFrom)
	}
	rfc := serveRead(t, s, http.MethodGet, "/api/tokens?from=2026-09-08T00:00:00Z")
	if rfc.Code != http.StatusOK || reads.tokensFrom != 1788825600 {
		t.Errorf("RFC3339 from = (%d, %d), want (200, 1788825600)", rfc.Code, reads.tokensFrom)
	}
	for _, url := range []string{
		"/api/tokens?from=soon",
		"/api/tokens?to=tomorrow",
		"/api/tokens?from=200&to=100",
		"/api/tokens?window=1h&from=100",
		"/api/tokens?window=1h&to=100",
	} {
		rec := serveRead(t, s, http.MethodGet, url)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", url, rec.Code)
		}
	}
	if gotTokens.Since != 1788874500 {
		t.Fatalf("tokens.since = %d, want 1788874500", gotTokens.Since)
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
	// The scalar settings projection must survive the handler's explicit
	// field mapping (docs/web-api.md GET /api/config).
	if gotConfig.Settings.LogLevel != "warn" || gotConfig.Settings.Scheduling.CircuitThreshold == nil || *gotConfig.Settings.Scheduling.CircuitThreshold != 7 {
		t.Fatalf("config settings = %#v", gotConfig.Settings)
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
	reads := &readAPIStub{logFile: "ignored", queries: dirRequestLogQueries{tmp}}
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
		requestlog.Record{Ts: "2026-07-29T12:01:00Z", RequestID: "wanted", Shadow: true, Agent: "claude-code", CalledModel: "Model-X", Provider: "Provider-X", Status: 500, RequestBody: "secret", ResponseBody: "reply", ResponseHeaders: `{"x-request-id":"x"}`},
	)
	list := serveRead(t, s, http.MethodGet, "/api/requests?model=model-x&provider=provider-x&errors=1&shadow=only&status=500&limit=5000&from=2026-07-29T12:00:30Z&to=2026-07-29T12:02:00Z")
	var gotList struct {
		Enabled bool                 `json:"enabled"`
		Records []requestlog.Summary `json:"records"`
		Facets  requestlog.Facets    `json:"facets"`
	}
	decodeReadJSON(t, list, &gotList)
	if list.Code != http.StatusOK || !gotList.Enabled || len(gotList.Records) != 1 || gotList.Records[0].RequestID != "wanted" || gotList.Records[0].Status != 500 {
		t.Fatalf("request list = %#v", gotList)
	}
	if gotList.Records[0].Agent != "claude-code" {
		t.Errorf("list summary agent = %q, want claude-code", gotList.Records[0].Agent)
	}
	// Facets are data-driven (distinct log values), NOT the config catalog, and
	// must not be narrowed by the request's own model/provider filter.
	if len(gotList.Facets.Providers) != 2 || len(gotList.Facets.Models) != 2 {
		t.Fatalf("request facets = %#v, want both providers/models despite the filter", gotList.Facets)
	}
	if len(gotList.Facets.ProviderModels["Provider-X"]) != 1 || gotList.Facets.ProviderModels["Provider-X"][0] != "Model-X" {
		t.Fatalf("provider_models facet = %#v", gotList.Facets.ProviderModels)
	}
	if len(gotList.Facets.Agents) != 1 || gotList.Facets.Agents[0] != "claude-code" {
		t.Fatalf("agents facet = %#v, want the one observed label (the agent-less record contributes none)", gotList.Facets.Agents)
	}
	if strings.Contains(list.Body.String(), "secret") || strings.Contains(list.Body.String(), "reply") || strings.Contains(list.Body.String(), "x-request-id") {
		t.Fatalf("request list leaked detail data: %s", list.Body.String())
	}

	// agent= is an exact-match filter wired through to requestlog.Filter.
	byAgent := serveRead(t, s, http.MethodGet, "/api/requests?agent=claude-code")
	var gotAgent struct {
		Records []requestlog.Summary `json:"records"`
	}
	decodeReadJSON(t, byAgent, &gotAgent)
	if byAgent.Code != http.StatusOK || len(gotAgent.Records) != 1 || gotAgent.Records[0].RequestID != "wanted" {
		t.Fatalf("agent filter = (%d, %#v)", byAgent.Code, gotAgent.Records)
	}
	partialAgent := serveRead(t, s, http.MethodGet, "/api/requests?agent=claude")
	decodeReadJSON(t, partialAgent, &gotAgent)
	if partialAgent.Code != http.StatusOK || len(gotAgent.Records) != 0 {
		t.Fatalf("partial agent filter = (%d, %#v), want no rows (exact match)", partialAgent.Code, gotAgent.Records)
	}

	detail := serveRead(t, s, http.MethodGet, "/api/requests/wanted")
	var gotDetail struct {
		Records []requestlog.Record    `json:"records"`
		Guard   []requestlog.GuardMark `json:"guard"`
	}
	decodeReadJSON(t, detail, &gotDetail)
	if detail.Code != http.StatusOK || len(gotDetail.Records) != 1 || gotDetail.Records[0].RequestBody != "secret" || gotDetail.Records[0].ResponseHeaders != `{"x-request-id":"x"}` {
		t.Fatalf("request detail = %#v", gotDetail)
	}
	if gotDetail.Records[0].Agent != "claude-code" {
		t.Errorf("detail record agent = %q, want claude-code", gotDetail.Records[0].Agent)
	}
	// The guard envelope is always present on the detail surface — an empty
	// array when the request has no guard trail (nil map coerced, never null).
	if gotDetail.Guard == nil || len(gotDetail.Guard) != 0 {
		t.Errorf("detail guard = %#v, want empty non-null", gotDetail.Guard)
	}
	missing := serveRead(t, s, http.MethodGet, "/api/requests/nope")
	var routeError struct {
		Error string `json:"error"`
	}
	decodeReadJSON(t, missing, &routeError)
	if missing.Code != http.StatusNotFound || routeError.Error != "no record for request id nope" {
		t.Fatalf("missing detail = (%d, %#v)", missing.Code, routeError)
	}

	reads.queries = nil
	disabled := serveRead(t, s, http.MethodGet, "/api/requests")
	var gotDisabled struct {
		Enabled bool              `json:"enabled"`
		Records []any             `json:"records"`
		Facets  requestlog.Facets `json:"facets"`
	}
	decodeReadJSON(t, disabled, &gotDisabled)
	// The contract is a NON-NULL empty array: `len(...) != 0` alone would also
	// pass a JSON null (nil slice), so pin the raw bytes too.
	if disabled.Code != http.StatusOK || gotDisabled.Enabled || gotDisabled.Records == nil {
		t.Fatalf("disabled request logging = %#v (records must be a non-null empty array)", gotDisabled)
	}
	if body := disabled.Body.String(); !strings.Contains(body, `"records":[]`) || !strings.Contains(body, `"providers":[]`) || !strings.Contains(body, `"agents":[]`) {
		t.Fatalf("disabled request logging body must serialize records/facets as []: %s", body)
	}
	noDetail := serveRead(t, s, http.MethodGet, "/api/requests/")
	decodeReadJSON(t, noDetail, &routeError)
	if noDetail.Code != http.StatusNotFound || routeError.Error != "request logging is off or no id given" {
		t.Fatalf("disabled detail = (%d, %#v)", noDetail.Code, routeError)
	}
	reads.queries = dirRequestLogQueries{filepath.Join(t.TempDir(), "missing")}
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

// TestReadRequestsSessionFilter: the /api/requests?session= param narrows the
// request-log scan to one client session.
// guardJoinStubQueries pins the detail surface's guard envelope passthrough:
// whatever the decorated port joins for the request id rides the response
// next to the records.
type guardJoinStubQueries struct {
	dirRequestLogQueries
	marks map[string][]requestlog.GuardMark
}

func (g guardJoinStubQueries) GuardAnnotations(ids []string) map[string][]requestlog.GuardMark {
	out := make(map[string][]requestlog.GuardMark, len(ids))
	for _, id := range ids {
		if m, ok := g.marks[id]; ok {
			out[id] = m
		}
	}
	return out
}

func TestReadRequestDetailCarriesGuardTrail(t *testing.T) {
	tmp := t.TempDir()
	writeReadLog(t, tmp,
		requestlog.Record{Ts: "2026-07-29T12:00:00Z", RequestID: "r1", CalledModel: "m", Provider: "p", Status: 400, RequestBody: "b", ResponseBody: "blocked"},
	)
	s := newReadServer(t, &readAPIStub{queries: guardJoinStubQueries{
		dirRequestLogQueries: dirRequestLogQueries{tmp},
		marks: map[string][]requestlog.GuardMark{
			"r1": {
				{Ts: 4000, Kind: "secret", Names: []string{"openai_api_key"}, Action: "block", Source: "audit"},
				{Ts: 3000, Kind: "secret", Names: []string{"openai_api_key"}, Verdict: "high", Reason: "real key", Model: "glm-5.3", Source: "judge", Cached: true},
			},
		},
	}})
	detail := serveRead(t, s, http.MethodGet, "/api/requests/r1")
	var got struct {
		Records []requestlog.Record    `json:"records"`
		Guard   []requestlog.GuardMark `json:"guard"`
	}
	decodeReadJSON(t, detail, &got)
	if detail.Code != http.StatusOK || len(got.Records) != 1 {
		t.Fatalf("detail = (%d, %d records)", detail.Code, len(got.Records))
	}
	if len(got.Guard) != 2 {
		t.Fatalf("guard = %#v, want the joined marks", got.Guard)
	}
	if got.Guard[0].Action != "block" || got.Guard[1].Verdict != "high" || got.Guard[1].Model != "glm-5.3" || !got.Guard[1].Cached {
		t.Errorf("guard marks = %#v", got.Guard)
	}
}

func TestReadRequestsSessionFilter(t *testing.T) {
	tmp := t.TempDir()
	writeReadLog(t, tmp,
		requestlog.Record{Ts: "2026-07-29T12:00:00Z", RequestID: "a", SessionID: "sess-a", CalledModel: "m", Provider: "p", Status: 200},
		requestlog.Record{Ts: "2026-07-29T12:01:00Z", RequestID: "b", SessionID: "sess-b", CalledModel: "m", Provider: "p", Status: 200},
	)
	s := newReadServer(t, &readAPIStub{queries: dirRequestLogQueries{tmp}})
	list := serveRead(t, s, http.MethodGet, "/api/requests?session=sess-b")
	var got struct {
		Records []requestlog.Summary `json:"records"`
	}
	decodeReadJSON(t, list, &got)
	if list.Code != http.StatusOK || len(got.Records) != 1 || got.Records[0].RequestID != "b" || got.Records[0].SessionID != "sess-b" {
		t.Fatalf("session-filtered requests = %#v", got)
	}
}

func TestReadShadowReportAndErrors(t *testing.T) {
	tmp := t.TempDir()
	writeReadLog(t, tmp,
		requestlog.Record{Ts: "2026-07-29T12:00:00Z", RequestID: "pair", Exposed: "chat", Provider: "primary", Status: 200, LatencyMs: 11, ResponseSize: 100},
		requestlog.Record{Ts: "2026-07-29T12:00:01Z", RequestID: "shadow-pair", Shadow: true, Exposed: "chat", Provider: "shadow", Status: 500, LatencyMs: 18, ResponseSize: 70},
	)
	reads := &readAPIStub{logDir: tmp, queries: dirRequestLogQueries{tmp}}
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
	reads.queries = dirRequestLogQueries{reads.logDir}
	failed := serveRead(t, s, http.MethodGet, "/api/shadow-report")
	var routeError struct {
		Error string `json:"error"`
	}
	decodeReadJSON(t, failed, &routeError)
	if failed.Code != http.StatusInternalServerError || !strings.HasPrefix(routeError.Error, "shadow report: ") {
		t.Fatalf("shadow error = (%d, %#v)", failed.Code, routeError)
	}
}

// yearWindowBounds mirrors the handler's heatmap window (the 1st of the
// month 12 months back, local) with one day of slop on each side.
type yearWindow struct{ lo, hi int64 }

func (y yearWindow) contains(v int64) bool { return v >= y.lo && v <= y.hi }

func yearWindowBounds(now time.Time) yearWindow {
	first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.Local).AddDate(0, -12, 0)
	return yearWindow{lo: first.Add(-24 * time.Hour).Unix(), hi: first.Add(24 * time.Hour).Unix()}
}

func TestReadStatsAgentsAndAnalyticsQueries(t *testing.T) {
	var statsQuery appapi.StatsQuery
	var agentQuery appapi.AgentStatsQuery
	var analyticsQueries []appapi.AnalyticsQuery
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
			analyticsQueries = append(analyticsQueries, q)
			return []observestats.AnalyticsBucket{
				// AvgDurationMs is store-derived (duration_sum/requests); the fake
				// fills both fields the way the real projection does.
				{Provider: "p", Model: "priced", Bucket: 100, Requests: 2, Failovers: 1, RateLimited429: 2, Input: 10, Output: 5, CacheRead: 2, CacheCreation: 1, DurationSum: 4000, AvgDurationMs: 2000},
				{Provider: "p", Model: "unknown", Bucket: 200, Requests: 1, Failovers: 3, Input: 7, Output: 3},
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

	// real was a Status→Dashboard-only filter and was removed with that view;
	// the endpoint keeps its CLI contract unchanged.
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
	// The unified derived-metric block (same shape on totals, compare and
	// each series) is the single authority for tokens/tok-s/cache-hit/err%.
	var analyticsResponse struct {
		Granularity string `json:"granularity"`
		By          string `json:"by"`
		Totals      struct {
			Input         uint64   `json:"input"`
			Output        uint64   `json:"output"`
			Tokens        uint64   `json:"tokens"`
			Failovers     uint64   `json:"failovers"`
			RateLimited42 uint64   `json:"rate_limited_429"`
			TokSec        *float64 `json:"tok_sec"`
			CacheHitPct   *float64 `json:"cache_hit_pct"`
			ErrPct        *float64 `json:"err_pct"`
			Cost          *float64 `json:"cost"`
		} `json:"totals"`
		Compare struct {
			From     int64    `json:"from"`
			To       int64    `json:"to"`
			Requests uint64   `json:"requests"`
			Tokens   uint64   `json:"tokens"`
			TokSec   *float64 `json:"tok_sec"`
			Cost     *float64 `json:"cost"`
		} `json:"compare"`
		Coverage struct {
			Priced   []providerModelPair `json:"priced"`
			Unpriced []providerModelPair `json:"unpriced"`
		} `json:"price_coverage"`
		Series []struct {
			Model  string `json:"model"`
			Totals struct {
				Requests uint64   `json:"requests"`
				Tokens   uint64   `json:"tokens"`
				TokSec   *float64 `json:"tok_sec"`
				ErrPct   *float64 `json:"err_pct"`
			} `json:"totals"`
			Points []struct {
				Tokens        uint64   `json:"tokens"`
				AvgDurationMs float64  `json:"avg_duration_ms"`
				TokSec        *float64 `json:"tok_sec"`
				CacheHitPct   *float64 `json:"cache_hit_pct"`
				ErrPct        *float64 `json:"err_pct"`
				Cost          *float64 `json:"cost"`
				Priced        bool     `json:"priced"`
			} `json:"points"`
		} `json:"series"`
	}
	decodeReadJSON(t, analytics, &analyticsResponse)
	// The handler issues THREE port calls: the requested window, the
	// equal-length comparison window immediately before it (one minute
	// earlier so the inclusive minute bounds never overlap), and the FIXED
	// trailing-year heatmap window at day granularity (from ≈ now-365d).
	wantQueries := []appapi.AnalyticsQuery{
		{From: 100, To: 200, Provider: "p", Model: "all", Granularity: "month", By: "model"},
		{From: -60, To: 40, Provider: "p", Model: "all", Granularity: "month", By: "model"},
	}
	var yearQuery *appapi.AnalyticsQuery
	windowed := []appapi.AnalyticsQuery{}
	for _, q := range analyticsQueries {
		if q.Granularity == "day" {
			yearQuery = &q
			continue
		}
		windowed = append(windowed, q)
	}
	if yearQuery == nil {
		t.Fatalf("no trailing-year heatmap read among queries: %#v", analyticsQueries)
	}
	// Window totals fold BOTH buckets: tokens = 17+8+1+2 = 28 (four-bucket),
	// tok/s = 8 output / 4s call time = 2, cache hit = 2/20 reads = 10%,
	// err% = 0 with requests (non-null), cost = $0.03 (priced bucket only).
	if analytics.Code != http.StatusOK || !reflect.DeepEqual(windowed, wantQueries) ||
		yearQuery.Provider != "p" || yearQuery.Model != "all" || yearQuery.By != "model" ||
		!yearWindowBounds(time.Now()).contains(yearQuery.From) ||
		analyticsResponse.Granularity != "month" || analyticsResponse.By != "model" ||
		analyticsResponse.Totals.Input != 17 || analyticsResponse.Totals.Output != 8 || analyticsResponse.Totals.Tokens != 28 ||
		analyticsResponse.Totals.TokSec == nil || math.Abs(*analyticsResponse.Totals.TokSec-2) > 1e-9 ||
		analyticsResponse.Totals.CacheHitPct == nil || math.Abs(*analyticsResponse.Totals.CacheHitPct-10) > 1e-9 ||
		analyticsResponse.Totals.Failovers != 4 || analyticsResponse.Totals.RateLimited42 != 2 ||
		analyticsResponse.Totals.ErrPct == nil || *analyticsResponse.Totals.ErrPct != 0 ||
		analyticsResponse.Totals.Cost == nil || math.Abs(*analyticsResponse.Totals.Cost-.03) > 1e-12 ||
		analyticsResponse.Compare.From != -60 || analyticsResponse.Compare.To != 40 || analyticsResponse.Compare.Requests != 3 || analyticsResponse.Compare.Tokens != 28 ||
		analyticsResponse.Compare.TokSec == nil || math.Abs(*analyticsResponse.Compare.TokSec-2) > 1e-9 ||
		!reflect.DeepEqual(analyticsResponse.Coverage.Priced, []providerModelPair{{Provider: "p", Model: "priced"}}) ||
		!reflect.DeepEqual(analyticsResponse.Coverage.Unpriced, []providerModelPair{{Provider: "p", Model: "unknown"}}) ||
		len(analyticsResponse.Series) != 2 || !analyticsResponse.Series[0].Points[0].Priced || analyticsResponse.Series[1].Points[0].Cost != nil {
		t.Fatalf("analytics queries=%#v response=%#v", analyticsQueries, analyticsResponse)
	}
	// Per-series unified block: the priced series folds only its own bucket
	// (requests 2, tokens 18, tok/s = 5 out / 4s = 1.25).
	if analyticsResponse.Series[0].Totals.Requests != 2 || analyticsResponse.Series[0].Totals.Tokens != 18 ||
		analyticsResponse.Series[0].Totals.TokSec == nil || math.Abs(*analyticsResponse.Series[0].Totals.TokSec-1.25) > 1e-9 {
		t.Fatalf("series totals = %#v", analyticsResponse.Series[0].Totals)
	}
	// Per-point derived fields + the full-call duration (the tok/s source)
	// must survive the transport projection: 4000ms over 2 requests → 2000ms
	// avg; tokens 18; tok/s 1.25; err% 0 (non-null with requests).
	pt := analyticsResponse.Series[0].Points[0]
	if pt.AvgDurationMs != 2000 || pt.Tokens != 18 || pt.TokSec == nil || math.Abs(*pt.TokSec-1.25) > 1e-9 || pt.ErrPct == nil || *pt.ErrPct != 0 {
		t.Fatalf("point derived fields = %#v", pt)
	}

	invalid := serveRead(t, s, http.MethodGet, "/api/analytics?granularity=year")
	var routeError struct {
		Error string `json:"error"`
	}
	decodeReadJSON(t, invalid, &routeError)
	if invalid.Code != http.StatusBadRequest || routeError.Error != "granularity must be minute, hour, day, week or month" {
		t.Fatalf("invalid analytics = (%d, %#v)", invalid.Code, routeError)
	}
	invalidBy := serveRead(t, s, http.MethodGet, "/api/analytics?by=route")
	decodeReadJSON(t, invalidBy, &routeError)
	if invalidBy.Code != http.StatusBadRequest || routeError.Error != "by must be model or agent" {
		t.Fatalf("invalid by = (%d, %#v)", invalidBy.Code, routeError)
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

// TestReadAnalyticsPriceCoverageByProviderModel pins the (provider, model)
// granularity of price_coverage: pricing resolves through a provider-scoped
// alias fallback, so one upstream model can be priced under one provider and
// unpriced under another — a model-only key would list it in both buckets.
func TestReadAnalyticsPriceCoverageByProviderModel(t *testing.T) {
	reads := &readAPIStub{
		analytics: func(appapi.AnalyticsQuery) ([]observestats.AnalyticsBucket, error) {
			return []observestats.AnalyticsBucket{
				{Provider: "p", Model: "k3", Bucket: 100, Requests: 1, Input: 10, Output: 5},
				{Provider: "q", Model: "k3", Bucket: 100, Requests: 1, Input: 10, Output: 5},
			}, nil
		},
		pricing: appapi.PricingSnapshot{
			Catalog: &pricing.Catalog{ByModel: map[string]pricing.Entry{"kimi-k3": {Prompt: .001, Completion: .002}}},
			Aliases: map[string]string{pricing.AliasKey("p", "k3"): "kimi-k3"},
		},
	}
	s := newReadServer(t, reads)
	resp := serveRead(t, s, http.MethodGet, "/api/analytics?from=100&to=200")
	var out struct {
		Coverage struct {
			Priced   []providerModelPair `json:"priced"`
			Unpriced []providerModelPair `json:"unpriced"`
		} `json:"price_coverage"`
	}
	decodeReadJSON(t, resp, &out)
	if resp.Code != http.StatusOK ||
		!reflect.DeepEqual(out.Coverage.Priced, []providerModelPair{{Provider: "p", Model: "k3"}}) ||
		!reflect.DeepEqual(out.Coverage.Unpriced, []providerModelPair{{Provider: "q", Model: "k3"}}) {
		t.Fatalf("price coverage = (%d, %#v)", resp.Code, out.Coverage)
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
	if got := sortedProviderModels(nil); len(got) != 0 || got == nil {
		t.Fatalf("sortedProviderModels(nil) = %#v, want non-nil empty", got)
	}
	if got := sortedProviderModels(map[providerModelPair]bool{
		{Provider: "q", Model: "m"}: true, {Provider: "p", Model: "z"}: true, {Provider: "p", Model: "a"}: true,
	}); fmt.Sprintf("%v", got) != "[{p a} {p z} {q m}]" {
		t.Fatalf("sorted provider/model pairs = %v", got)
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
	if invalid.Code != http.StatusBadRequest || routeError.Error != "kind must be secret, path, drift or unblock" {
		t.Fatalf("invalid kind = (%d, %#v)", invalid.Code, routeError)
	}
	// The unblock trail is a first-class kind on the filter surface.
	serveRead(t, s, http.MethodGet, "/api/security?kind=unblock")
	if securityQuery != (appapi.SecurityQuery{Kind: "unblock", Limit: 100}) {
		t.Fatalf("unblock query = %#v", securityQuery)
	}

	// Verdict-counts failure (records loaded, aggregation failed): the wire
	// carries counts_error and omits counts, so the client renders
	// "unavailable" instead of silent zeros.
	reads.security = func(appapi.SecurityQuery) (appapi.SecurityResult, error) {
		return appapi.SecurityResult{
			Enabled:     true,
			Records:     []appapi.SecurityRecord{{Ts: 1700000000123, Kind: "secret"}},
			CountsError: "verdict counts unavailable",
		}, nil
	}
	degraded := serveRead(t, s, http.MethodGet, "/api/security")
	var degradedResponse struct {
		Enabled     bool                          `json:"enabled"`
		Counts      *appapi.SecurityVerdictCounts `json:"counts"`
		CountsError string                        `json:"counts_error"`
		Records     []appapi.SecurityRecord       `json:"records"`
	}
	decodeReadJSON(t, degraded, &degradedResponse)
	if degraded.Code != http.StatusOK || !degradedResponse.Enabled || degradedResponse.Counts != nil ||
		degradedResponse.CountsError != "verdict counts unavailable" || len(degradedResponse.Records) != 1 {
		t.Fatalf("degraded security = (%d, %#v)", degraded.Code, degradedResponse)
	}
	if !strings.Contains(degraded.Body.String(), `"counts_error"`) || strings.Contains(degraded.Body.String(), `"counts"`) {
		t.Fatalf("degraded security wire must carry counts_error and omit counts: %s", degraded.Body.String())
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

func TestReadSecurityExplain(t *testing.T) {
	var gotID, gotKind string
	var gotNames []string
	reads := &readAPIStub{
		explain: func(requestID, kind string, names []string) (appapi.SecurityExplainResult, error) {
			gotID, gotKind, gotNames = requestID, kind, names
			return appapi.SecurityExplainResult{
				Status:    appapi.SecurityExplainOK,
				RequestID: requestID,
				Kind:      kind,
				Matches: []appapi.SecurityMatch{{
					Name: "aws_creds", Strength: "strong", Located: true,
					Pre: "cat ", Hit: "~/.aws/credentials", Post: "",
					Explanation: "Reference to ~/.aws/credentials.",
				}},
			}, nil
		},
	}
	s := newReadServer(t, reads)

	ok := serveRead(t, s, http.MethodGet, "/api/security/explain?request_id=r1&kind=path&name=aws_creds,%20ssh%20")
	var response appapi.SecurityExplainResult
	decodeReadJSON(t, ok, &response)
	if ok.Code != http.StatusOK || gotID != "r1" || gotKind != "path" ||
		len(gotNames) != 2 || gotNames[0] != "aws_creds" || gotNames[1] != "ssh" {
		t.Fatalf("explain = (%d, id=%q kind=%q names=%v)", ok.Code, gotID, gotKind, gotNames)
	}
	if response.Status != "ok" || len(response.Matches) != 1 ||
		response.Matches[0].Strength != "strong" || !response.Matches[0].Located {
		t.Fatalf("explain response = %#v", response)
	}

	var routeError struct {
		Error string `json:"error"`
	}
	for _, url := range []string{
		"/api/security/explain?request_id=r1&kind=drift&name=x",
		"/api/security/explain?request_id=r1&kind=bogus&name=x",
		"/api/security/explain?kind=secret&name=x",
		"/api/security/explain?request_id=r1&kind=secret",
	} {
		resp := serveRead(t, s, http.MethodGet, url)
		decodeReadJSON(t, resp, &routeError)
		if resp.Code != http.StatusBadRequest || routeError.Error == "" {
			t.Fatalf("%s = (%d, %#v), want 400 with a message", url, resp.Code, routeError)
		}
	}

	reads.explain = func(string, string, []string) (appapi.SecurityExplainResult, error) {
		return appapi.SecurityExplainResult{}, errors.New("request log unreadable")
	}
	failure := serveRead(t, s, http.MethodGet, "/api/security/explain?request_id=r1&kind=secret&name=openai_api_key")
	decodeReadJSON(t, failure, &routeError)
	// Same convention as /api/security: no underlying error detail leaks.
	if failure.Code != http.StatusInternalServerError || routeError.Error != "failed to analyze security record" {
		t.Fatalf("explain failure = (%d, %#v)", failure.Code, routeError)
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

func (r *readAPIStub) Presets() []presets.Preset     { return r.presets }
func (r *readAPIStub) MCPSurface() appapi.MCPSurface { return r.mcpSurface }

func (r *readAPIStub) ModelsDocument() appapi.ModelsDocument {
	if r.models.Providers == nil {
		return appapi.ModelsDocument{Providers: map[string]appapi.ProviderModelCaps{}}
	}
	return r.models
}

func (r *readAPIStub) TakeoverSurface(mode string) (appapi.TakeoverSurface, error) {
	if r.takeover != nil {
		return r.takeover(mode)
	}
	return appapi.TakeoverSurface{Clients: []appapi.TakeoverClient{}}, nil
}

func (r *readAPIStub) PreviewTakeover(req appapi.TakeoverRunRequest, managedOnly bool) (appapi.TakeoverPreview, error) {
	if r.takeoverPreview != nil {
		return r.takeoverPreview(req, managedOnly)
	}
	return appapi.TakeoverPreview{Writes: []appapi.TakeoverPreviewWrite{}}, nil
}

func (r *readAPIStub) TakeoverTemplate(name string) (appapi.TakeoverTemplateDoc, error) {
	if r.takeoverTemplate != nil {
		return r.takeoverTemplate(name)
	}
	return appapi.TakeoverTemplateDoc{}, appapi.NewHTTPError(http.StatusNotFound, "unknown takeover template: "+name)
}

// TestSecurityBlocksAndAdjudications covers the guard AI-adjudication
// surfaces: the blocks list, the recent-verdict ring, and the DELETE unblock
// route (404 for an unknown session, 200 + cleared state for a blocked one).
func TestSecurityBlocksAndAdjudications(t *testing.T) {
	reads := &readAPIStub{}
	commands := &commandFake{}
	s := newReadServerWithCommands(t, reads, commands)

	ok := serveRead(t, s, http.MethodGet, "/api/security/adjudications")
	var adj struct {
		Adjudications []appapi.SecurityAdjudication    `json:"adjudications"`
		Stats         appapi.SecurityAdjudicationStats `json:"stats"`
	}
	decodeReadJSON(t, ok, &adj)
	if ok.Code != http.StatusOK || len(adj.Adjudications) != 0 {
		t.Fatalf("adjudications = (%d, %+v), want 200 + empty list", ok.Code, adj.Adjudications)
	}
	if adj.Stats.Calls != 0 {
		t.Fatalf("stats = %+v, want zero-valued when no port data", adj.Stats)
	}
	// The LLM usage stats ride the same payload.
	reads.adjudicationStats = appapi.SecurityAdjudicationStats{Calls: 9, InputTokens: 100, OutputTokens: 5}
	ok = serveRead(t, s, http.MethodGet, "/api/security/adjudications")
	decodeReadJSON(t, ok, &adj)
	if ok.Code != http.StatusOK || adj.Stats.Calls != 9 || adj.Stats.InputTokens != 100 || adj.Stats.OutputTokens != 5 {
		t.Fatalf("stats passthrough = (%d, %+v)", ok.Code, adj.Stats)
	}

	// DELETE on a session the fake does not know → the admin 404 error maps
	// through writePortErr (HTTPError status).
	commands.unblock = func(sid string) error {
		return &appapi.HTTPError{Status: http.StatusNotFound, Message: "session " + sid + " is not blocked"}
	}
	missing := serveRead(t, s, http.MethodDelete, "/api/security/blocks/nope")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("unknown unblock status = %d, want 404", missing.Code)
	}

	// A blocked session lists and unblocks. (Field assignment instead of a
	// composite literal: SecurityBlock embeds adjudicate.Block, and promoted
	// fields are not allowed in literals.)
	blocked := appapi.SecurityBlock{SessionID: "s-1"}
	blocked.Kind, blocked.Rule, blocked.Ts = "secret", "jwt", 7
	reads.blocks = []appapi.SecurityBlock{blocked}
	commands.unblock = func(sid string) error {
		if sid != "s-1" {
			t.Errorf("unblock session = %q, want s-1", sid)
		}
		reads.blocks = nil
		return nil
	}
	ok = serveRead(t, s, http.MethodGet, "/api/security/blocks")
	var list struct {
		Blocks []appapi.SecurityBlock `json:"blocks"`
	}
	decodeReadJSON(t, ok, &list)
	if ok.Code != http.StatusOK || len(list.Blocks) != 1 || list.Blocks[0].SessionID != "s-1" {
		t.Fatalf("blocks = (%d, %+v)", ok.Code, list.Blocks)
	}
	ok = serveRead(t, s, http.MethodDelete, "/api/security/blocks/s-1")
	var unblocked struct {
		Status string `json:"status"`
	}
	decodeReadJSON(t, ok, &unblocked)
	if ok.Code != http.StatusOK || unblocked.Status != "unblocked" {
		t.Fatalf("unblock = (%d, %q)", ok.Code, unblocked.Status)
	}
	if len(reads.blocks) != 0 {
		t.Fatalf("unblock did not clear the table: %+v", reads.blocks)
	}
	// Empty session id in the path is a client error, not a port call.
	if bad := serveRead(t, s, http.MethodDelete, "/api/security/blocks/"); bad.Code != http.StatusBadRequest {
		t.Fatalf("empty-id unblock status = %d, want 400", bad.Code)
	}
}

// TestReadAnalyticsYearHeatmapAndAgentFacet pins the /api/analytics
// response's fixed trailing-year heatmap + agent-facet projections: the
// heatmap rides the same Analytics port at day granularity (same filters,
// including agent), cells fold through the unified Totals block with
// per-model pricing, and a heatmap read failure fails the whole request.
func TestReadAnalyticsYearHeatmapAndAgentFacet(t *testing.T) {
	const day = int64(1700000000)
	var yearQuery appapi.AnalyticsQuery
	reads := &readAPIStub{
		analytics: func(q appapi.AnalyticsQuery) ([]observestats.AnalyticsBucket, error) {
			if q.Granularity == "day" {
				yearQuery = q
				return []observestats.AnalyticsBucket{
					{Provider: "p", Model: "priced", Bucket: day, Requests: 2, Input: 10, Output: 5, DurationSum: 4000},
					{Provider: "p", Model: "unknown", Bucket: day, Requests: 1, Input: 7},
				}, nil
			}
			return []observestats.AnalyticsBucket{}, nil
		},
		analyticsAgents: func(appapi.AnalyticsQuery) []string { return []string{"codex", "pi"} },
		pricing:         appapi.PricingSnapshot{Catalog: &pricing.Catalog{ByModel: map[string]pricing.Entry{"priced": {Prompt: .001, Completion: .002}}}},
	}
	s := newReadServer(t, reads)
	before := time.Now()
	resp := serveRead(t, s, http.MethodGet, "/api/analytics?from=100&to=260&agent=codex")
	// The cell JSON is flat (YearCell embeds Totals), so the decode struct
	// embeds its totals block the same way.
	type yearTotals struct {
		Requests uint64   `json:"requests"`
		Tokens   uint64   `json:"tokens"`
		TokSec   *float64 `json:"tok_sec"`
		Cost     *float64 `json:"cost"`
	}
	type yearCell struct {
		Day int64 `json:"day"`
		yearTotals
	}
	var out struct {
		Heatmap struct {
			From  int64      `json:"from"`
			To    int64      `json:"to"`
			Cells []yearCell `json:"cells"`
		} `json:"heatmap"`
		Agents []string `json:"agents"`
	}
	decodeReadJSON(t, resp, &out)
	if resp.Code != http.StatusOK {
		t.Fatalf("code = %d", resp.Code)
	}
	if len(out.Agents) != 2 || out.Agents[0] != "codex" || out.Agents[1] != "pi" {
		t.Fatalf("agents = %v", out.Agents)
	}
	// The heatmap window is the FIXED trailing year (server-local midnight
	// 364 days back through now), independent of the toolbar's from/to.
	if !yearWindowBounds(before).contains(out.Heatmap.From) {
		t.Fatalf("heatmap from = %d, want the 1st of the month 12 months back", out.Heatmap.From)
	}
	if out.Heatmap.To < before.Unix() || out.Heatmap.To > time.Now().Unix() {
		t.Fatalf("heatmap to = %d, want ~now", out.Heatmap.To)
	}
	if yearQuery.Agent != "codex" || yearQuery.By != "model" || yearQuery.Granularity != "day" {
		t.Fatalf("year query = %#v, want agent forwarded at day/model", yearQuery)
	}
	if len(out.Heatmap.Cells) != 1 {
		t.Fatalf("cells = %+v, want the two models folded into one day", out.Heatmap.Cells)
	}
	c := out.Heatmap.Cells[0]
	if c.Day != day || c.Requests != 3 || c.Tokens != 22 ||
		c.TokSec == nil || math.Abs(*c.TokSec-1.25) > 1e-9 ||
		c.Cost == nil || math.Abs(*c.Cost-(10*.001+5*.002)) > 1e-12 {
		t.Fatalf("cell = %+v", c)
	}

	reads.analytics = func(q appapi.AnalyticsQuery) ([]observestats.AnalyticsBucket, error) {
		if q.Granularity == "day" {
			return nil, errors.New("heatmap down")
		}
		return []observestats.AnalyticsBucket{}, nil
	}
	// week granularity keeps the series/compare reads off the failing
	// day-granularity branch (day is also the default granularity).
	failed := serveRead(t, s, http.MethodGet, "/api/analytics?from=1&to=2&granularity=week")
	var routeError struct {
		Error string `json:"error"`
	}
	decodeReadJSON(t, failed, &routeError)
	if failed.Code != http.StatusInternalServerError || routeError.Error != "analytics heatmap: heatmap down" {
		t.Fatalf("heatmap failure = (%d, %#v)", failed.Code, routeError)
	}
}

// TestReadAnalyticsAllTimeClampsToEarliestBucket pins the from=0 all-time
// sentinel: the window (and the echoed from — the client's grid anchor and
// granularity-gating span) starts at the oldest persisted bucket, never at
// the epoch. An empty/disabled store keeps 0 and simply yields no series.
func TestReadAnalyticsAllTimeClampsToEarliestBucket(t *testing.T) {
	const earliest = int64(1788874500)
	var windows []int64
	reads := &readAPIStub{
		statsSince: earliest,
		analytics: func(q appapi.AnalyticsQuery) ([]observestats.AnalyticsBucket, error) {
			windows = append(windows, q.From)
			return []observestats.AnalyticsBucket{}, nil
		},
	}
	s := newReadServer(t, reads)
	resp := serveRead(t, s, http.MethodGet, "/api/analytics?from=0&granularity=week")
	var out struct {
		From int64 `json:"from"`
	}
	decodeReadJSON(t, resp, &out)
	if resp.Code != http.StatusOK || out.From != earliest {
		t.Fatalf("all-time echo = (%d, from %d), want from=%d", resp.Code, out.From, earliest)
	}
	if len(windows) == 0 || windows[0] != earliest {
		t.Fatalf("series query windows = %v, want clamped from=%d", windows, earliest)
	}

	// Empty store: no anchor, the sentinel passes through unchanged.
	reads.statsSince = 0
	windows = nil
	resp = serveRead(t, s, http.MethodGet, "/api/analytics?from=0&granularity=week")
	decodeReadJSON(t, resp, &out)
	if resp.Code != http.StatusOK || out.From != 0 || len(windows) == 0 || windows[0] != 0 {
		t.Fatalf("empty-store all-time = (%d, from %d, queries %v)", resp.Code, out.From, windows)
	}
}

// TestHandleMCPSurface serves the configured servers/routes projection with
// live gauges; TestHandleMCPTest covers the probe command path.
func TestHandleMCPSurface(t *testing.T) {
	s := newReadServer(t, &readAPIStub{mcpSurface: appapi.MCPSurface{
		Servers: []appapi.MCPServerInfo{
			{Name: "zhipu-search", Enabled: true, Transport: "streamable", Auth: "provider", Provider: "zhipu", URL: "https://open.bigmodel.cn/api/mcp/web_search_prime/mcp", Accounts: 2, Sessions: 3},
			{Name: "exa", Enabled: true, Transport: "streamable", Auth: "none", URL: "https://mcp.exa.ai/mcp"},
		},
		Routes: []appapi.MCPRouteInfo{
			{Name: "web-search", Enabled: true, Sessions: 1, Targets: []appapi.MCPRouteTargetInfo{{Server: "zhipu-search", Tools: 1}, {Server: "exa", Tools: 1}}},
		},
	}})
	req := httptest.NewRequest(http.MethodGet, "/api/mcp", nil)
	rec := httptest.NewRecorder()
	s.serveAPI(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
	}
	var out appapi.MCPSurface
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Servers) != 2 || out.Servers[0].Name != "zhipu-search" || out.Servers[0].Sessions != 3 || out.Servers[0].Accounts != 2 {
		t.Fatalf("servers = %+v", out.Servers)
	}
	if len(out.Routes) != 1 || len(out.Routes[0].Targets) != 2 || out.Routes[0].Sessions != 1 {
		t.Fatalf("routes = %+v", out.Routes)
	}
}

func TestHandleMCPTest(t *testing.T) {
	var gotName string
	s := newReadServerWithCommands(t, &readAPIStub{}, &commandFake{
		probeMCP: func(_ context.Context, name string) (appapi.MCPProbeResult, error) {
			gotName = name
			return appapi.MCPProbeResult{OK: true, ServerName: "fake-mcp", Protocol: "2025-03-26", Tools: []string{"search", "read"}, LatencyMs: 42}, nil
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/mcp/test", strings.NewReader(`{"name":"zhipu-search"}`))
	rec := httptest.NewRecorder()
	s.serveAPI(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body)
	}
	if gotName != "zhipu-search" {
		t.Fatalf("probe name = %q", gotName)
	}
	var out appapi.MCPProbeResult
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || len(out.Tools) != 2 || out.LatencyMs != 42 {
		t.Fatalf("probe result = %+v", out)
	}
	// Missing name → 400.
	req = httptest.NewRequest(http.MethodPost, "/api/mcp/test", strings.NewReader(`{}`))
	rec = httptest.NewRecorder()
	s.serveAPI(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty name = %d, want 400", rec.Code)
	}
}
