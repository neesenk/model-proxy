package provider

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// codexRequestAuth models the complete OAuth request contract without relying
// on an on-disk token. The CodexOAuth injector itself is covered in auth_test;
// these fetch tests guard that Quota and Usage actually send all its headers.
type codexRequestAuth struct{}

func (codexRequestAuth) Inject(req *http.Request) error {
	req.Header.Set("Authorization", "Bearer quota-token")
	req.Header.Set("originator", "codex_cli_rs")
	req.Header.Set("ChatGPT-Account-Id", "acct-quota")
	return nil
}
func (codexRequestAuth) Refresh() error { return nil }

type quotaErrAuth struct{}

func (quotaErrAuth) Inject(*http.Request) error { return errors.New("not logged in") }
func (quotaErrAuth) Refresh() error             { return nil }

// quota_fetch_test.go covers each provider's Quota() fetch+parse path directly
// (introduced in Phase 2 when fetch*Quota moved from main into the provider
// struct methods). Uses httptest mocks for codex/zhipu/deepseek; the aqp path
// injects a MonthlyUsage predicate; volcengine's signed GetAFPUsage targets a
// hardcoded host, so its success path can't be mocked and is covered only via
// the error branches here (the parser is covered separately).

// --- codex ---

func TestCodexProvider_Quota_Parsed(t *testing.T) {
	body := `{"email":"a@b.com","plan_type":"pro",` +
		`"rate_limit":{"allowed":true,"limit_reached":false,` +
		`"primary_window":{"used_percent":30,"limit_window_seconds":18000,"reset_after_seconds":12000},` +
		`"secondary_window":{"used_percent":60,"limit_window_seconds":604800,"reset_after_seconds":300000}},` +
		`"spend_control":{"reached":false,"individual_limit":{"used":"5","limit":"20","remaining":"15","used_percent":25,"reset_after_seconds":2500000}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/wham/usage" {
			t.Errorf("codex path=%q want /wham/usage", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer quota-token" {
			t.Errorf("Authorization=%q want %q", got, "Bearer quota-token")
		}
		if got := r.Header.Get("originator"); got != "codex_cli_rs" {
			t.Errorf("originator=%q want %q", got, "codex_cli_rs")
		}
		if got := r.Header.Get("ChatGPT-Account-Id"); got != "acct-quota" {
			t.Errorf("ChatGPT-Account-Id=%q want %q", got, "acct-quota")
		}
		w.Write([]byte(body))
	}))
	defer srv.Close()
	p := &CodexProvider{cfg: &Config{OpenAIBaseURL: srv.URL + "/codex"}, auth: codexRequestAuth{}}
	s, err := p.Quota()
	if err != nil {
		t.Fatal(err)
	}
	if s.Billing != BillingPlan {
		t.Errorf("Billing=%v want Plan", s.Billing)
	}
	if s.RemainingPct != 0.75 {
		t.Errorf("RemainingPct=%v want 0.75 (spend ultimate)", s.RemainingPct)
	}
	if s.Account != "a@b.com" || s.Plan != "pro" {
		t.Errorf("Account/Plan=%q/%q", s.Account, s.Plan)
	}
}

func TestCodexProvider_Quota_NotLoggedIn(t *testing.T) {
	p := &CodexProvider{cfg: &Config{OpenAIBaseURL: "http://x.invalid/codex"}, auth: quotaErrAuth{}}
	s, err := p.Quota()
	if err != nil {
		t.Fatalf("Quota() error=%v want nil", err)
	}
	if s == nil || s.Billing != BillingUnknown || s.Err != "not logged in" {
		t.Fatalf("Quota()=%+v want BillingUnknown with exact not-logged-in error", s)
	}
}

func TestCodexProvider_Quota_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
	}))
	defer srv.Close()
	p := &CodexProvider{cfg: &Config{OpenAIBaseURL: srv.URL + "/codex"}, auth: fakeAuth{key: "k"}}
	s, err := p.Quota()
	if err != nil {
		t.Fatal(err)
	}
	if s.Billing != BillingUnknown || s.Err != "HTTP 502" {
		t.Errorf("got %+v want BillingUnknown/HTTP 502", s)
	}
}

// TestCodexProvider_Quota_MalformedBody: a 200 wham/usage with an unparseable
// body must yield BillingUnknown, never a non-nil error, so the quota poll
// survives a bad upstream response. Regression guard: ParseCodexQuota returns
// (nil, err) on json failure and Quota() used to propagate that error verbatim,
// breaking the "never a non-nil error" contract shared with the other providers.
func TestCodexProvider_Quota_MalformedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`not-json`))
	}))
	defer srv.Close()
	p := &CodexProvider{cfg: &Config{OpenAIBaseURL: srv.URL + "/codex"}, auth: fakeAuth{key: "k"}}
	s, err := p.Quota()
	if err != nil {
		t.Fatalf("Quota() returned non-nil error %v (contract: never a non-nil error)", err)
	}
	if s == nil || s.Billing != BillingUnknown {
		t.Fatalf("got %+v want BillingUnknown snapshot", s)
	}
	if !strings.HasPrefix(s.Err, "parse wham/usage") {
		t.Errorf("Err=%q want prefix %q", s.Err, "parse wham/usage")
	}
}

// --- zhipu ---

func TestZhipuProvider_Quota_Parsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"success":true,"data":{"level":"GLM Coding Plan","limits":[` +
			`{"type":"TOKENS_LIMIT","unit":6,"percentage":70,"nextResetTime":1750000000000,"usage":200000,"currentValue":140000,"remaining":60000}]}}`))
	}))
	defer srv.Close()
	p := &ZhipuProvider{ApiKeyBase: NewApiKeyBaseWithKey("zhipu", "k"), cfg: &Config{UsageURL: srv.URL}}
	s, err := p.Quota()
	if err != nil {
		t.Fatal(err)
	}
	if s.Billing != BillingPlan {
		t.Errorf("Billing=%v want Plan", s.Billing)
	}
	if s.RemainingPct != 0.3 {
		t.Errorf("RemainingPct=%v want 0.3", s.RemainingPct)
	}
}

func TestZhipuProvider_Quota_NotZhipuFormat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"object":"list","data":[]}`))
	}))
	defer srv.Close()
	p := &ZhipuProvider{ApiKeyBase: NewApiKeyBaseWithKey("zhipu", "k"), cfg: &Config{UsageURL: srv.URL}}
	s, _ := p.Quota()
	if s.Billing != BillingUnknown || s.Err != "not zhipu quota format" {
		t.Errorf("got %+v want BillingUnknown/not-zhipu-format", s)
	}
}

func TestZhipuProvider_Quota_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	p := &ZhipuProvider{ApiKeyBase: NewApiKeyBaseWithKey("zhipu", "k"), cfg: &Config{UsageURL: srv.URL}}
	s, _ := p.Quota()
	if s.Billing != BillingUnknown || s.Err != "HTTP 500" {
		t.Errorf("got %+v want BillingUnknown/HTTP 500", s)
	}
}

func TestZhipuProvider_Quota_NotLoggedIn(t *testing.T) {
	p := &ZhipuProvider{ApiKeyBase: &ApiKeyBase{}, cfg: &Config{UsageURL: "http://x.invalid"}}
	s, err := p.Quota()
	if err != nil {
		t.Fatalf("Quota() error=%v want nil", err)
	}
	const wantErr = "not logged in; run `model-proxy login` for this provider"
	if s == nil || s.Billing != BillingUnknown || s.Err != wantErr {
		t.Fatalf("Quota()=%+v want BillingUnknown with exact not-logged-in error", s)
	}
}

// --- deepseek ---

func TestDeepSeekProvider_Quota_Parsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"is_available":true,"balance_infos":[` +
			`{"currency":"CNY","total_balance":"10.50","granted_balance":"8.00","topped_up_balance":"2.50"}]}`))
	}))
	defer srv.Close()
	p := &DeepSeekProvider{ApiKeyBase: NewApiKeyBaseWithKey("deepseek", "k"), cfg: &Config{UsageURL: srv.URL}}
	s, err := p.Quota()
	if err != nil {
		t.Fatal(err)
	}
	if s.Billing != BillingPayG {
		t.Errorf("Billing=%v want PayG", s.Billing)
	}
	if s.RemainingPct != -1 {
		t.Errorf("RemainingPct=%v want -1", s.RemainingPct)
	}
	if len(s.Windows) != 1 || s.Windows[0].Total != 10.5 {
		t.Errorf("windows=%+v", s.Windows)
	}
	// No breakdown in the UI - DetailLabel must stay empty (renderAccountUsage
	// gates the Details section on a non-empty DetailLabel).
	if s.Windows[0].DetailLabel != "" {
		t.Errorf("DetailLabel=%q want empty (no breakdown in UI)", s.Windows[0].DetailLabel)
	}
}

func TestDeepSeekProvider_Quota_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()
	p := &DeepSeekProvider{ApiKeyBase: NewApiKeyBaseWithKey("deepseek", "k"), cfg: &Config{UsageURL: srv.URL}}
	s, _ := p.Quota()
	if s.Billing != BillingUnknown || s.Err != "HTTP 503" {
		t.Errorf("got %+v want BillingUnknown/HTTP 503", s)
	}
}

func TestDeepSeekProvider_Quota_NotLoggedIn(t *testing.T) {
	p := &DeepSeekProvider{ApiKeyBase: &ApiKeyBase{}, cfg: &Config{UsageURL: "http://x.invalid"}}
	s, err := p.Quota()
	if err != nil {
		t.Fatalf("Quota() error=%v want nil", err)
	}
	const wantErr = "not logged in; run `model-proxy login` for this provider"
	if s == nil || s.Billing != BillingUnknown || s.Err != wantErr {
		t.Fatalf("Quota()=%+v want BillingUnknown with exact not-logged-in error", s)
	}
}

// --- aqp ---

// writeAqpStore writes an aqp account store (SSO cookie + project_id) for the
// fetch tests. Returns the store path.
func writeAqpStore(t *testing.T, path, projectID string) {
	t.Helper()
	if err := SaveAqpAccount(path, &AqpAccountData{
		Email:            "a@b.com",
		ProjectID:        projectID,
		SSOSessionCookie: "SSO_C=fake",
	}); err != nil {
		t.Fatal(err)
	}
}

// TestAqpProvider_Quota_NotLoggedIn: no store file -> BillingUnknown "not logged in".
func TestAqpProvider_Quota_NotLoggedIn(t *testing.T) {
	p := &AqpProvider{cfg: &Config{OAuthAuthFile: filepath.Join(t.TempDir(), "nope.json")}}
	s, err := p.Quota()
	if err != nil {
		t.Fatal(err)
	}
	if s.Billing != BillingUnknown || s.Err != "not logged in" {
		t.Errorf("got %+v want BillingUnknown/not logged in", s)
	}
}

// TestAqpProvider_Quota_NoProjectID: store present but project_id empty ->
// BillingUnknown "no project_id" (no network call).
func TestAqpProvider_Quota_NoProjectID(t *testing.T) {
	store := filepath.Join(t.TempDir(), "aqp_oauth_auth.json")
	writeAqpStore(t, store, "")
	p := &AqpProvider{cfg: &Config{OAuthAuthFile: store}}
	s, _ := p.Quota()
	if s.Billing != BillingUnknown || s.Err == "" {
		t.Errorf("got %+v want BillingUnknown with non-empty Err", s)
	}
}

// TestAqpProvider_Quota_HTTPError: store ok, but the monthly_usage endpoint
// returns a non-zero retcode -> BillingUnknown carrying the wrapped error.
func TestAqpProvider_Quota_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"retcode":1,"message":"denied"}`))
	}))
	defer srv.Close()
	store := filepath.Join(t.TempDir(), "aqp_oauth_auth.json")
	writeAqpStore(t, store, "proj-1")
	p := &AqpProvider{cfg: &Config{OAuthAuthFile: store, AqpBaseURL: srv.URL}}
	s, err := p.Quota()
	if err != nil {
		t.Fatal(err)
	}
	if s.Billing != BillingUnknown || s.Err == "" {
		t.Errorf("got %+v want BillingUnknown with non-empty Err", s)
	}
}

// TestAqpProvider_Quota_SessionExpired: a 401 "Session expired" (aged-out SSO
// cookie) returns an envelope with no retcode/data. Pre-fix this fell through to
// the misleading "monthly usage response missing data"; now it must surface a
// clear session-expired + re-login error. Asserts the exact markers so a
// regression that drops the status check turns the test red.
func TestAqpProvider_Quota_SessionExpired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"Session expired"}`))
	}))
	defer srv.Close()
	store := filepath.Join(t.TempDir(), "aqp_oauth_auth.json")
	writeAqpStore(t, store, "proj-1")
	p := &AqpProvider{cfg: &Config{OAuthAuthFile: store, AqpBaseURL: srv.URL}}
	s, err := p.Quota()
	if err != nil {
		t.Fatal(err)
	}
	if s.Billing != BillingUnknown {
		t.Errorf("Billing=%v want BillingUnknown", s.Billing)
	}
	if s.Err == "" {
		t.Fatal("Err is empty - 401 should produce a non-empty error")
	}
	for _, want := range []string{"session expired", "401", "re-login"} {
		if !strings.Contains(s.Err, want) {
			t.Errorf("Err=%q missing %q", s.Err, want)
		}
	}
	// Must NOT be the pre-fix misleading message.
	if strings.Contains(s.Err, "missing data") {
		t.Errorf("Err=%q should not be the misleading 'missing data' message", s.Err)
	}
}

// TestAqpProvider_Quota_Parsed: store + mock monthly_usage -> parsed plan snapshot.
func TestAqpProvider_Quota_Parsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != aqpMonthlyUsagePath || r.Method != http.MethodPost {
			t.Errorf("aqp monthly_usage: %s %s, want POST %s", r.Method, r.URL.Path, aqpMonthlyUsagePath)
		}
		w.Write([]byte(`{"retcode":0,"data":{"project_id":"proj-1","selected_year":2026,"selected_month":7,"total_amount":100,"usage":30,"balance":70,"plan":"CQP"}}`))
	}))
	defer srv.Close()
	store := filepath.Join(t.TempDir(), "aqp_oauth_auth.json")
	writeAqpStore(t, store, "proj-1")
	p := &AqpProvider{cfg: &Config{OAuthAuthFile: store, AqpBaseURL: srv.URL}}
	s, err := p.Quota()
	if err != nil {
		t.Fatal(err)
	}
	if s.Billing != BillingPlan || s.RemainingPct != 0.7 {
		t.Errorf("got %+v want Plan/0.7", s)
	}
}

// --- volcengine ---

func TestVolcengineProvider_Quota_NoCreds(t *testing.T) {
	p := &VolcengineProvider{cfg: &Config{}} // no AK/SK, no cred file
	s, err := p.Quota()
	if err != nil {
		t.Fatal(err)
	}
	if s.Billing != BillingUnknown || s.Err != "AK/SK not configured" {
		t.Errorf("got %+v want BillingUnknown/AK/SK not configured", s)
	}
}

func TestVolcengineProvider_Quota_BoundGetAFPFails(t *testing.T) {
	// Bound AK/SK, but getAFPUsage targets the hardcoded real Volcengine host;
	// force a dead proxy so the signed request fails fast -> BillingUnknown.
	deadSrv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := deadSrv.Listener.Addr().String()
	deadSrv.Close()
	t.Setenv("HTTPS_PROXY", "http://"+addr)
	t.Setenv("HTTP_PROXY", "http://"+addr)
	p := &VolcengineProvider{cfg: &Config{AccessKey: "AK", SecretKey: "SK"}}
	s, _ := p.Quota()
	if s.Billing != BillingUnknown || s.Err == "" {
		t.Errorf("got %+v want BillingUnknown with non-empty Err", s)
	}
}

func TestVolcengineProvider_ResolveAKSK_Bound(t *testing.T) {
	p := &VolcengineProvider{cfg: &Config{AccessKey: "AK", SecretKey: "SK"}}
	ak, sk, err := p.resolveAKSK()
	if err != nil || ak != "AK" || sk != "SK" {
		t.Errorf("resolveAKSK bound: ak=%q sk=%q err=%v", ak, sk, err)
	}
}

func TestVolcengineProvider_ResolveAKSK_FileFallback(t *testing.T) {
	// No bound keys; write a legacy cred file and point at it.
	dir := t.TempDir()
	credFile := filepath.Join(dir, "volcengine_apikey.json")
	os.WriteFile(credFile, []byte(`{"api_key":"ark","access_key":"AKf","secret_key":"SKf"}`), 0o600)
	p := &VolcengineProvider{cfg: &Config{VolcengineCredFile: credFile}}
	ak, sk, err := p.resolveAKSK()
	if err != nil || ak != "AKf" || sk != "SKf" {
		t.Errorf("resolveAKSK file: ak=%q sk=%q err=%v", ak, sk, err)
	}
}

func TestVolcengineProvider_ResolveAKSK_FileMissing(t *testing.T) {
	p := &VolcengineProvider{cfg: &Config{VolcengineCredFile: filepath.Join(t.TempDir(), "nope.json")}}
	if _, _, err := p.resolveAKSK(); err == nil {
		t.Error("resolveAKSK missing file: want error, got nil")
	}
}

func TestVolcengineProvider_ResolveAKSK_EmptyFileField(t *testing.T) {
	p := &VolcengineProvider{cfg: &Config{}} // no keys, no file path
	if _, _, err := p.resolveAKSK(); err == nil {
		t.Error("resolveAKSK no keys/file: want error, got nil")
	}
}

// keep time referenced (aqp parser uses time.Now via AsOf in other tests).
var _ = time.Now
