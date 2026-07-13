package provider

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
		w.Write([]byte(body))
	}))
	defer srv.Close()
	p := &CodexProvider{cfg: &Config{OpenAIBaseURL: srv.URL + "/codex", Auth: fakeAuth{key: "k"}}}
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

func TestCodexProvider_Quota_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
	}))
	defer srv.Close()
	p := &CodexProvider{cfg: &Config{OpenAIBaseURL: srv.URL + "/codex", Auth: fakeAuth{key: "k"}}}
	s, err := p.Quota()
	if err != nil {
		t.Fatal(err)
	}
	if s.Billing != BillingUnknown || s.Err != "HTTP 502" {
		t.Errorf("got %+v want BillingUnknown/HTTP 502", s)
	}
}

// --- zhipu ---

func TestZhipuProvider_Quota_Parsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"success":true,"data":{"level":"GLM Coding Plan","limits":[` +
			`{"type":"TOKENS_LIMIT","unit":6,"percentage":70,"nextResetTime":1750000000000,"usage":200000,"currentValue":140000,"remaining":60000}]}}`))
	}))
	defer srv.Close()
	p := &ZhipuProvider{cfg: &Config{UsageURL: srv.URL, Auth: fakeAuth{key: "k"}}}
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
	p := &ZhipuProvider{cfg: &Config{UsageURL: srv.URL, Auth: fakeAuth{key: "k"}}}
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
	p := &ZhipuProvider{cfg: &Config{UsageURL: srv.URL, Auth: fakeAuth{key: "k"}}}
	s, _ := p.Quota()
	if s.Billing != BillingUnknown || s.Err != "HTTP 500" {
		t.Errorf("got %+v want BillingUnknown/HTTP 500", s)
	}
}

// --- deepseek ---

func TestDeepSeekProvider_Quota_Parsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"is_available":true,"balance_infos":[` +
			`{"currency":"CNY","total_balance":"10.50","granted_balance":"8.00","topped_up_balance":"2.50"}]}`))
	}))
	defer srv.Close()
	p := &DeepSeekProvider{cfg: &Config{UsageURL: srv.URL, Auth: fakeAuth{key: "k"}}}
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
}

func TestDeepSeekProvider_Quota_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()
	p := &DeepSeekProvider{cfg: &Config{UsageURL: srv.URL, Auth: fakeAuth{key: "k"}}}
	s, _ := p.Quota()
	if s.Billing != BillingUnknown || s.Err != "HTTP 503" {
		t.Errorf("got %+v want BillingUnknown/HTTP 503", s)
	}
}

// --- aqp ---

func TestAqpProvider_Quota_NilFetcher(t *testing.T) {
	p := &AqpProvider{cfg: &Config{}} // AqpMonthlyUsage unset
	s, err := p.Quota()
	if err != nil {
		t.Fatal(err)
	}
	if s.Billing != BillingUnknown || s.Err == "" {
		t.Errorf("got %+v want BillingUnknown with Err", s)
	}
}

func TestAqpProvider_Quota_FetchError(t *testing.T) {
	p := &AqpProvider{cfg: &Config{AqpMonthlyUsage: func() (*MonthlyProjectUsage, error) {
		return nil, errFoo
	}}}
	s, _ := p.Quota()
	if s.Billing != BillingUnknown || s.Err != errFoo.Error() {
		t.Errorf("got %+v want BillingUnknown/"+errFoo.Error(), s)
	}
}

func TestAqpProvider_Quota_Parsed(t *testing.T) {
	p := &AqpProvider{cfg: &Config{AqpMonthlyUsage: func() (*MonthlyProjectUsage, error) {
		return &MonthlyProjectUsage{SelectedYear: 2026, SelectedMonth: 7,
			TotalAmount: 100, Usage: 30, Balance: 70, Plan: "CQP"}, nil
	}}}
	s, err := p.Quota()
	if err != nil {
		t.Fatal(err)
	}
	if s.Billing != BillingPlan || s.RemainingPct != 0.7 {
		t.Errorf("got %+v want Plan/0.7", s)
	}
}

var errFoo = &fooErr{}

type fooErr struct{}

func (e *fooErr) Error() string { return "fetch failed" }

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
