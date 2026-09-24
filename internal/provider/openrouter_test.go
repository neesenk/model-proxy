package provider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestOpenRouter builds an OpenRouterProvider with a temp auth file so
// AuthHeaders tests don't touch the real ~/.model-proxy store.
func newTestOpenRouter(t *testing.T, cfg *Config) *OpenRouterProvider {
	t.Helper()
	if cfg == nil {
		cfg = &Config{}
	}
	if cfg.ProviderID == "" {
		cfg.ProviderID = "openrouter"
	}
	p, err := New(cfg, "openrouter")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	op := p.(*OpenRouterProvider)
	op.ApiKeyBase = &ApiKeyBase{authFile: filepath.Join(t.TempDir(), "openrouter_apikey.json")}
	return op
}

// OpenRouter documents exactly one auth shape: Authorization: Bearer. The
// default ApiKeyBase injection must hold (and strip x-api-key — the Anthropic
// Messages leg also reads Bearer, so the extra header buys nothing).
func TestOpenRouterAuthHeaders_BearerOnly(t *testing.T) {
	p := newTestOpenRouter(t, &Config{OpenAIBaseURL: "https://openrouter.ai/api/v1"})
	if err := p.SaveKey("sk-or-v1-123"); err != nil {
		t.Fatalf("SaveKey: %v", err)
	}
	req, _ := http.NewRequest("POST", "https://openrouter.ai/api/v1/messages", nil)
	if err := p.AuthHeaders(req); err != nil {
		t.Fatalf("AuthHeaders: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sk-or-v1-123" {
		t.Errorf("Authorization = %q, want Bearer <key>", got)
	}
	if got := req.Header.Get("x-api-key"); got != "" {
		t.Errorf("x-api-key = %q, want empty (ApiKeyBase strips it)", got)
	}
}

// A pool-unrolled virtual binds its OWN key: AuthHeaders must use the bound
// key and never fall back to another account's file (credential isolation).
func TestOpenRouterAuthHeaders_BoundKeyIsolation(t *testing.T) {
	for _, key := range []string{"BOUND-KEY-A", "BOUND-KEY-B"} {
		bound := &OpenRouterProvider{
			ApiKeyBase: NewApiKeyBaseWithKey("openrouter", key),
			cfg:        &Config{OpenAIBaseURL: "https://openrouter.ai/api/v1"},
		}
		req, _ := http.NewRequest("GET", "https://openrouter.ai/api/v1/models", nil)
		if err := bound.AuthHeaders(req); err != nil {
			t.Fatalf("AuthHeaders: %v", err)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer "+key {
			t.Errorf("Authorization = %q, want the bound key %q", got, key)
		}
	}
}

// RewriteRequest is a no-op: the proxy selects the upstream base URL by
// protocol, so OpenRouter must not rewrite the URL itself (deepseek pattern).
func TestOpenRouterRewriteRequest_NoOp(t *testing.T) {
	p := newTestOpenRouter(t, nil)
	for _, tc := range []struct{ url, path string }{
		{"https://openrouter.ai/api/v1/messages", "/v1/messages"},
		{"https://openrouter.ai/api/v1/chat/completions", "/chat/completions"},
	} {
		if got, _ := p.RewriteRequest(tc.url, nil, tc.path); got != tc.url {
			t.Errorf("RewriteRequest(%q, %q) = %q, want unchanged", tc.url, tc.path, got)
		}
	}
}

// FetchModels lists the catalog via the OpenAI-compatible /models endpoint
// with Bearer auth (slashed vendor-prefixed ids pass through verbatim).
func TestOpenRouterFetchModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path = %q, want /models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-or-v1-list" {
			t.Errorf("Authorization = %q, want Bearer <key>", got)
		}
		w.Write([]byte(`{"data":[{"id":"anthropic/claude-sonnet-5"},{"id":"openai/gpt-5.5"},{"id":"z-ai/glm-5.3:free"}]}`))
	}))
	defer srv.Close()
	p := newTestOpenRouter(t, &Config{OpenAIBaseURL: srv.URL})
	if err := p.SaveKey("sk-or-v1-list"); err != nil {
		t.Fatalf("SaveKey: %v", err)
	}
	ids, err := p.FetchModels()
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	want := []string{"anthropic/claude-sonnet-5", "openai/gpt-5.5", "z-ai/glm-5.3:free"}
	if len(ids) != len(want) {
		t.Fatalf("FetchModels = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("FetchModels[%d] = %q, want %q", i, ids[i], want[i])
		}
	}
}

// A 401 must surface as an error so the caller (models refresh / usage
// display) falls back to the config models: list instead of an empty list.
func TestOpenRouterFetchModels_ErrorOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"User not found.","code":401}}`))
	}))
	defer srv.Close()
	p := newTestOpenRouter(t, &Config{OpenAIBaseURL: srv.URL})
	if err := p.SaveKey("sk-or-v1-bad"); err != nil {
		t.Fatalf("SaveKey: %v", err)
	}
	if _, err := p.FetchModels(); err == nil {
		t.Error("FetchModels on 401 returned nil error; want error so caller falls back to config models")
	}
}

// ProbeRequest keeps the baseProbe OpenAI default (deepseek parity): the
// probe framework selects the anthropic base + /v1/messages when
// anthropic_base_url is set, so a provider-side override would be dead weight.
func TestOpenRouterProbeRequest_DefaultOpenAIShape(t *testing.T) {
	p := newTestOpenRouter(t, nil)
	pr := p.ProbeRequest("anthropic/claude-sonnet-5")
	if pr.Path != "/chat/completions" {
		t.Errorf("ProbeRequest.Path = %q, want the baseProbe default /chat/completions", pr.Path)
	}
	if !strings.Contains(string(pr.Body), "anthropic/claude-sonnet-5") {
		t.Errorf("ProbeRequest.Body %q missing model id", pr.Body)
	}
}

// ExtraHeaders sets anthropic-version: the forward path's header whitelist
// does not carry the client's anthropic-version, and OpenRouter's Anthropic
// Messages endpoint behaves like the real Anthropic API (kimi-code/mimo
// lesson).
func TestOpenRouterExtraHeaders_AnthropicVersion(t *testing.T) {
	p := newTestOpenRouter(t, nil)
	req, _ := http.NewRequest("POST", "https://openrouter.ai/api/v1/messages", nil)
	p.ExtraHeaders(req, "/v1/messages")
	if got := req.Header.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want 2023-06-01", got)
	}
}

// openRouterKeyFixture is a realistic /api/v1/key body: a labeled key with a
// per-key credit cap, all three spend counters, free tier with the free-model
// daily request cap (shape from the api reference "Limits" page).
const openRouterKeyFixture = `{"data":{"label":"model-proxy","limit":10,"limit_remaining":7.5,
"limit_reset":"daily","usage":42.0,"usage_daily":1.25,"usage_weekly":9.5,"usage_monthly":30.25,
"is_free_tier":false,"free_model_daily_requests":{"used":12,"limit":50,"remaining":38}}}`

// Quota parses /key into a pay-as-you-go snapshot: BillingPayG with
// RemainingPct -1 (no windowed budget), the key-cap window carrying the real
// remaining fraction, three spend counters, and the free-model request window.
func TestOpenRouterQuota_ParsesKeyWindows(t *testing.T) {
	now := time.Date(2026, 9, 24, 12, 30, 0, 0, time.UTC) // Thursday
	s := ParseOpenRouterKeyQuota([]byte(openRouterKeyFixture), now)
	if s.Err != "" {
		t.Fatalf("parse error: %s", s.Err)
	}
	if s.Billing != BillingPayG {
		t.Errorf("Billing = %v, want BillingPayG (prepaid credits)", s.Billing)
	}
	if s.RemainingPct != -1 {
		t.Errorf("RemainingPct = %v, want -1 (no windowed budget)", s.RemainingPct)
	}
	if s.Account != "model-proxy" {
		t.Errorf("Account = %q, want the key label", s.Account)
	}
	if len(s.Windows) != 5 {
		t.Fatalf("windows = %d, want 5 (cap + 3 spend + free-model)\n%+v", len(s.Windows), s.Windows)
	}
	cap, daily, weekly, monthly, free := s.Windows[0], s.Windows[1], s.Windows[2], s.Windows[3], s.Windows[4]
	if cap.Label != "Key credit cap (daily reset)" || cap.Total != 10 || cap.Used != 2.5 || cap.RemainingPct != 0.75 {
		t.Errorf("key cap window = %+v, want limit 10 / used 2.5 / 75%% remaining", cap)
	}
	if daily.Used != 1.25 || daily.Kind != "money" || daily.RemainingPct != -1 {
		t.Errorf("daily window = %+v, want used 1.25 unmeasured", daily)
	}
	if weekly.Used != 9.5 || monthly.Used != 30.25 {
		t.Errorf("spend windows = %+v %+v, want weekly 9.5 monthly 30.25", weekly, monthly)
	}
	if free.Kind != "requests" || free.Used != 12 || free.Total != 50 || free.RemainingPct != 0.76 {
		t.Errorf("free-model window = %+v, want 12/50 with 76%% remaining", free)
	}
	// Reset boundaries are the documented UTC period edges: daily = next UTC
	// midnight, weekly = next Monday 00:00 UTC, monthly = 1st of next month.
	if want := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC); !daily.ResetsAt.Equal(want) {
		t.Errorf("daily ResetsAt = %v, want %v", daily.ResetsAt, want)
	}
	if want := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC); !weekly.ResetsAt.Equal(want) {
		t.Errorf("weekly ResetsAt = %v, want next Monday %v", weekly.ResetsAt, want)
	}
	if want := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC); !monthly.ResetsAt.Equal(want) {
		t.Errorf("monthly ResetsAt = %v, want %v", monthly.ResetsAt, want)
	}
}

// A capped key without limit_remaining keeps the window but stays unmeasured;
// an uncapped key (null limit) has no cap window at all. Free tier adds its
// note and drops nothing.
func TestOpenRouterQuota_UncappedAndFreeTier(t *testing.T) {
	uncapped := ParseOpenRouterKeyQuota([]byte(`{"data":{"label":"k","usage_daily":1}}`), time.Now())
	if len(uncapped.Windows) != 3 {
		t.Errorf("uncapped windows = %+v, want only the 3 spend counters", uncapped.Windows)
	}
	free := ParseOpenRouterKeyQuota([]byte(`{"data":{"label":"k","is_free_tier":true,
"free_model_daily_requests":{"used":3,"limit":50,"remaining":47}}}`), time.Now())
	if len(free.Windows) != 4 {
		t.Errorf("free-tier windows = %+v, want 3 spend + free-model", free.Windows)
	}
	if joined := strings.Join(free.Notes, "\n"); !strings.Contains(joined, "free tier") {
		t.Errorf("Notes %q missing free-tier marker", joined)
	}
}

// Quota failures follow the shared contract: HTTP errors and malformed bodies
// become BillingUnknown snapshots carrying the error, never a non-nil error.
func TestOpenRouterQuota_Failures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"User not found.","code":401}}`))
	}))
	defer srv.Close()
	p := newTestOpenRouter(t, &Config{UsageURL: srv.URL + "/api/v1/key"})
	if err := p.SaveKey("sk-or-v1-bad"); err != nil {
		t.Fatalf("SaveKey: %v", err)
	}
	s, err := p.Quota()
	if err != nil {
		t.Fatalf("Quota returned error %v, want (snapshot, nil)", err)
	}
	if s.Billing != BillingUnknown || s.Err == "" {
		t.Errorf("HTTP failure snapshot = %+v, want BillingUnknown with Err", s)
	}
	bad := ParseOpenRouterKeyQuota([]byte(`not-json`), time.Now())
	if bad.Billing != BillingUnknown || bad.Err == "" {
		t.Errorf("malformed body snapshot = %+v, want BillingUnknown with Err", bad)
	}
}

// Usage() must print the "Provider: <name>" first line (the usage-display
// contract) and the parsed /key windows; it never fails on a fetch error (the
// shared CLI error lines already printed).
func TestOpenRouterUsage_PrintsProviderLine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(openRouterKeyFixture))
	}))
	defer srv.Close()
	p := newTestOpenRouter(t, &Config{UsageURL: srv.URL + "/api/v1/key", Models: []string{"openai/gpt-5.5"}})
	if err := p.SaveKey("sk-or-v1-usage"); err != nil {
		t.Fatalf("SaveKey: %v", err)
	}
	out := captureStdoutProvider(func() {
		if err := p.Usage(); err != nil {
			t.Fatalf("Usage: %v", err)
		}
	})
	for _, want := range []string{"Provider:  ", "openrouter", "model-proxy", "Key credit cap", "Spend (today)"} {
		if !strings.Contains(out, want) {
			t.Errorf("usage output missing %q:\n%s", want, out)
		}
	}
}

// OpenRouter's chat endpoint speaks the native reasoning object shape — the
// r→chat dialect must render reasoning:{effort} (the dialect this repo named
// after the vendor), and no ChatEffortProfile may be registered (native
// pass-through, like aqp). Pins the protocol_hint.go registration.
func TestOpenRouterReasoningDialect(t *testing.T) {
	if got := ChatReasoningMode("openrouter"); got != "openrouter" {
		t.Errorf("ChatReasoningMode(openrouter) = %q, want \"openrouter\" (native reasoning:{effort} object)", got)
	}
	if p := ChatEffortProfile("openrouter", "openai/gpt-5.5"); len(p.Enum) > 0 || p.EnumOnly {
		t.Errorf("ChatEffortProfile(openrouter) = %+v, want the zero profile (native pass-through)", p)
	}
}

// The provider id is registered and JSON-tag fidelity of the parser is pinned
// by the fixture tests above; this guards the registry wiring itself.
func TestOpenRouterRegistered(t *testing.T) {
	if !json.Valid([]byte(openRouterKeyFixture)) {
		t.Fatal("fixture is not valid JSON")
	}
	p, err := New(&Config{ProviderID: "openrouter"}, "openrouter")
	if err != nil {
		t.Fatalf("New(openrouter): %v", err)
	}
	if _, ok := p.(*OpenRouterProvider); !ok {
		t.Fatalf("New(openrouter) returned %T", p)
	}
}

// Logout deletes the stored key (apikey pool contract).
func TestOpenRouterLogout_DeletesKey(t *testing.T) {
	p := newTestOpenRouter(t, &Config{OpenAIBaseURL: "https://openrouter.ai/api/v1"})
	if err := p.SaveKey("sk-or-v1-bye"); err != nil {
		t.Fatalf("SaveKey: %v", err)
	}
	if err := p.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	req, _ := http.NewRequest("GET", "https://openrouter.ai/api/v1/models", nil)
	if err := p.AuthHeaders(req); err == nil {
		t.Error("AuthHeaders after Logout: want error (key deleted), got nil")
	}
}
