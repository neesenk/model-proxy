package provider

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	// No store file at OAuthAuthFile -> LoadAqpAccount returns nil -> "Not logged in".
	p := &AqpProvider{cfg: &Config{OAuthAuthFile: filepath.Join(t.TempDir(), "nope.json")}}
	out := captureStdoutProvider(func() { _ = p.Usage() })
	if !contains(out, "Not logged in") || !contains(out, "aqp") {
		t.Errorf("aqp usage not-logged-in missing marker:\n%s", out)
	}
}

func TestAqpUsage_AccountError(t *testing.T) {
	// Store file exists but isn't valid JSON -> LoadAqpAccount errors -> "Error:".
	store := filepath.Join(t.TempDir(), "aqp_oauth_auth.json")
	os.WriteFile(store, []byte(`not-json`), 0o600)
	p := &AqpProvider{cfg: &Config{OAuthAuthFile: store}}
	out := captureStdoutProvider(func() { _ = p.Usage() })
	if !contains(out, "Error:") {
		t.Errorf("aqp usage account-error missing 'Error:':\n%s", out)
	}
}

func TestAqpUsage_Parsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"retcode":0,"data":{"project_id":"proj-1","selected_year":2026,"selected_month":7,"total_amount":250,"usage":141.93,"balance":108.07,"plan":"cqp"}}`))
	}))
	defer srv.Close()
	store := filepath.Join(t.TempDir(), "aqp_oauth_auth.json")
	writeAqpStore(t, store, "proj-1")
	p := &AqpProvider{cfg: &Config{OAuthAuthFile: store, AqpBaseURL: srv.URL}}
	out := captureStdoutProvider(func() { _ = p.Usage() })
	for _, want := range []string{"aqp", "a@b.com", "proj-1", "56.8% used", "$141.93", "cqp", "2026-07"} {
		if !contains(out, want) {
			t.Errorf("aqp usage parsed missing %q:\n%s", want, out)
		}
	}
}

func TestAqpUsage_MonthlyError(t *testing.T) {
	// Store present but project_id empty -> fetchMonthlyUsage returns
	// "no project_id" (no network call) -> "(unavailable: ...)".
	store := filepath.Join(t.TempDir(), "aqp_oauth_auth.json")
	writeAqpStore(t, store, "")
	p := &AqpProvider{cfg: &Config{OAuthAuthFile: store}}
	out := captureStdoutProvider(func() { _ = p.Usage() })
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
		if got := r.Header.Get("Authorization"); got != "Bearer quota-token" {
			t.Errorf("Authorization=%q want %q", got, "Bearer quota-token")
		}
		if got := r.Header.Get("originator"); got != "codex_cli_rs" {
			t.Errorf("originator=%q want %q", got, "codex_cli_rs")
		}
		if got := r.Header.Get("ChatGPT-Account-Id"); got != "acct-quota" {
			t.Errorf("ChatGPT-Account-Id=%q want %q", got, "acct-quota")
		}
		w.Write([]byte(codexUsageBody))
	}))
	defer srv.Close()
	p := &CodexProvider{cfg: &Config{OpenAIBaseURL: srv.URL + "/codex"}, auth: codexRequestAuth{}}
	out := captureStdoutProvider(func() { _ = p.Usage() })
	for _, want := range []string{"codex", "a@b.com", "pro", "has credits", "Rate Limit:", "25.0% used", "5 / 20 credits"} {
		if !contains(out, want) {
			t.Errorf("codex usage missing %q:\n%s", want, out)
		}
	}
}

func TestCodexUsage_NotLoggedIn(t *testing.T) {
	p := &CodexProvider{cfg: &Config{OpenAIBaseURL: "http://x.invalid/codex"}, auth: errAuth{}}
	out := captureStdoutProvider(func() { _ = p.Usage() })
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
	p := &CodexProvider{cfg: &Config{OpenAIBaseURL: srv.URL + "/codex"}, auth: fakeAuth{key: "k"}}
	out := captureStdoutProvider(func() { _ = p.Usage() })
	if !contains(out, "Error: HTTP 500: boom") {
		t.Errorf("codex usage HTTP error missing exact marker:\n%s", out)
	}
}

// --- zhipu ---

func TestZhipuUsage_Quota(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"success":true,"data":{"level":"GLM Coding Plan","limits":[` +
			`{"type":"TOKENS_LIMIT","unit":3,"percentage":40,"nextResetTime":1750000000000,"usage":100000,"currentValue":40000,"remaining":60000}]}}`))
	}))
	defer srv.Close()
	p := &ZhipuProvider{ApiKeyBase: NewApiKeyBaseWithKey("zhipu", "k"), cfg: &Config{UsageURL: srv.URL}, providerName: "zhipu"}
	out := captureStdoutProvider(func() { _ = p.Usage() })
	for _, want := range []string{"zhipu", "GLM Coding Plan", "5h tokens", "40.0% used"} {
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
	p := &ZhipuProvider{ApiKeyBase: NewApiKeyBaseWithKey("zhipu", "k"), cfg: &Config{UsageURL: srv.URL}, providerName: "zhipu"}
	out := captureStdoutProvider(func() { _ = p.Usage() })
	if !contains(out, "1 models available") || !contains(out, "gpt-4") {
		t.Errorf("zhipu usage model-list fallback missing marker:\n%s", out)
	}
}

// The model-list fallback shows the owner in the second column — the model ID
// must not be printed twice. Regression: the second column used to repeat m.ID.
func TestZhipuUsage_FallbackModelListShowsOwner(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"object":"list","data":[{"id":"glm-5.2","owned_by":"zhipuai"}]}`))
	}))
	defer srv.Close()
	p := &ZhipuProvider{ApiKeyBase: NewApiKeyBaseWithKey("zhipu", "k"), cfg: &Config{UsageURL: srv.URL}, providerName: "zhipu"}
	out := captureStdoutProvider(func() { _ = p.Usage() })
	if !contains(out, "zhipuai") {
		t.Errorf("model-list fallback missing owner %q:\n%s", "zhipuai", out)
	}
	if strings.Count(out, "glm-5.2") != 1 {
		t.Errorf("model id printed %d times, want exactly 1:\n%s", strings.Count(out, "glm-5.2"), out)
	}
}

func TestZhipuUsage_FallbackRawJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok","count":42}`))
	}))
	defer srv.Close()
	p := &ZhipuProvider{ApiKeyBase: NewApiKeyBaseWithKey("zhipu", "k"), cfg: &Config{UsageURL: srv.URL}, providerName: "zhipu"}
	out := captureStdoutProvider(func() { _ = p.Usage() })
	if !contains(out, "count") || !contains(out, "42") {
		t.Errorf("zhipu usage raw-JSON fallback missing marker:\n%s", out)
	}
}

func TestZhipuUsage_NotLoggedIn(t *testing.T) {
	p := &ZhipuProvider{ApiKeyBase: &ApiKeyBase{}, cfg: &Config{UsageURL: "http://x.invalid"}, providerName: "zhipu"}
	out := captureStdoutProvider(func() { _ = p.Usage() })
	if !contains(out, "Not logged in") {
		t.Errorf("zhipu usage not-logged-in missing marker:\n%s", out)
	}
}

func TestZhipuUsage_HTTPAndParseErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		code int
		want string
	}{
		{name: "http", code: http.StatusBadGateway, body: "quota unavailable", want: "Error: HTTP 502: quota unavailable"},
		{name: "parse", code: http.StatusOK, body: "not-json", want: "Error: parse usage response:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.code)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			p := &ZhipuProvider{ApiKeyBase: NewApiKeyBaseWithKey("zhipu", "k"), cfg: &Config{UsageURL: srv.URL}, providerName: "zhipu"}
			out := captureStdoutProvider(func() { _ = p.Usage() })
			if !contains(out, tc.want) {
				t.Errorf("zhipu usage %s missing %q:\n%s", tc.name, tc.want, out)
			}
		})
	}
}

// --- zcode ---

func TestZcodeUsage_Quota(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-api-key"); got != "k" {
			t.Errorf("zcode usage GET x-api-key = %q, want k (ZCode sends both auth headers)", got)
		}
		w.Write([]byte(`{"success":true,"data":{"level":"GLM Coding Plan","limits":[` +
			`{"type":"TOKENS_LIMIT","unit":3,"percentage":40,"nextResetTime":1750000000000,"usage":100000,"currentValue":40000,"remaining":60000}]}}`))
	}))
	defer srv.Close()
	p := &ZCodeProvider{ApiKeyBase: NewApiKeyBaseWithKey("zcode", "k"), cfg: &Config{UsageURL: srv.URL}, providerName: "zcode"}
	out := captureStdoutProvider(func() { _ = p.Usage() })
	for _, want := range []string{"zcode", "GLM Coding Plan", "5h tokens", "40.0% used"} {
		if !contains(out, want) {
			t.Errorf("zcode usage missing %q:\n%s", want, out)
		}
	}
}

func TestZcodeUsage_NotLoggedIn(t *testing.T) {
	p := &ZCodeProvider{ApiKeyBase: &ApiKeyBase{}, cfg: &Config{UsageURL: "http://x.invalid"}, providerName: "zcode"}
	out := captureStdoutProvider(func() { _ = p.Usage() })
	if !contains(out, "Not logged in") {
		t.Errorf("zcode usage not-logged-in missing marker:\n%s", out)
	}
}

// --- deepseek ---

func TestDeepseekUsage_Balance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"is_available":true,"balance_infos":[` +
			`{"currency":"CNY","total_balance":"10.50","granted_balance":"8.00","topped_up_balance":"2.50"}]}`))
	}))
	defer srv.Close()
	p := &DeepSeekProvider{ApiKeyBase: NewApiKeyBaseWithKey("deepseek", "k"), cfg: &Config{UsageURL: srv.URL, ProviderName: "deepseek"}}
	out := captureStdoutProvider(func() { _ = p.Usage() })
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
	p := &DeepSeekProvider{ApiKeyBase: NewApiKeyBaseWithKey("deepseek", "k"), cfg: &Config{UsageURL: srv.URL, ProviderName: "deepseek"}}
	out := captureStdoutProvider(func() { _ = p.Usage() })
	if !contains(out, "insufficient balance") {
		t.Errorf("deepseek usage unavailable missing marker:\n%s", out)
	}
}

func TestDeepseekUsage_NotLoggedIn(t *testing.T) {
	p := &DeepSeekProvider{ApiKeyBase: &ApiKeyBase{}, cfg: &Config{UsageURL: "http://x.invalid", ProviderName: "deepseek"}}
	out := captureStdoutProvider(func() { _ = p.Usage() })
	if !contains(out, "Not logged in") {
		t.Errorf("deepseek usage not-logged-in missing marker:\n%s", out)
	}
}

func TestDeepseekUsage_HTTPAndParseErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		code int
		want string
	}{
		{name: "http", code: http.StatusServiceUnavailable, body: "maintenance", want: "Error: HTTP 503: maintenance"},
		{name: "parse", code: http.StatusOK, body: "not-json", want: "Error: parse usage response:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.code)
				w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			p := &DeepSeekProvider{ApiKeyBase: NewApiKeyBaseWithKey("deepseek", "k"), cfg: &Config{UsageURL: srv.URL, ProviderName: "deepseek"}}
			out := captureStdoutProvider(func() { _ = p.Usage() })
			if !contains(out, tc.want) {
				t.Errorf("deepseek usage %s missing %q:\n%s", tc.name, tc.want, out)
			}
		})
	}
}

// --- volcengine ---

func TestVolcengineUsage_NoCredsFallsBack(t *testing.T) {
	p := &VolcengineProvider{cfg: &Config{ProviderName: "volcengine", Models: []string{"doubao", "glm-5.2"}}}
	out := captureStdoutProvider(func() { _ = p.Usage() })
	for _, want := range []string{"volcengine", "GetAFPUsage", "AK/SK", "2 models", "doubao"} {
		if !contains(out, want) {
			t.Errorf("volcengine usage no-creds missing %q:\n%s", want, out)
		}
	}
}

func TestVolcengineUsage_GetAFPFailsFallsBack(t *testing.T) {
	// Inject the failure via the OpenAPI base (an HTTP 500 mock) — not via
	// HTTP_PROXY env: http.ProxyFromEnvironment caches the proxy config
	// process-wide, so env-based injection silently degrades into a REAL call
	// to the production endpoint.
	orig := volcengineOpenAPIBase
	defer func() { volcengineOpenAPIBase = orig }()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer bad.Close()
	volcengineOpenAPIBase = bad.URL
	p := &VolcengineProvider{cfg: &Config{ProviderName: "volcengine", AccessKey: "AK", SecretKey: "SK", Models: []string{"doubao"}}}
	out := captureStdoutProvider(func() { _ = p.Usage() })
	if !contains(out, "GetAFPUsage failed") || !contains(out, "1 models") {
		t.Errorf("volcengine usage getafp-fails missing marker:\n%s", out)
	}
}
