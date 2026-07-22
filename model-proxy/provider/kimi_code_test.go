package provider

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newTestKimiCode builds a KimiCodeProvider with a temp auth file so AuthHeaders
// tests don't touch the real ~/.model-proxy store.
func newTestKimiCode(t *testing.T) *KimiCodeProvider {
	t.Helper()
	return &KimiCodeProvider{
		ApiKeyBase: &ApiKeyBase{authFile: filepath.Join(t.TempDir(), "kimi-code_apikey.json")},
		cfg:        &Config{OpenAIBaseURL: "https://api.kimi.com/coding/v1"},
	}
}

// One key must authenticate both endpoints: Bearer for OpenAI + /usages, x-api-key
// for Anthropic. Mirrors DeepSeek's contract — deleting either set must turn this red.
func TestKimiCodeAuthHeaders_BothSchemes(t *testing.T) {
	p := newTestKimiCode(t)
	if err := p.SaveKey("sk-kimi-123"); err != nil {
		t.Fatalf("SaveKey: %v", err)
	}
	req, _ := http.NewRequest("POST", "https://api.kimi.com/coding/v1/chat/completions", nil)
	if err := p.AuthHeaders(req); err != nil {
		t.Fatalf("AuthHeaders: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sk-kimi-123" {
		t.Errorf("Authorization: got %q, want %q", got, "Bearer sk-kimi-123")
	}
	if got := req.Header.Get("x-api-key"); got != "sk-kimi-123" {
		t.Errorf("x-api-key: got %q, want %q", got, "sk-kimi-123")
	}
}

// AuthHeaders must fail clearly when not logged in (no cred file).
func TestKimiCodeAuthHeaders_NotLoggedIn(t *testing.T) {
	p := newTestKimiCode(t)
	req, _ := http.NewRequest("GET", "https://api.kimi.com/coding/v1/usages", nil)
	if err := p.AuthHeaders(req); err == nil {
		t.Errorf("AuthHeaders: want error when not logged in, got nil")
	}
}

// RewriteRequest is a no-op: the proxy selects base URL by protocol, so Kimi Code
// must not rewrite the URL itself.
func TestKimiCodeRewriteRequest_NoOp(t *testing.T) {
	p := newTestKimiCode(t)
	for _, tc := range []struct{ url, path string }{
		{"https://api.kimi.com/coding/v1/messages", "/messages"},
		{"https://api.kimi.com/coding/v1/chat/completions", "/chat/completions"},
	} {
		if got, _ := p.RewriteRequest(tc.url, nil, tc.path); got != tc.url {
			t.Errorf("RewriteRequest(%q, %q): got %q, want unchanged", tc.url, tc.path, got)
		}
	}
}

// ProbeRequest targets the Anthropic /v1/messages path with an anthropic body.
func TestKimiCodeProbeRequest(t *testing.T) {
	p := newTestKimiCode(t)
	pr := p.ProbeRequest("kimi-for-coding")
	if pr.Method != http.MethodPost {
		t.Errorf("Method: got %q, want POST", pr.Method)
	}
	if pr.Path != "/v1/messages" {
		t.Errorf("Path: got %q, want /v1/messages", pr.Path)
	}
	// Assert structurally — JSON map key order is non-deterministic.
	var b struct {
		Model     string                   `json:"model"`
		MaxTokens int                      `json:"max_tokens"`
		Messages  []map[string]interface{} `json:"messages"`
	}
	if err := json.Unmarshal(pr.Body, &b); err != nil {
		t.Fatalf("Body parse: %v", err)
	}
	if b.Model != "kimi-for-coding" {
		t.Errorf("Body model: got %q, want kimi-for-coding", b.Model)
	}
	if b.MaxTokens != 1 {
		t.Errorf("Body max_tokens: got %d, want 1", b.MaxTokens)
	}
	if len(b.Messages) != 1 {
		t.Errorf("Body messages: got %d entries, want 1", len(b.Messages))
	}
}

// ExtraHeaders sets anthropic-version (required by the Anthropic endpoint on the
// probe path, which has no client request to copy from).
func TestKimiCodeExtraHeaders(t *testing.T) {
	p := newTestKimiCode(t)
	req, _ := http.NewRequest("POST", "https://api.kimi.com/coding/v1/messages", nil)
	p.ExtraHeaders(req, "/v1/messages")
	if got := req.Header.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version: got %q, want 2023-06-01", got)
	}
}

// usagesURL derives the quota endpoint from openai_base_url + /usages.
func TestKimiCodeUsagesURL(t *testing.T) {
	p := newTestKimiCode(t)
	if got, want := p.usagesURL(), "https://api.kimi.com/coding/v1/usages"; got != want {
		t.Errorf("usagesURL: got %q, want %q", got, want)
	}
	// UsageURL overrides the derived URL (test/mirror seam).
	p.cfg.UsageURL = "https://example.test/usages"
	if got, want := p.usagesURL(), "https://example.test/usages"; got != want {
		t.Errorf("usagesURL with override: got %q, want %q", got, want)
	}
	// No base and no override -> "" (Quota treats as unmeasured).
	p.cfg.OpenAIBaseURL = ""
	p.cfg.UsageURL = ""
	if got := p.usagesURL(); got != "" {
		t.Errorf("usagesURL empty: got %q, want %q", got, "")
	}
}

// ParseKimiCodeQuota: the summary `usage` is the Ultimate weekly window; the 5h
// limit is the Short rate-cap; the booster wallet is a display-only money window.
// Asserts exact Ultimate/Short/Duration/RemainingPct per the quota-marker contract.
func TestParseKimiCodeQuota_Markers(t *testing.T) {
	body := []byte(`{
		"usage": {"name":"Weekly limit","used":400,"limit":1000,"resetAt":"2026-07-21T00:00:00Z"},
		"limits": [
			{"detail":{"used":10,"limit":100,"name":"5h limit","remaining":90,"resetAt":"2026-07-17T15:00:00Z"},"window":{"duration":300,"timeUnit":"MINUTE"}},
			{"detail":{"used":50,"limit":500,"name":"Daily limit"},"window":{"duration":1,"timeUnit":"DAY"}}
		],
		"boosterWallet": {
			"balance":{"type":"BOOSTER","amount":100000000,"amountLeft":80000000},
			"monthlyChargeLimit":{"priceInCents":2000,"currency":"CNY"},
			"monthlyUsed":{"priceInCents":500,"currency":"CNY"},
			"monthlyChargeLimitEnabled": true
		}
	}`)
	s, err := ParseKimiCodeQuota(body, "")
	if err != nil {
		t.Fatalf("ParseKimiCodeQuota: unexpected error %v", err)
	}
	if s == nil {
		t.Fatalf("ParseKimiCodeQuota: got nil snapshot")
	}
	if s.Billing != BillingPlan {
		t.Errorf("Billing: got %v, want BillingPlan", s.Billing)
	}
	// RemainingPct binds to the Ultimate (longest = weekly) window: (1000-400)/1000 = 0.6
	if got, want := s.RemainingPct, 0.6; got != want {
		t.Errorf("RemainingPct: got %v, want %v", got, want)
	}

	// Consensus rule: longest window (weekly, 7d) → Ultimate (hard limit).
	weekly := findWindowByLabel(t, s.Windows, "Weekly limit")
	if !weekly.Ultimate {
		t.Errorf("weekly window: want Ultimate=true, got false")
	}
	if weekly.Short {
		t.Errorf("weekly window: want Short=false, got true")
	}
	if weekly.Duration != 7*24*time.Hour {
		t.Errorf("weekly Duration: got %v, want %v", weekly.Duration, 7*24*time.Hour)
	}
	if got, want := weekly.RemainingPct, 0.6; got != want {
		t.Errorf("weekly RemainingPct: got %v, want %v", got, want)
	}
	if want := time.Date(2026, 7, 21, 0, 0, 0, 0, time.UTC); !weekly.ResetsAt.Equal(want) {
		t.Errorf("weekly ResetsAt: got %v, want %v", weekly.ResetsAt, want)
	}

	// Shorter windows (5h, daily) → Short (soft rate-caps).
	fiveHour := findWindowByLabel(t, s.Windows, "5h limit")
	if !fiveHour.Short {
		t.Errorf("5h window: want Short=true, got false")
	}
	if fiveHour.Ultimate {
		t.Errorf("5h window: want Ultimate=false, got true")
	}
	if fiveHour.Duration != 5*time.Hour {
		t.Errorf("5h Duration: got %v, want %v", fiveHour.Duration, 5*time.Hour)
	}
	// 5h remaining = (100-10)/100 = 0.9
	if got, want := fiveHour.RemainingPct, 0.9; got != want {
		t.Errorf("5h RemainingPct: got %v, want %v", got, want)
	}

	// Daily (1d, shorter than weekly) is also a Short soft rate-cap per the rule.
	daily := findWindowByLabel(t, s.Windows, "Daily limit")
	if !daily.Short {
		t.Errorf("daily window: want Short=true (shorter than weekly), got false")
	}
	if daily.Ultimate {
		t.Errorf("daily window: want Ultimate=false, got true")
	}

	// Booster wallet: amount 100000000 / 1e6 / 100 = 1.00 total; amountLeft -> 0.80.
	booster := findWindowByLabel(t, s.Windows, "Extra usage")
	if got, want := booster.Kind, "money"; got != want {
		t.Errorf("booster Kind: got %q, want %q", got, want)
	}
	if got, want := booster.Total, 1.0; got != want {
		t.Errorf("booster Total: got %v, want %v", got, want)
	}
	if got, want := booster.RemainingPct, -1.0; got != want {
		t.Errorf("booster RemainingPct: got %v, want %v (unmeasured)", got, want)
	}
}

// ParseKimiCodeQuota parses the REAL API shape: numbers as quoted strings
// ("100", "1"), timeUnit "TIME_UNIT_MINUTE", reset field "resetTime", weekly
// summary with only limit/remaining (no used), and a boosterWallet whose
// monthlyChargeLimit.priceInCents is a quoted string. This is a regression test
// for the bug where a quoted priceInCents failed json.Unmarshal → "not kimi-code
// usages format".
func TestParseKimiCodeQuota_RealAPIShape(t *testing.T) {
	body := []byte(`{
		"usage": {"limit":"100","remaining":"100","resetTime":"2026-07-23T18:35:14.745795Z"},
		"limits": [{"window":{"duration":300,"timeUnit":"TIME_UNIT_MINUTE"},"detail":{"limit":"100","used":"2","remaining":"98","resetTime":"2026-07-17T04:35:14.745795Z"}}],
		"boosterWallet": {
			"balance":{"type":"BOOSTER","unit":"UNIT_CURRENCY"},
			"monthlyChargeLimit":{"currency":"CNY","priceInCents":"10000"},
			"monthlyUsed":{"currency":"CNY","priceInCents":"0"},
			"monthlyChargeLimitEnabled": true
		}
	}`)
	s, err := ParseKimiCodeQuota(body, "")
	if err != nil || s == nil {
		t.Fatalf("ParseKimiCodeQuota: err=%v s=%v (regression: quoted-string numbers must parse)", err, s)
	}
	if s.Billing != BillingPlan {
		t.Errorf("Billing: got %v, want BillingPlan", s.Billing)
	}
	// Weekly (longest, no `used`) → Ultimate; used derived = 100-100 = 0; rem=1.0
	weekly := findWindowByLabel(t, s.Windows, "Weekly limit")
	if !weekly.Ultimate {
		t.Errorf("weekly: want Ultimate=true, got false")
	}
	if weekly.Used != 0 || weekly.Total != 100 {
		t.Errorf("weekly Used/Total: got %v/%v, want 0/100", weekly.Used, weekly.Total)
	}
	if weekly.RemainingPct != 1.0 {
		t.Errorf("weekly RemainingPct: got %v, want 1.0", weekly.RemainingPct)
	}
	if want := parseTime(t, "2026-07-23T18:35:14.745795Z"); !weekly.ResetsAt.Equal(want) {
		t.Errorf("weekly ResetsAt: got %v, want %v (resetTime field)", weekly.ResetsAt, want)
	}
	// 5h (300×TIME_UNIT_MINUTE = 5h) → Short; used="2" parsed from quoted string.
	fiveHour := findWindowByLabel(t, s.Windows, "5h limit")
	if !fiveHour.Short {
		t.Errorf("5h: want Short=true, got false")
	}
	if fiveHour.Duration != 5*time.Hour {
		t.Errorf("5h Duration: got %v, want %v", fiveHour.Duration, 5*time.Hour)
	}
	if fiveHour.Used != 2 || fiveHour.Total != 100 {
		t.Errorf("5h Used/Total (quoted strings): got %v/%v, want 2/100", fiveHour.Used, fiveHour.Total)
	}
	// Monthly cap from quoted priceInCents: limit 10000/100 = 100, used 0.
	cap := findWindowByLabel(t, s.Windows, "Monthly cap CNY ")
	if cap.Total != 100 {
		t.Errorf("Monthly cap Total: got %v, want 100 (10000 cents / 100)", cap.Total)
	}
}

func parseTime(t *testing.T, s string) time.Time {
	t.Helper()
	tt, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("parseTime %q: %v", s, err)
	}
	return tt
}

// ParseKimiCodeQuota derives `used` from `remaining` when `used` is absent (field
// spelling drift across CLI versions).
func TestParseKimiCodeQuota_UsedFromRemaining(t *testing.T) {
	body := []byte(`{
		"usage": {"name":"Weekly limit","limit":1000,"remaining":250,"resetAt":"2026-07-21T00:00:00Z"},
		"limits": [{"detail":{"limit":100,"remaining":90},"window":{"duration":300,"timeUnit":"MINUTE"}}]
	}`)
	s, err := ParseKimiCodeQuota(body, "")
	if err != nil || s == nil {
		t.Fatalf("ParseKimiCodeQuota: err=%v s=%v", err, s)
	}
	weekly := findWindowByLabel(t, s.Windows, "Weekly limit")
	// used = 1000 - 250 = 750; remaining pct = 0.25. Weekly (longest) is Ultimate.
	if !weekly.Ultimate {
		t.Errorf("weekly window: want Ultimate=true, got false")
	}
	if got, want := weekly.Used, 750.0; got != want {
		t.Errorf("weekly Used (derived): got %v, want %v", got, want)
	}
	if got, want := weekly.RemainingPct, 0.25; got != want {
		t.Errorf("weekly RemainingPct: got %v, want %v", got, want)
	}
	// The 5h limit (no name, 5h window) is Short; used derived = 100 - 90 = 10.
	var sh *QuotaWindow
	for i := range s.Windows {
		if s.Windows[i].Short {
			sh = &s.Windows[i]
		}
	}
	if sh == nil {
		t.Fatalf("expected a Short (5h) window, got none")
	}
	if got, want := sh.Used, 10.0; got != want {
		t.Errorf("5h Used (derived): got %v, want %v", got, want)
	}
}

// ParseKimiCodeQuota identifies the 5h rate-cap by name when window duration is
// absent (some payloads omit the window block); it is Short, weekly is Ultimate.
func TestParseKimiCodeQuota_5hByNameIsShort(t *testing.T) {
	body := []byte(`{
		"usage": {"name":"Weekly limit","used":100,"limit":1000,"resetAt":"2026-07-21T00:00:00Z"},
		"limits": [{"detail":{"used":10,"limit":100,"name":"5h limit"}}]
	}`)
	s, err := ParseKimiCodeQuota(body, "")
	if err != nil || s == nil {
		t.Fatalf("ParseKimiCodeQuota: err=%v s=%v", err, s)
	}
	w := findWindowByLabel(t, s.Windows, "5h limit")
	if !w.Short {
		t.Errorf("expected Short by name, got false")
	}
	if w.Duration != 5*time.Hour {
		t.Errorf("Short Duration: got %v, want %v", w.Duration, 5*time.Hour)
	}
}

// ParseKimiCodeQuota returns nil for a non-usages body (caller falls back).
func TestParseKimiCodeQuota_NotKimiFormat(t *testing.T) {
	s, err := ParseKimiCodeQuota([]byte(`{"unrelated": true}`), "")
	if s != nil {
		t.Errorf("expected nil snapshot for unrelated body, got %+v", s)
	}
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
}

// ParseKimiCodeQuota degrades to BillingUnknown when no window can be marked
// Ultimate (no resolvable duration → maxDur==0) — the scheduler falls back to
// priority ordering rather than a bogus 0 surplus. Here a single limit has no
// window block and no 5h name, so its duration is unknown.
func TestParseKimiCodeQuota_NoUltimateIsUnknown(t *testing.T) {
	body := []byte(`{
		"limits": [{"detail":{"used":50,"limit":500,"name":"Custom limit"}}]
	}`)
	s, err := ParseKimiCodeQuota(body, "")
	if err != nil || s == nil {
		t.Fatalf("ParseKimiCodeQuota: err=%v s=%v", err, s)
	}
	if s.Billing != BillingUnknown {
		t.Errorf("Billing: got %v, want BillingUnknown (no Ultimate window)", s.Billing)
	}
	if s.RemainingPct != -1 {
		t.Errorf("RemainingPct: got %v, want -1", s.RemainingPct)
	}
}

// Quota returns BillingUnknown (never a non-nil error) on an HTTP failure, so the
// scheduler treats Kimi Code as unmeasured rather than crashing the poll.
func TestKimiCodeQuota_HTTPFailureIsUnknown(t *testing.T) {
	p := newTestKimiCode(t)
	// No cred file → AuthHeaders fails → BillingUnknown, nil error.
	s, err := p.Quota()
	if err != nil {
		t.Errorf("Quota: want nil error on auth failure, got %v", err)
	}
	if s == nil || s.Billing != BillingUnknown {
		t.Errorf("Quota: want BillingUnknown snapshot, got %+v", s)
	}
}

// Quota fetches /usages end-to-end: asserts the request carries Bearer + Accept,
// and the parsed snapshot has the weekly Ultimate + 5h Short windows. Also covers
// the 404-membership hint and the non-200 path.
func TestKimiCodeQuota_FetchAndParse(t *testing.T) {
	var gotAuth, gotAccept string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{
			"usage": {"name":"Weekly limit","used":200,"limit":1000,"resetAt":"2026-07-21T00:00:00Z"},
			"limits": [{"detail":{"used":10,"limit":100,"name":"5h limit","remaining":90},"window":{"duration":300,"timeUnit":"MINUTE"}}]
		}`)
	}))
	defer srv.Close()

	p := newTestKimiCode(t)
	p.cfg.UsageURL = srv.URL + "/usages"
	if err := p.SaveKey("sk-kimi-9"); err != nil {
		t.Fatalf("SaveKey: %v", err)
	}
	s, err := p.Quota()
	if err != nil {
		t.Fatalf("Quota: unexpected error %v", err)
	}
	if gotAuth != "Bearer sk-kimi-9" {
		t.Errorf("request Authorization: got %q, want Bearer sk-kimi-9", gotAuth)
	}
	if gotAccept != "application/json" {
		t.Errorf("request Accept: got %q, want application/json", gotAccept)
	}
	if s.Billing != BillingPlan {
		t.Errorf("Billing: got %v, want BillingPlan", s.Billing)
	}
	// RemainingPct binds to the Ultimate (weekly) window: (1000-200)/1000 = 0.8
	if s.RemainingPct != 0.8 {
		t.Errorf("RemainingPct: got %v, want 0.8", s.RemainingPct)
	}
	if !hasUltimate(s.Windows) {
		t.Errorf("expected an Ultimate window")
	}
	var short bool
	for _, w := range s.Windows {
		if w.Short {
			short = true
		}
	}
	if !short {
		t.Errorf("expected a Short (5h) window")
	}
}

// Quota 404 surfaces the membership hint and stays BillingUnknown (never a
// non-nil error).
func TestKimiCodeQuota_404Hint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	p := newTestKimiCode(t)
	p.cfg.UsageURL = srv.URL + "/usages"
	if err := p.SaveKey("sk-kimi-9"); err != nil {
		t.Fatalf("SaveKey: %v", err)
	}
	s, err := p.Quota()
	if err != nil {
		t.Fatalf("Quota: want nil error on 404, got %v", err)
	}
	if s.Billing != BillingUnknown {
		t.Errorf("Billing: got %v, want BillingUnknown", s.Billing)
	}
	if s.Err == "" || !strings.Contains(s.Err, "membership") {
		t.Errorf("Err: got %q, want it to mention membership", s.Err)
	}
}

// Usage prints the provider header + parsed windows on success, and falls back to
// config models on fetch failure (never mute).
func TestKimiCodeUsage_SuccessAndFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"usage":{"name":"Weekly limit","used":100,"limit":1000,"resetAt":"2026-07-21T00:00:00Z"},"limits":[{"detail":{"used":10,"limit":100,"name":"5h limit"},"window":{"duration":300,"timeUnit":"MINUTE"}}]}`)
	}))
	defer srv.Close()

	p := newTestKimiCode(t)
	p.cfg.ProviderName = "kimi-code"
	p.cfg.UsageURL = srv.URL + "/usages"
	p.cfg.Models = []string{"kimi-for-coding", "k3"}
	if err := p.SaveKey("sk-kimi-9"); err != nil {
		t.Fatalf("SaveKey: %v", err)
	}
	out := captureStdoutProvider(func() {
		if err := p.Usage(); err != nil {
			t.Fatalf("Usage: %v", err)
		}
	})
	for _, want := range []string{"kimi-code", "Plan:", "Weekly limit", "5h limit"} {
		if !strings.Contains(out, want) {
			t.Errorf("usage output missing %q:\n%s", want, out)
		}
	}

	// Fallback path: no cred file → Quota fails → listConfigModels used.
	p2 := newTestKimiCode(t)
	p2.cfg.Models = []string{"k3"}
	out2 := captureStdoutProvider(func() {
		if err := p2.Usage(); err != nil {
			t.Fatalf("Usage fallback: %v", err)
		}
	})
	for _, want := range []string{"not logged in", "1 models", "k3"} {
		if !strings.Contains(out2, want) {
			t.Errorf("fallback output missing %q:\n%s", want, out2)
		}
	}
}

func findWindowByLabel(t *testing.T, ws []QuotaWindow, label string) QuotaWindow {
	t.Helper()
	for _, w := range ws {
		if w.Label == label {
			return w
		}
	}
	t.Fatalf("window %q not found in %d windows", label, len(ws))
	return QuotaWindow{}
}
