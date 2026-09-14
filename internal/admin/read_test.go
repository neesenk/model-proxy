package admin

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"model-proxy/internal/accounts"
	"model-proxy/internal/appapi"
	responsecache "model-proxy/internal/cache"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/fusion"
	obscounters "model-proxy/internal/observe/counters"
	"model-proxy/internal/observe/requestlog"
	"model-proxy/internal/observe/seclog"
	observestats "model-proxy/internal/observe/stats"
	"model-proxy/internal/pricing"
	"model-proxy/internal/provider"
	runtimestate "model-proxy/internal/runtime"
	runtimewire "model-proxy/internal/runtime/wirecap"
)

func TestDashboardProjection(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	state := DashboardState{
		Listen:        "127.0.0.1:8080",
		RouteWarnings: []string{"warn-one"},
		Cache:         responsecache.New(responsecache.Options{}),
		Runtime: runtimestate.DashboardSnapshot{
			Providers: map[string]runtimestate.ProviderStatus{
				"up": {CircuitState: "closed", Available: true},
				"down": {
					CircuitState:     "open",
					Available:        false,
					CircuitOpenUntil: now.Add(time.Hour),
					RateLimitedUntil: now.Add(30 * time.Minute),
					RateLimitKind:    runtimestate.Quota,
				},
				"iced": {CircuitState: "closed", Available: false, Frozen: true},
			},
			ModelLocks: map[string][]runtimestate.ModelLockStatus{
				"down": {{Provider: "down", Model: "m", LockedUntil: now.Add(2 * time.Hour)}},
			},
			Quotas: map[string]*provider.QuotaSnapshot{
				"up": {Billing: provider.BillingPlan},
			},
		},
		QuotaEnabled: true,
		StartedAt:    now.Add(-90 * time.Second),
		Counters: map[string]obscounters.ProviderMetricsSnapshot{
			"up": {Requests: 7, Failovers: 1, RateLimited429: 2, Failures: 3, LastRequestAt: 99, LatencySum: 11, TTFTSum: 5},
		},
		Schedule: []byte(`{"models":{}}`),
	}
	service := New(Ports{
		DashboardState: func(got time.Time) DashboardState {
			if !got.Equal(now) {
				t.Errorf("DashboardState now = %v, want %v", got, now)
			}
			return state
		},
	})

	dashboard := service.Dashboard(now)

	if dashboard.Listen != "127.0.0.1:8080" {
		t.Errorf("listen = %q", dashboard.Listen)
	}
	if dashboard.Uptime == "" || !strings.HasSuffix(dashboard.Uptime, "s") {
		t.Errorf("uptime = %q, want a duration string", dashboard.Uptime)
	}
	if strings.Contains(dashboard.Uptime, ".") {
		t.Errorf("uptime = %q, want whole-second granularity (no fractional part)", dashboard.Uptime)
	}
	up, ok := dashboard.Health["up"].(map[string]any)
	if !ok || up["circuit_state"] != "closed" || up["available"] != true {
		t.Errorf("health[up] = %v", dashboard.Health["up"])
	}
	if _, present := up["circuit_until"]; present {
		t.Errorf("healthy provider must not carry circuit_until: %v", up)
	}
	if _, present := up["frozen"]; present {
		t.Errorf("non-frozen provider must not carry frozen: %v", up)
	}
	iced, ok := dashboard.Health["iced"].(map[string]any)
	if !ok || iced["frozen"] != true || iced["available"] != false {
		t.Errorf("health[iced] = %v, want frozen:true + available:false", dashboard.Health["iced"])
	}
	down, ok := dashboard.Health["down"].(map[string]any)
	if !ok || down["available"] != false {
		t.Fatalf("health[down] = %v", dashboard.Health["down"])
	}
	if down["circuit_until"] != now.Add(time.Hour).UTC().Format(time.RFC3339) {
		t.Errorf("circuit_until = %v", down["circuit_until"])
	}
	if down["rate_limited_until"] != now.Add(30*time.Minute).UTC().Format(time.RFC3339) ||
		down["rate_limit_kind"] != "quota" {
		t.Errorf("rate-limit projection = %v", down)
	}
	locks := dashboard.ModelLocks["down"]
	if len(locks) != 1 || locks[0]["model"] != "m" ||
		locks[0]["until"] != now.Add(2*time.Hour).UTC().Format(time.RFC3339) {
		t.Errorf("model locks = %v", locks)
	}
	if len(dashboard.Quota) != 1 {
		t.Errorf("quota = %v, want one entry", dashboard.Quota)
	}
	if dashboard.Cache["enabled"] != true {
		t.Errorf("cache info = %v, want enabled", dashboard.Cache)
	}
	counter := dashboard.Counters["up"]
	if counter.Requests != 7 || counter.Failovers != 1 || counter.RateLimited429 != 2 ||
		counter.Failures != 3 || counter.LastRequestAt != 99 || counter.LatencySum != 11 || counter.TTFTSum != 5 {
		t.Errorf("counters[up] = %+v", counter)
	}
	if string(dashboard.Schedule) != `{"models":{}}` {
		t.Errorf("schedule = %s", dashboard.Schedule)
	}
	if len(dashboard.Warnings) != 1 || dashboard.Warnings[0] != "warn-one" {
		t.Errorf("warnings = %v", dashboard.Warnings)
	}
	if dashboard.CredentialStore == "" {
		t.Error("credential store mode empty")
	}

	// The DTO must be detached from the captured state.
	dashboard.Warnings[0] = "changed"
	dashboard.Schedule[0] = '['
	if state.RouteWarnings[0] != "warn-one" || string(state.Schedule) != `{"models":{}}` {
		t.Error("dashboard DTO aliases the captured state")
	}
}

func TestDashboardWithoutQuotaAndCache(t *testing.T) {
	service := New(Ports{
		DashboardState: func(time.Time) DashboardState {
			return DashboardState{
				Runtime: runtimestate.DashboardSnapshot{
					Quotas: map[string]*provider.QuotaSnapshot{
						"up": {Billing: provider.BillingPlan},
					},
				},
				QuotaEnabled: false,
				StartedAt:    time.Now(),
			}
		},
	})
	dashboard := service.Dashboard(time.Now())
	if dashboard.Quota != nil {
		t.Errorf("quota = %v, want nil when the tracker is disabled", dashboard.Quota)
	}
	if dashboard.Cache["enabled"] != false {
		t.Errorf("cache = %v, want disabled for a nil store", dashboard.Cache)
	}
	if len(dashboard.Counters) != 0 {
		t.Errorf("counters = %v, want empty", dashboard.Counters)
	}
}

func TestLogFileAndRequestLogDirectory(t *testing.T) {
	service := New(Ports{
		LogFile:             func() string { return "/var/log/proxy.log" },
		RequestLogDirectory: func() string { return "" },
	})
	if got := service.LogFile(); got != "/var/log/proxy.log" {
		t.Errorf("LogFile = %q", got)
	}
	if got := service.RequestLogDirectory(); got != "" {
		t.Errorf("RequestLogDirectory = %q, want empty when disabled", got)
	}
	// Request log disabled (or the port unwired) → nil query port, so the web
	// handlers answer their {enabled:false} shape.
	if got := service.RequestLogQueries(); got != nil {
		t.Errorf("RequestLogQueries = %v, want nil when disabled", got)
	}
	if got := New(Ports{}).RequestLogQueries(); got != nil {
		t.Errorf("RequestLogQueries with nil directory port = %v, want nil", got)
	}
}

// TestRequestLogQueriesFallback: with the request log enabled but no index
// (open failure degrades instead of failing startup), the query port answers
// through the directory scan with identical semantics.
func TestRequestLogQueriesFallback(t *testing.T) {
	dir := t.TempDir()
	line := `{"ts":"2026-07-29T12:00:00Z","request_id":"r1","session_id":"s1","called_model":"m","provider":"p","status":200,"request_body":"body","response_body":"{\"usage\":{\"input_tokens\":3,\"output_tokens\":4}}"}`
	if err := os.WriteFile(filepath.Join(dir, "requests-20260729.log"), []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	service := New(Ports{RequestLogDirectory: func() string { return dir }})
	queries := service.RequestLogQueries()
	if queries == nil {
		t.Fatal("RequestLogQueries = nil with the request log enabled")
	}
	summaries, facets, err := queries.SummariesWithFacets(requestlog.Filter{Limit: 10, UsageOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].RequestID != "r1" || summaries[0].Input != 3 || summaries[0].Output != 4 {
		t.Fatalf("fallback summaries = %+v", summaries)
	}
	if len(facets.Providers) != 1 || facets.Providers[0] != "p" {
		t.Fatalf("fallback facets = %+v", facets)
	}
	records, err := queries.Detail("r1")
	if err != nil || len(records) != 1 || records[0].RequestBody != "body" {
		t.Fatalf("fallback detail = (%+v, %v)", records, err)
	}
	sessions, err := queries.SessionSummaries(2000, 50, nil)
	if err != nil || len(sessions) != 1 || sessions[0].SessionID != "s1" || sessions[0].Usage.Input != 3 {
		t.Fatalf("fallback sessions = (%+v, %v)", sessions, err)
	}
}

// TestRequestLogQueriesIndexDelegation: with a running index wired, the query
// port delegates to it (the reconciled index answers, not the raw directory).
func TestRequestLogQueriesIndexDelegation(t *testing.T) {
	dir := t.TempDir()
	line := `{"ts":"2026-07-29T12:00:00Z","request_id":"r1","called_model":"m","provider":"p","status":200}`
	if err := os.WriteFile(filepath.Join(dir, "requests-20260729.log"), []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	indexer, err := requestlog.NewIndexer(dir)
	if err != nil {
		t.Fatal(err)
	}
	go indexer.Run()
	t.Cleanup(indexer.Shutdown)
	service := New(Ports{
		RequestLogDirectory: func() string { return dir },
		RequestLogIndex:     func() *requestlog.Indexer { return indexer },
	})
	queries := service.RequestLogQueries()
	if queries == nil {
		t.Fatal("RequestLogQueries = nil with index wired")
	}
	// The index reconciles on its own tick; poll until the record appears.
	deadline := time.Now().Add(5 * time.Second)
	for {
		summaries, _, err := queries.SummariesWithFacets(requestlog.Filter{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(summaries) == 1 && summaries[0].RequestID == "r1" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("index-delegated query never saw the record")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAccountsProjection(t *testing.T) {
	home := setTestHome(t)
	_ = home
	if err := provider.SaveAqpAccount(accounts.AuthFilePath("aqp", "oauth_auth"), &provider.AqpAccountData{
		AccountID: "aqp-id",
		Email:     "u@x.com",
		CreatedAt: 1700000000,
	}); err != nil {
		t.Fatal(err)
	}
	codexAuth := &provider.CodexAuthFile{}
	codexAuth.Tokens.AccountID = "codex-id"
	if err := provider.WriteCodexAuthFile(accounts.AuthFilePath("codex", "oauth_auth"), codexAuth); err != nil {
		t.Fatal(err)
	}
	apiKeyID := accounts.AccountID("zhipu", accounts.Credentials{APIKey: "k"})
	if err := accounts.NewStore(accounts.HomeDir()).Save("zhipu", "zhipu", accounts.Pool{Accounts: []accounts.Account{
		{ID: apiKeyID, Label: "first", APIKey: "k", AddedAt: "2026-01-01"},
	}}); err != nil {
		t.Fatal(err)
	}

	service := New(Ports{
		ProviderConfigs: func() map[string]configdomain.Provider {
			return map[string]configdomain.Provider{
				"aqp":   {Provider: "aqp"},
				"codex": {Provider: "codex"},
				"zhipu": {Provider: "zhipu", Billing: "plan"},
				"empty": {Provider: "deepseek"},
				// pay-as-you-go WITH a usage endpoint: polled like a plan
				// provider, so the UI must expose Refresh usage for it.
				"deepseek": {Provider: "deepseek", Billing: "pay-as-you-go", UsageURL: "http://x/user/balance"},
			}
		},
	})
	out := service.Accounts()
	byName := map[string]appapi.ProviderAccounts{}
	for _, item := range out {
		byName[item.Name] = item
	}
	if len(byName) != 5 {
		t.Fatalf("accounts = %v", out)
	}
	aqp := byName["aqp"]
	if len(aqp.Accounts) != 1 || aqp.Accounts[0].ID != "aqp-id" ||
		aqp.Accounts[0].Email != "u@x.com" || aqp.Accounts[0].AddedAt == "" {
		t.Errorf("aqp accounts = %+v", aqp.Accounts)
	}
	codex := byName["codex"]
	if len(codex.Accounts) != 1 || codex.Accounts[0].ID != "codex-id" {
		t.Errorf("codex accounts = %+v", codex.Accounts)
	}
	zhipu := byName["zhipu"]
	if zhipu.ProviderID != "zhipu" || zhipu.Billing != "plan" ||
		len(zhipu.Accounts) != 1 || zhipu.Accounts[0].ID != apiKeyID ||
		zhipu.Accounts[0].Label != "first" || zhipu.Accounts[0].AddedAt != "2026-01-01" {
		t.Errorf("zhipu accounts = %+v", zhipu)
	}
	empty := byName["empty"]
	if empty.Accounts == nil || len(empty.Accounts) != 0 {
		t.Errorf("provider without accounts must project an empty (non-nil) list: %+v", empty.Accounts)
	}
	if empty.UsageEndpoint {
		t.Errorf("provider without usage_url must project usage_endpoint=false: %+v", empty)
	}
	if ds := byName["deepseek"]; !ds.UsageEndpoint {
		t.Errorf("pay-as-you-go provider with usage_url must project usage_endpoint=true: %+v", ds)
	}
}

func TestTokensProjection(t *testing.T) {
	service := New(Ports{
		TokenUsage: func() map[obscounters.TokenKey]obscounters.TokenUsage {
			return map[obscounters.TokenKey]obscounters.TokenUsage{
				{Provider: "up", Model: "m"}: {Input: 1, Output: 2, CacheCreation: 3, CacheRead: 4, Requests: 5},
			}
		},
	})
	out, err := service.Tokens(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("tokens = %v", out)
	}
	got := out[0]
	if got.Provider != "up" || got.Model != "m" || got.Input != 1 || got.Output != 2 ||
		got.CacheCreation != 3 || got.CacheRead != 4 || got.Total != 10 || got.Requests != 5 {
		t.Errorf("token usage = %+v", got)
	}
}

func TestTokensNilSnapshot(t *testing.T) {
	service := New(Ports{
		TokenUsage: func() map[obscounters.TokenKey]obscounters.TokenUsage { return nil },
	})
	if out, _ := service.Tokens(0, 0); len(out) != 0 {
		t.Errorf("tokens = %v, want empty", out)
	}
}

// TestTokensWindowedProjection: from > 0 reads the persisted-bucket port, not
// the hot counters, and maps token_requests into Requests; store errors
// propagate.
func TestTokensWindowedProjection(t *testing.T) {
	wantErr := errors.New("store closed")
	service := New(Ports{
		TokenUsage: func() map[obscounters.TokenKey]obscounters.TokenUsage {
			t.Error("windowed Tokens must not read the hot counters")
			return nil
		},
		TokenUsageRange: func(from, to int64) (map[observestats.Key]observestats.Counters, error) {
			if from <= 0 {
				t.Errorf("TokenUsageRange from = %d, want > 0", from)
			}
			return map[observestats.Key]observestats.Counters{
				{Provider: "up", Model: "m"}: {Input: 10, Output: 4, CacheCreation: 2, CacheRead: 8, TokenRequests: 3},
			}, nil
		},
	})
	out, err := service.Tokens(3600, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := appapi.TokenUsage{Provider: "up", Model: "m", Input: 10, Output: 4, CacheCreation: 2, CacheRead: 8, Total: 24, Requests: 3}
	if len(out) != 1 || out[0] != want {
		t.Errorf("windowed tokens = %+v, want [%+v]", out, want)
	}
	service.ports.TokenUsageRange = func(int64, int64) (map[observestats.Key]observestats.Counters, error) {
		return nil, wantErr
	}
	if _, err := service.Tokens(3600, 0); !errors.Is(err, wantErr) {
		t.Errorf("Tokens error = %v, want %v", err, wantErr)
	}
}

func TestAgentsProjection(t *testing.T) {
	service := New(Ports{
		AgentUsage: func() map[obscounters.AgentKey]obscounters.AgentCount {
			return map[obscounters.AgentKey]obscounters.AgentCount{
				{Agent: "codex", Provider: "z", Model: "glm"}:     {Requests: 2, Input: 10, Output: 4, CacheCreation: 1, CacheRead: 5},
				{Agent: "codex", Provider: "a", Model: "glm-5.2"}: {Requests: 1, Input: 40, Output: 8},
				{Agent: "pi", Provider: "z", Model: "glm"}:        {Requests: 1, Input: 3, Output: 4},
				{Agent: "pi", Provider: "a", Model: "m2"}:         {Requests: 7},
			}
		},
	})
	got, err := service.Agents(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	// codex totals 10+4+1+5 + 40+8 = 68 across two models; pi totals 7.
	want := []appapi.AgentUsage{
		{
			Agent: "codex", Requests: 3, Input: 50, Output: 12, CacheCreation: 1, CacheRead: 5, Total: 68,
			Models: []appapi.AgentModelUsage{
				{Provider: "a", Model: "glm-5.2", Requests: 1, Input: 40, Output: 8, Total: 48},
				{Provider: "z", Model: "glm", Requests: 2, Input: 10, Output: 4, CacheCreation: 1, CacheRead: 5, Total: 20},
			},
		},
		{
			Agent: "pi", Requests: 8, Input: 3, Output: 4, Total: 7,
			Models: []appapi.AgentModelUsage{
				{Provider: "z", Model: "glm", Requests: 1, Input: 3, Output: 4, Total: 7},
				{Provider: "a", Model: "m2", Requests: 7},
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("agents = %+v, want %+v", got, want)
	}
}

// TestAgentsWindowedProjection: from > 0 reads the persisted-bucket port and
// produces the same nested shape as the cumulative view; store errors
// propagate.
func TestAgentsWindowedProjection(t *testing.T) {
	wantErr := errors.New("store closed")
	service := New(Ports{
		AgentUsage: func() map[obscounters.AgentKey]obscounters.AgentCount {
			t.Error("windowed Agents must not read the hot counters")
			return nil
		},
		AgentUsageRange: func(from, to int64) (map[observestats.AgentKey]observestats.AgentCounters, error) {
			if from <= 0 {
				t.Errorf("AgentUsageRange from = %d, want > 0", from)
			}
			return map[observestats.AgentKey]observestats.AgentCounters{
				{Agent: "codex", Provider: "z", Model: "glm"}: {Requests: 2, Input: 10, Output: 4, CacheRead: 6},
				{Agent: "codex", Provider: "a", Model: "m2"}:  {Requests: 1, Input: 30, Output: 6},
			}, nil
		},
	})
	got, err := service.Agents(3600, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []appapi.AgentUsage{
		{
			Agent: "codex", Requests: 3, Input: 40, Output: 10, CacheRead: 6, Total: 56,
			Models: []appapi.AgentModelUsage{
				{Provider: "a", Model: "m2", Requests: 1, Input: 30, Output: 6, Total: 36},
				{Provider: "z", Model: "glm", Requests: 2, Input: 10, Output: 4, CacheRead: 6, Total: 20},
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("windowed agents = %+v, want %+v", got, want)
	}
	service.ports.AgentUsageRange = func(int64, int64) (map[observestats.AgentKey]observestats.AgentCounters, error) {
		return nil, wantErr
	}
	if _, err := service.Agents(3600, 0); !errors.Is(err, wantErr) {
		t.Errorf("Agents error = %v, want %v", err, wantErr)
	}
}

func TestAgentsNilSnapshot(t *testing.T) {
	service := New(Ports{
		AgentUsage: func() map[obscounters.AgentKey]obscounters.AgentCount { return nil },
	})
	if out, _ := service.Agents(0, 0); len(out) != 0 {
		t.Errorf("agents = %v, want empty", out)
	}
}

func TestStatsQueriesPassThrough(t *testing.T) {
	wantErr := errors.New("store closed")
	service := New(Ports{
		StatsRange: func(from, to int64, provider, model string, bucketSecs int64) ([]observestats.Bucket, error) {
			if from != 1 || to != 2 || provider != "p" || model != "m" || bucketSecs != 60 {
				t.Errorf("StatsRange args = %d %d %q %q %d", from, to, provider, model, bucketSecs)
			}
			return []observestats.Bucket{{Minute: 1}}, nil
		},
		AgentStats: func(from, to int64, agent, provider, model string, bucketSecs int64) ([]observestats.AgentBucket, error) {
			if agent != "cli" {
				t.Errorf("AgentStats agent = %q", agent)
			}
			return nil, wantErr
		},
		Analytics: func(from, to int64, provider, model, granularity string) ([]observestats.AnalyticsBucket, error) {
			if granularity != "day" {
				t.Errorf("Analytics granularity = %q", granularity)
			}
			return []observestats.AnalyticsBucket{{Bucket: 1}}, nil
		},
	})
	buckets, err := service.Stats(appapi.StatsQuery{From: 1, To: 2, Provider: "p", Model: "m", BucketSecs: 60})
	if err != nil || len(buckets) != 1 {
		t.Errorf("Stats = %v, %v", buckets, err)
	}
	if _, err := service.AgentStats(appapi.AgentStatsQuery{Agent: "cli"}); !errors.Is(err, wantErr) {
		t.Errorf("AgentStats err = %v, want %v", err, wantErr)
	}
	analytics, err := service.Analytics(appapi.AnalyticsQuery{Granularity: "day"})
	if err != nil || len(analytics) != 1 {
		t.Errorf("Analytics = %v, %v", analytics, err)
	}
}

func TestPricingPassThrough(t *testing.T) {
	catalog := pricing.Empty()
	overrides := map[string]pricing.Override{"m": {Input: 9}}
	service := New(Ports{
		Pricing: func() (*pricing.Catalog, map[string]pricing.Override) {
			return catalog, overrides
		},
	})
	got := service.Pricing()
	if got.Catalog != catalog || got.Overrides["m"].Input != 9 {
		t.Errorf("pricing = %+v", got)
	}
}

func TestFusionPassThrough(t *testing.T) {
	stats := map[string]fusion.WorkflowStats{"wf": {Runs: 2}}
	runs := []fusion.Run{{Workflow: "wf"}}
	now := time.Now()
	service := New(Ports{
		FusionSnapshot: func(workflow string, got time.Time) (map[string]fusion.WorkflowStats, []fusion.Run) {
			if workflow != "wf" || !got.Equal(now) {
				t.Errorf("FusionSnapshot args = %q %v", workflow, got)
			}
			return stats, runs
		},
	})
	gotStats, gotRuns := service.Fusion("wf", now)
	if gotStats["wf"].Runs != 2 || len(gotRuns) != 1 {
		t.Errorf("fusion = %v %v", gotStats, gotRuns)
	}
}

func TestPinsProjection(t *testing.T) {
	expires := time.Date(2026, 8, 1, 13, 0, 0, 0, time.UTC)
	service := New(Ports{
		Pins: func() map[string]PinState {
			return map[string]PinState{"m": {Provider: "up", ExpiresAt: expires}}
		},
	})
	out := service.Pins()
	if len(out) != 1 || out[0].Route != "m" || out[0].Provider != "up" || !out[0].ExpiresAt.Equal(expires) {
		t.Errorf("pins = %v", out)
	}
}

func TestModelsDocumentProjection(t *testing.T) {
	probed := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	service := New(Ports{
		ModelCapsSnapshot: func() map[string]runtimewire.ProviderModelCaps {
			return map[string]runtimewire.ProviderModelCaps{
				"up": {
					Fingerprint: "0123456789abcdef",
					ProbedAt:    probed,
					Models: map[string]runtimewire.ModelProtocols{
						"m1": {Chat: runtimewire.Yes, Anthropic: runtimewire.No, Responses: runtimewire.Unknown},
					},
				},
				// Provider probed but with no recorded models keeps its
				// fingerprint/probed_at and projects an empty, non-nil map.
				"empty": {Fingerprint: "fedcba9876543210", ProbedAt: probed},
			}
		},
	})

	document := service.ModelsDocument()

	up, ok := document.Providers["up"]
	if !ok || up.Fingerprint != "0123456789abcdef" || !up.ProbedAt.Equal(probed) {
		t.Fatalf("providers[up] = %+v", document.Providers["up"])
	}
	m1 := up.Models["m1"]
	if m1.Chat != "yes" || m1.Anthropic != "no" || m1.Responses != "unknown" {
		t.Errorf("verdict strings = %+v, want yes/no/unknown", m1)
	}
	empty, ok := document.Providers["empty"]
	if !ok || empty.Models == nil || len(empty.Models) != 0 {
		t.Errorf("providers[empty] = %+v, want present with an empty non-nil models map", empty)
	}

	// JSON shape: the wire contract keys, never Go field names.
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
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
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("document JSON = %s: %v", data, err)
	}
	wire := decoded.Providers["up"]
	if wire.Fingerprint != "0123456789abcdef" || wire.ProbedAt != "2026-09-01T10:00:00Z" ||
		wire.Models["m1"].Chat != "yes" || wire.Models["m1"].Anthropic != "no" || wire.Models["m1"].Responses != "unknown" {
		t.Errorf("wire projection = %+v (%s)", wire, data)
	}
}

func TestModelsDocumentEmptyStore(t *testing.T) {
	// Empty snapshot → {"providers":{}} (non-nil, never null).
	service := New(Ports{
		ModelCapsSnapshot: func() map[string]runtimewire.ProviderModelCaps {
			return map[string]runtimewire.ProviderModelCaps{}
		},
	})
	document := service.ModelsDocument()
	if document.Providers == nil || len(document.Providers) != 0 {
		t.Errorf("empty store document = %+v", document)
	}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"providers":{}}` {
		t.Errorf("empty store JSON = %s, want {\"providers\":{}}", data)
	}

	// A missing port degrades to the same empty document.
	document = New(Ports{}).ModelsDocument()
	if document.Providers == nil || len(document.Providers) != 0 {
		t.Errorf("nil port document = %+v", document)
	}
}

func TestSecurityAuditDisabled(t *testing.T) {
	service := New(Ports{
		Config: func() *configdomain.Config {
			return &configdomain.Config{Guard: configdomain.GuardConfig{Audit: false}}
		},
	})
	result, err := service.Security(appapi.SecurityQuery{Kind: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Enabled || result.Records == nil || len(result.Records) != 0 {
		t.Errorf("disabled result = %+v", result)
	}
}

func TestSecurityQueryProjectsRecords(t *testing.T) {
	home := setTestHome(t)
	auditPath := filepath.Join(home, "logs", "security.log")
	if err := os.MkdirAll(filepath.Dir(auditPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := seclog.AppendSync(filepath.Dir(auditPath), &seclog.Record{
		Ts:        1000,
		Kind:      "secret",
		RequestID: "req-1",
		Agent:     "cli",
		Protocol:  "anthropic",
		Exposed:   "m",
		Names:     []string{"api-key"},
		Action:    "blocked",
		Detail:    "outbound",
	}); err != nil {
		t.Fatal(err)
	}

	service := New(Ports{
		Config: func() *configdomain.Config {
			return &configdomain.Config{Guard: configdomain.GuardConfig{Audit: true, AuditPath: auditPath}}
		},
	})
	result, err := service.Security(appapi.SecurityQuery{Kind: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Enabled || len(result.Records) != 1 {
		t.Fatalf("result = %+v", result)
	}
	record := result.Records[0]
	if record.Ts != 1000 || record.Kind != "secret" || record.RequestID != "req-1" ||
		record.Agent != "cli" || record.Protocol != "anthropic" || record.Exposed != "m" ||
		len(record.Names) != 1 || record.Names[0] != "api-key" ||
		record.Action != "blocked" || record.Detail != "outbound" {
		t.Errorf("record = %+v", record)
	}
}

func TestSecurityMissingDirectoryDegradesToDisabled(t *testing.T) {
	home := setTestHome(t)
	service := New(Ports{
		Config: func() *configdomain.Config {
			return &configdomain.Config{Guard: configdomain.GuardConfig{
				Audit:     true,
				AuditPath: filepath.Join(home, "missing", "security.log"),
			}}
		},
	})
	result, err := service.Security(appapi.SecurityQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Enabled || len(result.Records) != 0 {
		t.Errorf("missing directory result = %+v, want empty disabled", result)
	}
}

func TestConfigDocument(t *testing.T) {
	path := writeTestConfig(t)
	service := New(Ports{ConfigFile: func() string { return path }})

	document, err := service.ConfigDocument()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(document.YAML, "zhipu") {
		t.Errorf("yaml = %q", document.YAML)
	}
	if document.Summary.Listen != "127.0.0.1:8080" || document.Summary.ProviderCount != 1 ||
		document.Summary.RouteCount != 1 {
		t.Errorf("summary = %+v", document.Summary)
	}
	if len(document.ProviderModels["zhipu"]) != 1 || document.ProviderModels["zhipu"][0] != "glm" {
		t.Errorf("provider models = %v", document.ProviderModels)
	}
	targets := document.Routes["glm"]
	if len(targets) != 1 || targets[0].Provider != "zhipu" || targets[0].Model != "glm" || targets[0].Priority != 1 {
		t.Errorf("routes = %v", document.Routes)
	}
	// Settings project the effective log level (loader default) and keep unset
	// scheduling scalars as null pointers so the form can show the code default.
	if document.Settings.LogLevel != "info" {
		t.Errorf("settings log_level = %q, want effective default info", document.Settings.LogLevel)
	}
	if document.Settings.Scheduling.CircuitThreshold != nil {
		t.Errorf("unset circuit_threshold = %v, want nil", *document.Settings.Scheduling.CircuitThreshold)
	}
	if document.Settings.RequestLog.Enabled || document.Settings.Cache.Enabled {
		t.Errorf("settings enabled flags = %+v, want false", document.Settings)
	}
}

// TestConfigDocumentSettings pins the raw scalar projection for the blocks the
// Config tab's settings form edits (empty = key absent, so the form shows the
// code default as a placeholder).
func TestConfigDocumentSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	config := `listen: 127.0.0.1:8080
log_level: debug
log_file: /tmp/mp.log
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://example.test, models: [glm]}
scheduling:
  circuit_threshold: 5
  circuit_cooldown: 2m
  quality_error_weight: 0
request_log:
  enabled: true
  retention: 720h
stats:
  retention: 0
cache:
  enabled: true
  max_entries: 6000
`
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	service := New(Ports{ConfigFile: func() string { return path }})
	document, err := service.ConfigDocument()
	if err != nil {
		t.Fatal(err)
	}
	settings := document.Settings
	if settings.LogLevel != "debug" || settings.LogFile != "/tmp/mp.log" {
		t.Errorf("general settings = %+v", settings)
	}
	if settings.Scheduling.CircuitThreshold == nil || *settings.Scheduling.CircuitThreshold != 5 {
		t.Errorf("circuit_threshold = %v", settings.Scheduling.CircuitThreshold)
	}
	if settings.Scheduling.CircuitCooldown != "2m" {
		t.Errorf("circuit_cooldown = %q", settings.Scheduling.CircuitCooldown)
	}
	// An explicit 0 must survive as a non-nil pointer (it disables the signal).
	if settings.Scheduling.QualityErrorWeight == nil || *settings.Scheduling.QualityErrorWeight != 0 {
		t.Errorf("quality_error_weight = %v, want explicit 0", settings.Scheduling.QualityErrorWeight)
	}
	if !settings.RequestLog.Enabled || settings.RequestLog.Retention != "720h" {
		t.Errorf("request_log = %+v", settings.RequestLog)
	}
	if settings.Stats.Retention != "0" {
		t.Errorf("stats retention = %q, want \"0\"", settings.Stats.Retention)
	}
	if !settings.Cache.Enabled || settings.Cache.MaxEntries != 6000 {
		t.Errorf("cache = %+v", settings.Cache)
	}
}

func TestConfigDocumentErrors(t *testing.T) {
	service := New(Ports{ConfigFile: func() string { return filepath.Join(t.TempDir(), "missing.yaml") }})
	if _, err := service.ConfigDocument(); err == nil {
		t.Error("missing file must error")
	}

	invalid := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(invalid, []byte("providers: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	service = New(Ports{ConfigFile: func() string { return invalid }})
	if _, err := service.ConfigDocument(); err == nil {
		t.Error("invalid config must error")
	}
}

func TestCurrentConfigFileNilSafety(t *testing.T) {
	var service *Service
	if got := service.currentConfigFile(); got != "" {
		t.Errorf("nil service config file = %q", got)
	}
	if got := New(Ports{}).currentConfigFile(); got != "" {
		t.Errorf("missing ConfigFile port = %q", got)
	}
}

func TestPresetsListsSharedCatalog(t *testing.T) {
	service := New(Ports{})
	catalog := service.Presets()
	if len(catalog) == 0 {
		t.Fatal("preset catalog empty")
	}
	known := false
	for _, preset := range catalog {
		if preset.Name == "zhipu" {
			known = true
		}
	}
	if !known {
		t.Error("preset catalog missing zhipu")
	}
}

// TestTokensExcludesVirtualProviders: guard/attempts/routing/fusion share the
// (provider, model) key space with upstream usage but are request counters, not
// billable models — they must not appear in /api/tokens (cumulative or windowed).
func TestTokensExcludesVirtualProviders(t *testing.T) {
	service := New(Ports{
		TokenUsage: func() map[obscounters.TokenKey]obscounters.TokenUsage {
			return map[obscounters.TokenKey]obscounters.TokenUsage{
				{Provider: "up", Model: "m"}:             {Input: 1, Requests: 1},
				{Provider: "guard", Model: "ssh"}:        {Requests: 9},
				{Provider: "attempts", Model: "ok"}:      {Requests: 9},
				{Provider: "routing", Model: "decision"}: {Requests: 9},
				{Provider: "fusion", Model: "wf"}:        {Requests: 9},
			}
		},
		TokenUsageRange: func(from, to int64) (map[observestats.Key]observestats.Counters, error) {
			return map[observestats.Key]observestats.Counters{
				{Provider: "up", Model: "m"}:      {Input: 1, TokenRequests: 1},
				{Provider: "guard", Model: "ssh"}: {Requests: 9},
			}, nil
		},
	})
	for _, tc := range []struct {
		name     string
		from, to int64
	}{
		{"cumulative", 0, 0},
		{"windowed", 3600, 0},
	} {
		out, err := service.Tokens(tc.from, tc.to)
		if err != nil {
			t.Fatal(err)
		}
		if len(out) != 1 || out[0].Provider != "up" {
			t.Errorf("%s tokens = %+v, want only up/m", tc.name, out)
		}
	}
}

// TestAnalyticsExcludesVirtualProviders: the calendar-bucket projection drops
// virtual counter namespaces so they cannot surface as unpriced "models".
func TestAnalyticsExcludesVirtualProviders(t *testing.T) {
	service := New(Ports{
		Analytics: func(from, to int64, provider, model, granularity string) ([]observestats.AnalyticsBucket, error) {
			return []observestats.AnalyticsBucket{
				{Provider: "up", Model: "m", Bucket: 1},
				{Provider: "guard", Model: "ssh", Bucket: 1},
				{Provider: "routing", Model: "decision", Bucket: 1},
			}, nil
		},
	})
	out, err := service.Analytics(appapi.AnalyticsQuery{Granularity: "day"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Provider != "up" {
		t.Errorf("analytics = %+v, want only up/m", out)
	}
}

// writeExplainRequestLog persists one request-log JSONL record for
// SecurityExplain tests (the scan fallback path — index-backed equivalence
// lives in internal/observe/requestlog).
func writeExplainRequestLog(t *testing.T, dir string, record requestlog.Record) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "requests-2026-09-11.log"), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSecurityExplainValidation(t *testing.T) {
	service := New(Ports{LocateGuardHits: func([]byte, string, []string) ([]appapi.SecurityMatch, error) {
		return nil, nil
	}})
	if _, err := service.SecurityExplain("req-1", "drift", []string{"x"}); err == nil {
		t.Error("kind drift: want error")
	}
	if _, err := service.SecurityExplain("", "secret", []string{"x"}); err == nil {
		t.Error("empty request id: want error")
	}
	if _, err := service.SecurityExplain("req-1", "secret", nil); err == nil {
		t.Error("empty names: want error")
	}
}

func TestSecurityExplainUnwiredPortFailsClosed(t *testing.T) {
	if _, err := New(Ports{}).SecurityExplain("req-1", "secret", []string{"openai_api_key"}); err == nil {
		t.Error("nil LocateGuardHits port: want error, not a silently empty analysis")
	}
}

func TestSecurityExplainNoRequestLog(t *testing.T) {
	service := New(Ports{LocateGuardHits: func([]byte, string, []string) ([]appapi.SecurityMatch, error) {
		return nil, nil
	}})
	result, err := service.SecurityExplain("req-1", "secret", []string{"openai_api_key"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != appapi.SecurityExplainNoRequestLog {
		t.Errorf("status = %q, want no_request_log", result.Status)
	}
}

func TestSecurityExplainNotFound(t *testing.T) {
	dir := t.TempDir()
	service := New(Ports{
		RequestLogDirectory: func() string { return dir },
		LocateGuardHits: func([]byte, string, []string) ([]appapi.SecurityMatch, error) {
			return nil, nil
		},
	})
	result, err := service.SecurityExplain("req-missing", "secret", []string{"openai_api_key"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != appapi.SecurityExplainNotFound {
		t.Errorf("status = %q, want not_found", result.Status)
	}
}

func TestSecurityExplainLocatedMatch(t *testing.T) {
	dir := t.TempDir()
	writeExplainRequestLog(t, dir, requestlog.Record{
		RequestID:   "req-1",
		RequestBody: `{"key":"sk-fixture"}`,
	})
	var gotBody, gotKind string
	var gotNames []string
	service := New(Ports{
		RequestLogDirectory: func() string { return dir },
		LocateGuardHits: func(body []byte, kind string, names []string) ([]appapi.SecurityMatch, error) {
			gotBody, gotKind, gotNames = string(body), kind, names
			return []appapi.SecurityMatch{{Name: "openai_api_key", Located: true, Hit: "sk-…"}}, nil
		},
	})
	result, err := service.SecurityExplain("req-1", "secret", []string{"openai_api_key"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != appapi.SecurityExplainOK || len(result.Matches) != 1 || !result.Matches[0].Located {
		t.Errorf("result = %+v, want ok with 1 located match", result)
	}
	if gotBody != `{"key":"sk-fixture"}` || gotKind != "secret" || len(gotNames) != 1 || gotNames[0] != "openai_api_key" {
		t.Errorf("port args = body %q kind %q names %v", gotBody, gotKind, gotNames)
	}
}

func TestSecurityExplainRedactedBody(t *testing.T) {
	dir := t.TempDir()
	writeExplainRequestLog(t, dir, requestlog.Record{
		RequestID:   "req-1",
		RequestBody: `{"key":"[REDACTED]"}`,
	})
	service := New(Ports{
		RequestLogDirectory: func() string { return dir },
		LocateGuardHits: func(body []byte, kind string, names []string) ([]appapi.SecurityMatch, error) {
			return []appapi.SecurityMatch{{Name: "openai_api_key", Located: false}}, nil
		},
	})
	result, err := service.SecurityExplain("req-1", "secret", []string{"openai_api_key"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != appapi.SecurityExplainRedacted {
		t.Errorf("status = %q, want redacted", result.Status)
	}
}

func TestSecurityExplainCrossRequest(t *testing.T) {
	dir := t.TempDir()
	writeExplainRequestLog(t, dir, requestlog.Record{
		RequestID:   "req-1",
		RequestBody: `{"text":"nothing here"}`,
	})
	service := New(Ports{
		RequestLogDirectory: func() string { return dir },
		LocateGuardHits: func(body []byte, kind string, names []string) ([]appapi.SecurityMatch, error) {
			return []appapi.SecurityMatch{{Name: "known_secret_fragmented", Located: false}}, nil
		},
	})
	result, err := service.SecurityExplain("req-1", "secret", []string{"known_secret_fragmented"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != appapi.SecurityExplainCrossRequest {
		t.Errorf("status = %q, want cross_request", result.Status)
	}
}

// TestAnalyticsAgentFilterReroutesModelDimension: by=model with a non-empty
// agent must still narrow (through agent_buckets — the only table carrying
// the agent dimension), with Agent cleared so the series keys stay
// provider+model; by=agent keeps the agent label.
func TestAnalyticsAgentFilterReroutesModelDimension(t *testing.T) {
	var agentArg string
	service := New(Ports{
		AnalyticsAgents: func(from, to int64, agent, provider, model, granularity string) ([]observestats.AnalyticsBucket, error) {
			agentArg = agent
			return []observestats.AnalyticsBucket{
				{Agent: agent, Provider: "p", Model: "m", Bucket: 1, Requests: 2},
			}, nil
		},
		Analytics: func(from, to int64, provider, model, granularity string) ([]observestats.AnalyticsBucket, error) {
			t.Error("by=model+agent must not read minute_buckets")
			return nil, nil
		},
	})
	out, err := service.Analytics(appapi.AnalyticsQuery{Granularity: "day", By: "model", Agent: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if agentArg != "codex" {
		t.Errorf("agent forwarded = %q, want codex", agentArg)
	}
	if len(out) != 1 || out[0].Agent != "" || out[0].Requests != 2 {
		t.Fatalf("model-dimension buckets = %+v, want agent cleared", out)
	}

	byAgent, err := service.Analytics(appapi.AnalyticsQuery{Granularity: "day", By: "agent", Agent: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	if len(byAgent) != 1 || byAgent[0].Agent != "codex" {
		t.Fatalf("agent-dimension buckets = %+v, want agent kept", byAgent)
	}
}

// TestAnalyticsAgentNamesPassThrough: the facet passes provider/model only
// (the agent filter is deliberately ignored) and a missing port behaves
// like a disabled store (empty, non-nil).
func TestAnalyticsAgentNamesPassThrough(t *testing.T) {
	var nameArgs [4]int64
	service := New(Ports{
		AnalyticsAgentNames: func(from, to int64, provider, model string) []string {
			nameArgs = [4]int64{from, to, int64(len(provider)), int64(len(model))}
			return []string{"codex"}
		},
	})
	names := service.AnalyticsAgentNames(appapi.AnalyticsQuery{From: 1, To: 2, Provider: "p", Model: "m"})
	if len(names) != 1 || names[0] != "codex" || nameArgs != [4]int64{1, 2, 1, 1} {
		t.Fatalf("agent names = %v (args %v)", names, nameArgs)
	}
	bare := New(Ports{})
	if names := bare.AnalyticsAgentNames(appapi.AnalyticsQuery{}); names == nil || len(names) != 0 {
		t.Errorf("nil-port agent names = %v; want empty non-nil", names)
	}
}

// The Security result carries the SERVER-side verdict aggregation (the KPI
// tiles' source): counts follow the query window, cover the whole store (not
// the returned page), and low rides the adjudication stats port because
// lows never enter the queryable store.
func TestSecurityCountsAggregation(t *testing.T) {
	home := setTestHome(t)
	auditPath := filepath.Join(home, "logs", "security.log")
	if err := os.MkdirAll(filepath.Dir(auditPath), 0o700); err != nil {
		t.Fatal(err)
	}
	seed := []seclog.Record{
		{Ts: 1000, Kind: "secret", Names: []string{"api-key"}, Action: "log", Verdict: "high"},
		{Ts: 2000, Kind: "secret", Names: []string{"api-key"}, Action: "log", Verdict: "high"},
		{Ts: 3000, Kind: "path", Names: []string{"ssh"}, Action: "log", Verdict: "medium"},
		{Ts: 4000, Kind: "secret", Names: []string{"api-key"}, Action: "log", Verdict: "error"},
		{Ts: 5000, Kind: "secret", Names: []string{"api-key"}, Action: "log"}, // classic, no verdict
	}
	for i := range seed {
		if err := seclog.AppendSync(filepath.Dir(auditPath), &seed[i]); err != nil {
			t.Fatal(err)
		}
	}

	stats := appapi.SecurityAdjudicationStats{LowVerdicts: 42}
	service := New(Ports{
		Config: func() *configdomain.Config {
			return &configdomain.Config{Guard: configdomain.GuardConfig{Audit: true, AuditPath: auditPath}}
		},
		AdjudicationStats: func() appapi.SecurityAdjudicationStats { return stats },
	})

	// Full window: high=2, medium=1, error=1, skipped=0, low from the port.
	// The page limit (1) must NOT truncate the aggregation.
	result, err := service.Security(appapi.SecurityQuery{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if result.Counts == nil {
		t.Fatal("no counts in result")
	}
	c := result.Counts
	if c.High != 2 || c.Medium != 1 || c.Error != 1 || c.Skipped != 0 || c.Low != 42 {
		t.Errorf("counts = %+v, want high=2 medium=1 error=1 skipped=0 low=42", c)
	}
	if len(result.Records) != 1 {
		t.Errorf("records = %d, want the limited page", len(result.Records))
	}

	// The window narrows server-side: From=3500 drops the two highs.
	narrow, err := service.Security(appapi.SecurityQuery{From: 3500})
	if err != nil {
		t.Fatal(err)
	}
	if nc := narrow.Counts; nc.High != 0 || nc.Error != 1 {
		t.Errorf("narrow counts = %+v, want high=0 error=1", nc)
	}
}
