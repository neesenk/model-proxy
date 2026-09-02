package admin

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"model-proxy/internal/accounts"
	"model-proxy/internal/appapi"
	responsecache "model-proxy/internal/cache"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/fusion"
	obscounters "model-proxy/internal/observe/counters"
	"model-proxy/internal/observe/seclog"
	observestats "model-proxy/internal/observe/stats"
	"model-proxy/internal/pricing"
	"model-proxy/internal/provider"
	runtimestate "model-proxy/internal/runtime"
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
	up, ok := dashboard.Health["up"].(map[string]any)
	if !ok || up["circuit_state"] != "closed" || up["available"] != true {
		t.Errorf("health[up] = %v", dashboard.Health["up"])
	}
	if _, present := up["circuit_until"]; present {
		t.Errorf("healthy provider must not carry circuit_until: %v", up)
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
			}
		},
	})
	out := service.Accounts()
	byName := map[string]appapi.ProviderAccounts{}
	for _, item := range out {
		byName[item.Name] = item
	}
	if len(byName) != 4 {
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
}

func TestTokensProjection(t *testing.T) {
	service := New(Ports{
		TokenUsage: func() map[obscounters.TokenKey]obscounters.TokenUsage {
			return map[obscounters.TokenKey]obscounters.TokenUsage{
				{Provider: "up", Model: "m"}: {Input: 1, Output: 2, CacheCreation: 3, CacheRead: 4, Requests: 5},
			}
		},
	})
	out := service.Tokens()
	if len(out) != 1 {
		t.Fatalf("tokens = %v", out)
	}
	got := out[0]
	if got.Provider != "up" || got.Model != "m" || got.Input != 1 || got.Output != 2 ||
		got.CacheCreation != 3 || got.CacheRead != 4 || got.Requests != 5 {
		t.Errorf("token usage = %+v", got)
	}
}

func TestTokensNilSnapshot(t *testing.T) {
	service := New(Ports{
		TokenUsage: func() map[obscounters.TokenKey]obscounters.TokenUsage { return nil },
	})
	if out := service.Tokens(); len(out) != 0 {
		t.Errorf("tokens = %v, want empty", out)
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
