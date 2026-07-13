package provider

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// usage_test.go covers each provider's Usage() display method directly (the
// display logic moved from main in Phase 3). The main-package usage_display
// tests exercise the same paths via buildOne, but `go test ./provider` needs
// direct calls to count provider-package coverage.

// errAuth is an Authenticator whose Inject always errors (not-logged-in path).
type errAuth struct{}

func (errAuth) Inject(*http.Request) error { return errors.New("not logged in") }
func (errAuth) Refresh() error             { return nil }

// --- aqp ---

func TestAqpUsage_NotLoggedIn(t *testing.T) {
	p := &AqpProvider{cfg: &Config{}} // AqpAccount nil
	out := captureStdoutProvider(func() { _, _ = p.Usage() })
	if !contains(out, "Not logged in") || !contains(out, "aqp") {
		t.Errorf("aqp usage not-logged-in missing marker:\n%s", out)
	}
}

func TestAqpUsage_AccountError(t *testing.T) {
	p := &AqpProvider{cfg: &Config{AqpAccount: func() (string, string, string, error) {
		return "", "", "", errors.New("bad store")
	}}}
	out := captureStdoutProvider(func() { _, _ = p.Usage() })
	if !contains(out, "Error:") {
		t.Errorf("aqp usage account-error missing 'Error:':\n%s", out)
	}
}

func TestAqpUsage_Parsed(t *testing.T) {
	p := &AqpProvider{cfg: &Config{
		AqpAccount: func() (string, string, string, error) {
			return "alice@example.com", "proj-1", "/path/aqp_oauth_auth.json", nil
		},
		AqpMonthlyUsage: func() (*MonthlyProjectUsage, error) {
			return &MonthlyProjectUsage{SelectedYear: 2026, SelectedMonth: 7,
				TotalAmount: 250, Usage: 141.93, Balance: 108.07, Plan: "cqp"}, nil
		},
	}}
	out := captureStdoutProvider(func() { _, _ = p.Usage() })
	for _, want := range []string{"aqp", "alice@example.com", "proj-1", "57% used", "$141.93", "cqp", "2026-07", "/path/aqp_oauth_auth.json"} {
		if !contains(out, want) {
			t.Errorf("aqp usage parsed missing %q:\n%s", want, out)
		}
	}
}

func TestAqpUsage_MonthlyError(t *testing.T) {
	p := &AqpProvider{cfg: &Config{
		AqpAccount:      func() (string, string, string, error) { return "a@b.com", "p", "/s", nil },
		AqpMonthlyUsage: func() (*MonthlyProjectUsage, error) { return nil, errors.New("no project_id") },
	}}
	out := captureStdoutProvider(func() { _, _ = p.Usage() })
	if !contains(out, "unavailable") || !contains(out, "no project_id") {
		t.Errorf("aqp usage monthly-error missing marker:\n%s", out)
	}
}

// --- codex ---

const codexUsageBody = `{"email":"a@b.com","plan_type":"pro",` +
	`"credits":{"has_credits":true,"unlimited":false,"balance":"$10"},` +
	`"rate_limit":{"allowed":true,"limit_reached":false,` +
	`"primary_window":{"used_percent":30,"limit_window_seconds":18000,"reset_after_seconds":12000},` +
	`"secondary_window":{"used_percent":60,"limit_window_seconds":604800,"reset_after_seconds":300000}},` +
	`"spend_control":{"reached":false,"individual_limit":{"used":"5","limit":"20","remaining":"15","used_percent":25,"reset_after_seconds":2500000}}}`

func TestCodexUsage_Parsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/wham/usage" {
			t.Errorf("codex path=%q want /wham/usage", r.URL.Path)
		}
		w.Write([]byte(codexUsageBody))
	}))
	defer srv.Close()
	p := &CodexProvider{cfg: &Config{OpenAIBaseURL: srv.URL + "/codex", Auth: fakeAuth{key: "k"}}}
	out := captureStdoutProvider(func() { _, _ = p.Usage() })
	for _, want := range []string{"codex", "a@b.com", "pro", "has credits", "Rate Limit:", "25% used", "5 / 20 credits"} {
		if !contains(out, want) {
			t.Errorf("codex usage missing %q:\n%s", want, out)
		}
	}
}

func TestCodexUsage_NotLoggedIn(t *testing.T) {
	p := &CodexProvider{cfg: &Config{OpenAIBaseURL: "http://x.invalid/codex", Auth: errAuth{}}}
	out := captureStdoutProvider(func() { _, _ = p.Usage() })
	if !contains(out, "Not logged in") {
		t.Errorf("codex usage not-logged-in missing marker:\n%s", out)
	}
}

func TestCodexUsage_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`boom`))
	}))
	defer srv.Close()
	p := &CodexProvider{cfg: &Config{OpenAIBaseURL: srv.URL + "/codex", Auth: fakeAuth{key: "k"}}}
	out := captureStdoutProvider(func() { _, _ = p.Usage() })
	if !contains(out, "HTTP 500") {
		t.Errorf("codex usage HTTP error missing 'HTTP 500':\n%s", out)
	}
}

// --- zhipu ---

func TestZhipuUsage_Quota(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"success":true,"data":{"level":"GLM Coding Plan","limits":[` +
			`{"type":"TOKENS_LIMIT","unit":3,"percentage":40,"nextResetTime":1750000000000,"usage":100000,"currentValue":40000,"remaining":60000}]}}`))
	}))
	defer srv.Close()
	p := &ZhipuProvider{cfg: &Config{UsageURL: srv.URL, Auth: fakeAuth{key: "k"}}, providerName: "zhipu"}
	out := captureStdoutProvider(func() { _, _ = p.Usage() })
	for _, want := range []string{"zhipu", "GLM Coding Plan", "5h tokens", "40% used"} {
		if !contains(out, want) {
			t.Errorf("zhipu usage missing %q:\n%s", want, out)
		}
	}
}

func TestZhipuUsage_FallbackModelList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"object":"list","data":[{"id":"gpt-4","owned_by":"x"}]}`))
	}))
	defer srv.Close()
	p := &ZhipuProvider{cfg: &Config{UsageURL: srv.URL, Auth: fakeAuth{key: "k"}}, providerName: "zhipu"}
	out := captureStdoutProvider(func() { _, _ = p.Usage() })
	if !contains(out, "1 models available") || !contains(out, "gpt-4") {
		t.Errorf("zhipu usage model-list fallback missing marker:\n%s", out)
	}
}

func TestZhipuUsage_FallbackRawJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok","count":42}`))
	}))
	defer srv.Close()
	p := &ZhipuProvider{cfg: &Config{UsageURL: srv.URL, Auth: fakeAuth{key: "k"}}, providerName: "zhipu"}
	out := captureStdoutProvider(func() { _, _ = p.Usage() })
	if !contains(out, "count") || !contains(out, "42") {
		t.Errorf("zhipu usage raw-JSON fallback missing marker:\n%s", out)
	}
}

func TestZhipuUsage_NotLoggedIn(t *testing.T) {
	p := &ZhipuProvider{cfg: &Config{UsageURL: "http://x.invalid", Auth: errAuth{}}, providerName: "zhipu"}
	out := captureStdoutProvider(func() { _, _ = p.Usage() })
	if !contains(out, "Not logged in") {
		t.Errorf("zhipu usage not-logged-in missing marker:\n%s", out)
	}
}

// --- deepseek ---

func TestDeepseekUsage_Balance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"is_available":true,"balance_infos":[` +
			`{"currency":"CNY","total_balance":"10.50","granted_balance":"8.00","topped_up_balance":"2.50"}]}`))
	}))
	defer srv.Close()
	p := &DeepSeekProvider{cfg: &Config{UsageURL: srv.URL, Auth: fakeAuth{key: "k"}, ProviderName: "deepseek"}}
	out := captureStdoutProvider(func() { _, _ = p.Usage() })
	for _, want := range []string{"deepseek", "Available:", "yes", "CNY", "10.50", "granted"} {
		if !contains(out, want) {
			t.Errorf("deepseek usage missing %q:\n%s", want, out)
		}
	}
}

func TestDeepseekUsage_Unavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"is_available":false,"balance_infos":[]}`))
	}))
	defer srv.Close()
	p := &DeepSeekProvider{cfg: &Config{UsageURL: srv.URL, Auth: fakeAuth{key: "k"}, ProviderName: "deepseek"}}
	out := captureStdoutProvider(func() { _, _ = p.Usage() })
	if !contains(out, "insufficient balance") {
		t.Errorf("deepseek usage unavailable missing marker:\n%s", out)
	}
}

func TestDeepseekUsage_NotLoggedIn(t *testing.T) {
	p := &DeepSeekProvider{cfg: &Config{UsageURL: "http://x.invalid", Auth: errAuth{}, ProviderName: "deepseek"}}
	out := captureStdoutProvider(func() { _, _ = p.Usage() })
	if !contains(out, "Not logged in") {
		t.Errorf("deepseek usage not-logged-in missing marker:\n%s", out)
	}
}

// --- volcengine ---

func TestVolcengineUsage_NoCredsFallsBack(t *testing.T) {
	p := &VolcengineProvider{cfg: &Config{ProviderName: "volcengine", Models: []string{"doubao", "glm-5.2"}}}
	out := captureStdoutProvider(func() { _, _ = p.Usage() })
	for _, want := range []string{"volcengine", "GetAFPUsage", "AK/SK", "2 models", "doubao"} {
		if !contains(out, want) {
			t.Errorf("volcengine usage no-creds missing %q:\n%s", want, out)
		}
	}
}

func TestVolcengineUsage_GetAFPFailsFallsBack(t *testing.T) {
	deadSrv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := deadSrv.Listener.Addr().String()
	deadSrv.Close()
	t.Setenv("HTTPS_PROXY", "http://"+addr)
	t.Setenv("HTTP_PROXY", "http://"+addr)
	p := &VolcengineProvider{cfg: &Config{ProviderName: "volcengine", AccessKey: "AK", SecretKey: "SK", Models: []string{"doubao"}}}
	out := captureStdoutProvider(func() { _, _ = p.Usage() })
	if !contains(out, "GetAFPUsage failed") || !contains(out, "1 models") {
		t.Errorf("volcengine usage getafp-fails missing marker:\n%s", out)
	}
}
