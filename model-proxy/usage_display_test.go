package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"model-proxy/provider"
)

// mustMarshalT json-marshals v (test helper, panics on error - never happens for maps).
func mustMarshalT(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// usage_display_test.go covers the usage-display + quota-fetch/print functions in
// main.go that were previously 0% covered. Network-mocked functions use
// httptest.NewServer; the print-only functions are exercised with constructed
// inputs. Color is forced off during stdout capture so assertions are deterministic.

// --- helpers ---

// captureStdout runs fn with os.Stdout redirected to a pipe and returns what was
// written. colorEnabled is a package var decided at init from the real stdout
// (tty → color on); force it off during capture so output has no ANSI codes, and
// restore it afterward so other tests are unaffected. Tests in this package run
// sequentially (none call t.Parallel), so mutating colorEnabled is safe.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	oldOut := os.Stdout
	oldColor := colorEnabled
	colorEnabled = false
	syncColorEnabled() // mirror into provider.ColorEnabled (display helpers)
	defer func() {
		os.Stdout = oldOut
		colorEnabled = oldColor
		syncColorEnabled()
	}()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	fn()
	w.Close()
	return <-done
}

// useTempHome redirects homeDir() (which reads $HOME) at a fresh temp dir so
// authFilePath lands cred files in an isolated location. Restored automatically
// by t.Setenv.
func useTempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	return dir
}

// writeCred writes data to <home>/.model-proxy/<name>_<suffix>.json.
func writeCred(t *testing.T, home, name, suffix string, data []byte) {
	t.Helper()
	dir := filepath.Join(home, ".model-proxy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name+"_"+suffix+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// b64url is base64 RawURLEncoding (no padding), as used for JWT parts.
func b64url(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// fakeJWT builds an unsigned JWT with the given claims. jwtExpiry /
// accountIDFromTokens decode the payload without verifying the signature, so a
// dummy signature is fine.
func fakeJWT(claims map[string]any) string {
	header := b64url([]byte(`{"alg":"RS256","typ":"JWT"}`))
	payload := b64url(mustMarshalT(claims))
	return header + "." + payload + ".fakesig"
}

// codexAccessToken returns a JWT whose exp is far in the future (so
// CodexOAuthProvider.token() treats it as valid and skips refresh).
func codexAccessToken() string {
	return fakeJWT(map[string]any{"exp": time.Now().Add(24 * time.Hour).Unix()})
}

// codexIDToken returns a JWT carrying the chatgpt_account_id claim.
func codexIDToken(acct string) string {
	return fakeJWT(map[string]any{
		"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": acct},
	})
}

// writeCodexAuthFile writes a codex_oauth_auth.json (at the standard cred path
// under home) whose access_token is a non-expired JWT, so token() returns it
// without a refresh network call. account_id is left empty so
// accountIDFromTokens parses the id_token JWT (exercises that path). Named
// distinctly from auth_codex_test.go's writeCodexAuth (different signature).
func writeCodexAuthFile(t *testing.T, home, acct string) {
	af := map[string]any{
		"auth_mode": "oauth",
		"tokens": map[string]any{
			"access_token":  codexAccessToken(),
			"refresh_token": "rt-dummy",
			"id_token":      codexIDToken(acct),
		},
		"last_refresh": time.Now().UTC().Format(time.RFC3339Nano),
	}
	writeCred(t, home, "codex", "oauth_auth", mustMarshalT(af))
}

// deadProxyURL returns the address of a server that has been closed — connecting
// to it is refused immediately. Used to make volcengineGet (which hardcodes the
// real Volcengine host) fail fast without a 30s timeout, exercising
// getAFPUsage's error path.
func deadProxyURL(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.Listener.Addr().String()
	srv.Close()
	return "http://" + addr
}

// --- showDeepseekUsage ---

// TestShowDeepseekUsage_BalanceParsed: mock /user/balance returns two currency
// balances; the display prints Available + per-currency lines.
func TestShowDeepseekUsage_BalanceParsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/balance" {
			t.Errorf("deepseek path=%q want /user/balance", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer dk" {
			t.Errorf("deepseek Authorization=%q want Bearer dk", got)
		}
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"is_available":true,"balance_infos":[` +
			`{"currency":"CNY","total_balance":"10.50","granted_balance":"8.00","topped_up_balance":"2.50"},` +
			`{"currency":"USD","total_balance":"5.00","granted_balance":"4.00","topped_up_balance":"1.00"}]}`))
	}))
	defer srv.Close()

	home := useTempHome(t)
	writeCred(t, home, "deepseek", "apikey", mustMarshalT(map[string]string{"api_key": "dk"}))
	cfg := &Config{Providers: map[string]Provider{
		"deepseek": {Provider: "deepseek", UsageURL: srv.URL + "/user/balance"},
	}}
	out := captureStdout(t, func() {
		showDeepseekUsage(cfg, "deepseek", cfg.Providers["deepseek"], nil)
	})
	for _, want := range []string{"deepseek", "Available:", "yes", "CNY", "10.50", "USD", "5.00", "granted", "topped-up"} {
		if !strings.Contains(out, want) {
			t.Errorf("deepseek usage missing %q:\n%s", want, out)
		}
	}
}

// TestShowDeepseekUsage_Unavailable: is_available=false → "no (insufficient balance)".
func TestShowDeepseekUsage_Unavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"is_available":false,"balance_infos":[]}`))
	}))
	defer srv.Close()
	home := useTempHome(t)
	writeCred(t, home, "deepseek", "apikey", mustMarshalT(map[string]string{"api_key": "dk"}))
	cfg := &Config{Providers: map[string]Provider{
		"deepseek": {Provider: "deepseek", UsageURL: srv.URL},
	}}
	out := captureStdout(t, func() {
		showDeepseekUsage(cfg, "deepseek", cfg.Providers["deepseek"], nil)
	})
	if !strings.Contains(out, "insufficient balance") {
		t.Errorf("missing 'insufficient balance':\n%s", out)
	}
}

// TestShowDeepseekUsage_NotLoggedIn: no cred file → "Not logged in." (auth.Inject fails).
func TestShowDeepseekUsage_NotLoggedIn(t *testing.T) {
	useTempHome(t) // no cred file
	cfg := &Config{Providers: map[string]Provider{
		"deepseek": {Provider: "deepseek", UsageURL: "https://x.invalid/user/balance"},
	}}
	out := captureStdout(t, func() {
		showDeepseekUsage(cfg, "deepseek", cfg.Providers["deepseek"], nil)
	})
	if !strings.Contains(out, "Not logged in") {
		t.Errorf("missing 'Not logged in':\n%s", out)
	}
}

// TestShowDeepseekUsage_HTTPError: mock returns 500 → "HTTP 500".
func TestShowDeepseekUsage_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`boom`))
	}))
	defer srv.Close()
	home := useTempHome(t)
	writeCred(t, home, "deepseek", "apikey", mustMarshalT(map[string]string{"api_key": "dk"}))
	cfg := &Config{Providers: map[string]Provider{
		"deepseek": {Provider: "deepseek", UsageURL: srv.URL},
	}}
	out := captureStdout(t, func() {
		showDeepseekUsage(cfg, "deepseek", cfg.Providers["deepseek"], nil)
	})
	if !strings.Contains(out, "HTTP 500") {
		t.Errorf("missing 'HTTP 500':\n%s", out)
	}
}

// TestShowDeepseekUsage_BadJSON: mock returns non-JSON → "parse usage response".
func TestShowDeepseekUsage_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`not-json`))
	}))
	defer srv.Close()
	home := useTempHome(t)
	writeCred(t, home, "deepseek", "apikey", mustMarshalT(map[string]string{"api_key": "dk"}))
	cfg := &Config{Providers: map[string]Provider{
		"deepseek": {Provider: "deepseek", UsageURL: srv.URL},
	}}
	out := captureStdout(t, func() {
		showDeepseekUsage(cfg, "deepseek", cfg.Providers["deepseek"], nil)
	})
	if !strings.Contains(out, "parse usage response") {
		t.Errorf("missing 'parse usage response':\n%s", out)
	}
}

// --- fetchDeepseekQuota ---

func TestFetchDeepseekQuota_BalanceParsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"is_available":true,"balance_infos":[` +
			`{"currency":"CNY","total_balance":"10.50","granted_balance":"8.00","topped_up_balance":"2.50"}]}`))
	}))
	defer srv.Close()
	home := useTempHome(t)
	writeCred(t, home, "deepseek", "apikey", mustMarshalT(map[string]string{"api_key": "dk"}))
	cfg := &Config{Providers: map[string]Provider{
		"deepseek": {Provider: "deepseek", UsageURL: srv.URL},
	}}
	s, err := fetchDeepseekQuota(cfg, "deepseek", cfg.Providers["deepseek"], nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Billing != provider.BillingPayG {
		t.Errorf("Billing=%v want PayG", s.Billing)
	}
	if s.RemainingPct != -1 {
		t.Errorf("RemainingPct=%v want -1 (payg, no window)", s.RemainingPct)
	}
	if len(s.Windows) != 1 || s.Windows[0].Total != 10.5 {
		t.Errorf("windows=%+v want 1 window total=10.5", s.Windows)
	}
}

func TestFetchDeepseekQuota_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
	}))
	defer srv.Close()
	home := useTempHome(t)
	writeCred(t, home, "deepseek", "apikey", mustMarshalT(map[string]string{"api_key": "dk"}))
	cfg := &Config{Providers: map[string]Provider{
		"deepseek": {Provider: "deepseek", UsageURL: srv.URL},
	}}
	s, err := fetchDeepseekQuota(cfg, "deepseek", cfg.Providers["deepseek"], nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Billing != provider.BillingUnknown || s.Err != "HTTP 503" {
		t.Errorf("got %+v want BillingUnknown/HTTP 503", s)
	}
}

func TestFetchDeepseekQuota_NotLoggedIn(t *testing.T) {
	useTempHome(t)
	cfg := &Config{Providers: map[string]Provider{
		"deepseek": {Provider: "deepseek", UsageURL: "https://x.invalid"},
	}}
	s, _ := fetchDeepseekQuota(cfg, "deepseek", cfg.Providers["deepseek"], nil)
	if s.Billing != provider.BillingUnknown {
		t.Errorf("got %+v want BillingUnknown", s)
	}
}

// --- showAqpUsage ---

// TestShowAqpUsage_NotLoggedIn: no cred file → "Not logged in.".
func TestShowAqpUsage_NotLoggedIn(t *testing.T) {
	useTempHome(t) // no cred file
	out := captureStdout(t, func() { showAqpUsage(&Config{}) })
	if !strings.Contains(out, "Not logged in") {
		t.Errorf("missing 'Not logged in':\n%s", out)
	}
	if !strings.Contains(out, "aqp") {
		t.Errorf("missing provider name:\n%s", out)
	}
}

// TestShowAqpUsage_BadStore: cred file exists but isn't valid JSON → loadAccount
// returns an error → "Error:" branch.
func TestShowAqpUsage_BadStore(t *testing.T) {
	home := useTempHome(t)
	writeCred(t, home, "aqp", "oauth_auth", []byte(`not-json`))
	out := captureStdout(t, func() { showAqpUsage(&Config{}) })
	if !strings.Contains(out, "Error:") {
		t.Errorf("missing 'Error:' for bad store:\n%s", out)
	}
}

// TestShowAqpUsage_MonthlyUsageError: account present but project_id empty →
// monthlyUsageAt rejects it ("no project_id in store") WITHOUT a network call.
// showAqpUsage prints account + Project ID + the "unavailable" line + store path.
// (The success branch needs the hardcoded aqpMonthlyUsage URL — can't redirect
// without product-code changes, so it stays uncovered.)
func TestShowAqpUsage_MonthlyUsageError(t *testing.T) {
	home := useTempHome(t)
	writeCred(t, home, "aqp", "oauth_auth", mustMarshalT(map[string]any{
		"email":              "alice@example.com",
		"sso_session_cookie": "SSO_C=abc",
		"project_id":         "", // triggers "no project_id" error in MonthlyUsage
	}))
	out := captureStdout(t, func() { showAqpUsage(&Config{}) })
	for _, want := range []string{"aqp", "alice@example.com", "unavailable", "no project_id"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

// --- fetchAqpQuota ---

func TestFetchAqpQuota_NotLoggedIn(t *testing.T) {
	useTempHome(t)
	s, err := fetchAqpQuota(&Config{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Billing != provider.BillingUnknown || s.Err == "" {
		t.Errorf("got %+v want BillingUnknown with Err", s)
	}
}

// TestFetchAqpQuota_NoProjectID: account present but no project_id → MonthlyUsage
// errors without a network call → BillingUnknown.
func TestFetchAqpQuota_NoProjectID(t *testing.T) {
	home := useTempHome(t)
	writeCred(t, home, "aqp", "oauth_auth", mustMarshalT(map[string]any{
		"email":              "alice@example.com",
		"sso_session_cookie": "SSO_C=abc",
		"project_id":         "",
	}))
	s, err := fetchAqpQuota(&Config{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Billing != provider.BillingUnknown || !strings.Contains(s.Err, "project_id") {
		t.Errorf("got %+v want BillingUnknown with project_id Err", s)
	}
}

// --- showCodexUsage ---

const codexUsageBody = `{"email":"a@b.com","plan_type":"pro",` +
	`"credits":{"has_credits":true,"unlimited":false,"balance":"$10"},` +
	`"rate_limit":{"allowed":true,"limit_reached":false,` +
	`"primary_window":{"used_percent":30,"limit_window_seconds":18000,"reset_after_seconds":12000},` +
	`"secondary_window":{"used_percent":60,"limit_window_seconds":604800,"reset_after_seconds":300000}},` +
	`"spend_control":{"reached":false,"individual_limit":{"used":"5","limit":"20","remaining":"15","used_percent":25,"reset_after_seconds":2500000}}}`

// TestShowCodexUsage_UsageParsed: mock /wham/usage returns the full codex usage
// envelope; the display prints account/plan/credits/rate-limit/spend lines.
func TestShowCodexUsage_UsageParsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/wham/usage" {
			t.Errorf("codex path=%q want /wham/usage", r.URL.Path)
		}
		if got := r.Header.Get("originator"); got != "codex_cli_rs" {
			t.Errorf("codex originator=%q want codex_cli_rs", got)
		}
		if got, want := r.Header.Get("Authorization"), "Bearer "+codexAccessToken(); got != want {
			t.Errorf("codex Authorization=%q want %q", got, want)
		}
		if got := r.Header.Get("ChatGPT-Account-Id"); got != "acct-test" {
			t.Errorf("codex ChatGPT-Account-Id=%q want acct-test (parsed from id_token JWT)", got)
		}
		w.Write([]byte(codexUsageBody))
	}))
	defer srv.Close()

	home := useTempHome(t)
	writeCodexAuthFile(t, home, "acct-test")
	cfg := &Config{Providers: map[string]Provider{
		"codex": {Provider: "codex", OpenAIBaseURL: srv.URL + "/codex"},
	}}
	out := captureStdout(t, func() { showCodexUsage(cfg, cfg.Providers["codex"]) })
	for _, want := range []string{"codex", "a@b.com", "pro", "Credits:", "has credits",
		"Rate Limit:", "allowed", "primary:", "weekly:", "Usage:"} {
		if !strings.Contains(out, want) {
			t.Errorf("codex usage missing %q:\n%s", want, out)
		}
	}
	// New format: "[<BAR>] <PCT>% used · <USED> / <TOTAL> credits, resets <DUR>"
	// (fixture: used=5, limit=20, used_percent=25, reset_after=2500000s -> 28d22h).
	// <PCT> renders with 1-decimal precision (25.0%).
	if !strings.Contains(out, "25.0% used · 5 / 20 credits, resets 28d22h") {
		t.Errorf("codex usage line format wrong:\n%s", out)
	}
}

// TestShowCodexUsage_NotLoggedIn: no cred file → token() returns error → "Not logged in.".
func TestShowCodexUsage_NotLoggedIn(t *testing.T) {
	useTempHome(t) // no cred file
	cfg := &Config{Providers: map[string]Provider{
		"codex": {Provider: "codex", OpenAIBaseURL: "https://x.invalid/codex"},
	}}
	out := captureStdout(t, func() { showCodexUsage(cfg, cfg.Providers["codex"]) })
	if !strings.Contains(out, "Not logged in") {
		t.Errorf("missing 'Not logged in':\n%s", out)
	}
}

// TestShowCodexUsage_HTTPError: mock returns 500 → "HTTP 500".
func TestShowCodexUsage_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`boom`))
	}))
	defer srv.Close()
	home := useTempHome(t)
	writeCodexAuthFile(t, home, "acct-test")
	cfg := &Config{Providers: map[string]Provider{
		"codex": {Provider: "codex", OpenAIBaseURL: srv.URL + "/codex"},
	}}
	out := captureStdout(t, func() { showCodexUsage(cfg, cfg.Providers["codex"]) })
	if !strings.Contains(out, "HTTP 500") {
		t.Errorf("missing 'HTTP 500':\n%s", out)
	}
}

// --- fetchCodexQuota ---

func TestFetchCodexQuota_Parsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(codexUsageBody))
	}))
	defer srv.Close()
	home := useTempHome(t)
	writeCodexAuthFile(t, home, "acct-test")
	cfg := &Config{Providers: map[string]Provider{
		"codex": {Provider: "codex", OpenAIBaseURL: srv.URL + "/codex"},
	}}
	s, err := fetchCodexQuota(cfg, cfg.Providers["codex"])
	if err != nil {
		t.Fatal(err)
	}
	if s.Billing != provider.BillingPlan {
		t.Errorf("Billing=%v want Plan", s.Billing)
	}
	// ultimate = monthly spend → 0.75 remaining.
	if s.RemainingPct != 0.75 {
		t.Errorf("RemainingPct=%v want 0.75 (spend ultimate)", s.RemainingPct)
	}
}

func TestFetchCodexQuota_NotLoggedIn(t *testing.T) {
	useTempHome(t)
	cfg := &Config{Providers: map[string]Provider{
		"codex": {Provider: "codex", OpenAIBaseURL: "https://x.invalid/codex"},
	}}
	s, err := fetchCodexQuota(cfg, cfg.Providers["codex"])
	if err != nil {
		t.Fatal(err)
	}
	if s.Billing != provider.BillingUnknown {
		t.Errorf("got %+v want BillingUnknown", s)
	}
}

func TestFetchCodexQuota_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
	}))
	defer srv.Close()
	home := useTempHome(t)
	writeCodexAuthFile(t, home, "acct-test")
	cfg := &Config{Providers: map[string]Provider{
		"codex": {Provider: "codex", OpenAIBaseURL: srv.URL + "/codex"},
	}}
	s, _ := fetchCodexQuota(cfg, cfg.Providers["codex"])
	if s.Billing != provider.BillingUnknown || s.Err != "HTTP 502" {
		t.Errorf("got %+v want BillingUnknown/HTTP 502", s)
	}
}

// --- showGenericUsage (zhipu) ---

const zhipuQuotaBody = `{"success":true,"data":{"level":"GLM Coding Plan","limits":[` +
	`{"type":"TOKENS_LIMIT","unit":3,"percentage":40,"nextResetTime":1750000000000,"usage":100000,"currentValue":40000,"remaining":60000,"usageDetails":[{"modelCode":"glm-5.2","usage":30000}]},` +
	`{"type":"TOKENS_LIMIT","unit":6,"percentage":70,"nextResetTime":1750000000000,"usage":200000,"currentValue":140000,"remaining":60000},` +
	`{"type":"TIME_LIMIT","unit":5,"percentage":10,"nextResetTime":1750000000000,"usage":3600,"currentValue":360,"remaining":3240}]}}`

// TestShowGenericUsage_ZhipuQuota: mock returns the BigModel quota format → prints
// level + per-window bars via printQuotaSnapshot.
func TestShowGenericUsage_ZhipuQuota(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(zhipuQuotaBody))
	}))
	defer srv.Close()
	home := useTempHome(t)
	writeCred(t, home, "zhipu", "apikey", mustMarshalT(map[string]string{"api_key": "zk"}))
	cfg := &Config{Providers: map[string]Provider{
		"zhipu": {Provider: "zhipu", UsageURL: srv.URL},
	}}
	out := captureStdout(t, func() {
		showGenericUsage(cfg, "zhipu", cfg.Providers["zhipu"], nil)
	})
	for _, want := range []string{"zhipu", "GLM Coding Plan", "5h tokens", "Weekly tokens", "Monthly time", "By model"} {
		if !strings.Contains(out, want) {
			t.Errorf("zhipu usage missing %q:\n%s", want, out)
		}
	}
}

// TestShowGenericUsage_NotLoggedIn: no cred file → "Not logged in.".
func TestShowGenericUsage_NotLoggedIn(t *testing.T) {
	useTempHome(t)
	cfg := &Config{Providers: map[string]Provider{
		"zhipu": {Provider: "zhipu", UsageURL: "https://x.invalid"},
	}}
	out := captureStdout(t, func() {
		showGenericUsage(cfg, "zhipu", cfg.Providers["zhipu"], nil)
	})
	if !strings.Contains(out, "Not logged in") {
		t.Errorf("missing 'Not logged in':\n%s", out)
	}
}

// TestShowGenericUsage_HTTPError: mock returns 500 → "HTTP 500".
func TestShowGenericUsage_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`boom`))
	}))
	defer srv.Close()
	home := useTempHome(t)
	writeCred(t, home, "zhipu", "apikey", mustMarshalT(map[string]string{"api_key": "zk"}))
	cfg := &Config{Providers: map[string]Provider{
		"zhipu": {Provider: "zhipu", UsageURL: srv.URL},
	}}
	out := captureStdout(t, func() {
		showGenericUsage(cfg, "zhipu", cfg.Providers["zhipu"], nil)
	})
	if !strings.Contains(out, "HTTP 500") {
		t.Errorf("missing 'HTTP 500':\n%s", out)
	}
}

// TestShowGenericUsage_FallbackModelList: body isn't zhipu quota but is an
// OpenAI-style model list → "N models available" + each id.
func TestShowGenericUsage_FallbackModelList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"object":"list","data":[{"id":"gpt-4","owned_by":"x"},{"id":"gpt-3.5","owned_by":"y"}]}`))
	}))
	defer srv.Close()
	home := useTempHome(t)
	writeCred(t, home, "zhipu", "apikey", mustMarshalT(map[string]string{"api_key": "zk"}))
	cfg := &Config{Providers: map[string]Provider{
		"zhipu": {Provider: "zhipu", UsageURL: srv.URL},
	}}
	out := captureStdout(t, func() {
		showGenericUsage(cfg, "zhipu", cfg.Providers["zhipu"], nil)
	})
	for _, want := range []string{"zhipu", "2 models available", "gpt-4", "gpt-3.5"} {
		if !strings.Contains(out, want) {
			t.Errorf("model-list fallback missing %q:\n%s", want, out)
		}
	}
}

// TestShowGenericUsage_FallbackRawJSON: body is neither zhipu quota nor a model
// list → printUsageFields prints the raw JSON keys (sorted).
func TestShowGenericUsage_FallbackRawJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"status":"ok","count":42,"nested":{"x":1}}`))
	}))
	defer srv.Close()
	home := useTempHome(t)
	writeCred(t, home, "zhipu", "apikey", mustMarshalT(map[string]string{"api_key": "zk"}))
	cfg := &Config{Providers: map[string]Provider{
		"zhipu": {Provider: "zhipu", UsageURL: srv.URL},
	}}
	out := captureStdout(t, func() {
		showGenericUsage(cfg, "zhipu", cfg.Providers["zhipu"], nil)
	})
	// printUsageFields prints sorted keys: count, nested, status.
	for _, want := range []string{"count", "42", "nested", "status", "ok"} {
		if !strings.Contains(out, want) {
			t.Errorf("raw-JSON fallback missing %q:\n%s", want, out)
		}
	}
}

// --- fetchZhipuQuota ---

func TestFetchZhipuQuota_Parsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(zhipuQuotaBody))
	}))
	defer srv.Close()
	home := useTempHome(t)
	writeCred(t, home, "zhipu", "apikey", mustMarshalT(map[string]string{"api_key": "zk"}))
	cfg := &Config{Providers: map[string]Provider{
		"zhipu": {Provider: "zhipu", UsageURL: srv.URL},
	}}
	s, err := fetchZhipuQuota(cfg, "zhipu", cfg.Providers["zhipu"], nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Billing != provider.BillingPlan {
		t.Errorf("Billing=%v want Plan", s.Billing)
	}
	// binding = weekly remaining = 0.3 (5h is 0.6; TIME_LIMIT excluded).
	if s.RemainingPct != 0.3 {
		t.Errorf("RemainingPct=%v want 0.3", s.RemainingPct)
	}
	if len(s.Windows) != 3 {
		t.Errorf("Windows len=%d want 3", len(s.Windows))
	}
}

func TestFetchZhipuQuota_NotLoggedIn(t *testing.T) {
	useTempHome(t)
	cfg := &Config{Providers: map[string]Provider{
		"zhipu": {Provider: "zhipu", UsageURL: "https://x.invalid"},
	}}
	s, _ := fetchZhipuQuota(cfg, "zhipu", cfg.Providers["zhipu"], nil)
	if s.Billing != provider.BillingUnknown {
		t.Errorf("got %+v want BillingUnknown", s)
	}
}

func TestFetchZhipuQuota_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	home := useTempHome(t)
	writeCred(t, home, "zhipu", "apikey", mustMarshalT(map[string]string{"api_key": "zk"}))
	cfg := &Config{Providers: map[string]Provider{
		"zhipu": {Provider: "zhipu", UsageURL: srv.URL},
	}}
	s, _ := fetchZhipuQuota(cfg, "zhipu", cfg.Providers["zhipu"], nil)
	if s.Billing != provider.BillingUnknown || s.Err != "HTTP 500" {
		t.Errorf("got %+v want BillingUnknown/HTTP 500", s)
	}
}

// TestFetchZhipuQuota_NotZhipuFormat: body parses as JSON but isn't the zhipu quota
// format → BillingUnknown "not zhipu quota format".
func TestFetchZhipuQuota_NotZhipuFormat(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"object":"list","data":[]}`)) // not zhipu
	}))
	defer srv.Close()
	home := useTempHome(t)
	writeCred(t, home, "zhipu", "apikey", mustMarshalT(map[string]string{"api_key": "zk"}))
	cfg := &Config{Providers: map[string]Provider{
		"zhipu": {Provider: "zhipu", UsageURL: srv.URL},
	}}
	s, _ := fetchZhipuQuota(cfg, "zhipu", cfg.Providers["zhipu"], nil)
	if s.Billing != provider.BillingUnknown || s.Err != "not zhipu quota format" {
		t.Errorf("got %+v want BillingUnknown/not-zhipu-format", s)
	}
}

// --- showVolcengineUsage ---

// TestShowVolcengineUsage_NoCredsFallsBack: no AK/SK cred file → showVolcengineUsage
// prints the GetAFPUsage note and falls back to listConfigModels. (The success
// branch calls getAFPUsage which targets the hardcoded real Volcengine host.)
func TestShowVolcengineUsage_NoCredsFallsBack(t *testing.T) {
	useTempHome(t) // no cred file
	prov := Provider{
		Provider: "volcengine",
		Models:   []string{"doubao", "glm-5.2"},
	}
	cfg := &Config{Providers: map[string]Provider{"volcengine": prov}}
	out := captureStdout(t, func() { showVolcengineUsage(cfg, "volcengine", prov, nil) })
	for _, want := range []string{"volcengine", "GetAFPUsage", "AK/SK", "2 models", "doubao", "glm-5.2"} {
		if !strings.Contains(out, want) {
			t.Errorf("volcengine fallback missing %q:\n%s", want, out)
		}
	}
}

// TestShowVolcengineUsage_GetAFPFailsFallsBack: AK/SK configured, but the signed
// GetAFPUsage call can't reach the real Volcengine host (HTTPS_PROXY forced at a
// dead address → immediate connection refused). showVolcengineUsage prints the
// "GetAFPUsage failed" line and falls back to listConfigModels. This exercises
// getAFPUsage's request + error-return path + the showVolcengineUsage error branch.
func TestShowVolcengineUsage_GetAFPFailsFallsBack(t *testing.T) {
	home := useTempHome(t)
	t.Setenv("HTTPS_PROXY", deadProxyURL(t))
	t.Setenv("HTTP_PROXY", deadProxyURL(t))
	writeCred(t, home, "volcengine", "apikey", mustMarshalT(map[string]string{
		"api_key":    "ark-key",
		"access_key": "AKtest",
		"secret_key": "SKtest",
	}))
	prov := Provider{
		Provider: "volcengine",
		Models:   []string{"doubao"},
	}
	cfg := &Config{Providers: map[string]Provider{"volcengine": prov}}
	out := captureStdout(t, func() { showVolcengineUsage(cfg, "volcengine", prov, nil) })
	if !strings.Contains(out, "GetAFPUsage failed") {
		t.Errorf("missing 'GetAFPUsage failed':\n%s", out)
	}
	if !strings.Contains(out, "1 models") {
		t.Errorf("missing fallback model list:\n%s", out)
	}
}

// TestPrintProviderUsage_Single: the `usage <provider>` dispatch path for a
// single-account provider builds the provider and calls Usage(). Asserts the
// deepseek display is printed (covers printProviderUsage's single-account branch).
func TestPrintProviderUsage_Single(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"is_available":true,"balance_infos":[` +
			`{"currency":"CNY","total_balance":"10.50","granted_balance":"8.00","topped_up_balance":"2.50"}]}`))
	}))
	defer srv.Close()
	home := useTempHome(t)
	writeCred(t, home, "deepseek", "apikey", mustMarshalT(map[string]string{"api_key": "dk"}))
	cfg := &Config{Providers: map[string]Provider{
		"deepseek": {Provider: "deepseek", UsageURL: srv.URL},
	}}
	out := captureStdout(t, func() { printProviderUsage(cfg, "deepseek") })
	for _, want := range []string{"deepseek", "Available:", "CNY", "10.50"} {
		if !strings.Contains(out, want) {
			t.Errorf("printProviderUsage missing %q:\n%s", want, out)
		}
	}
}

// TestFetchDeepseekQuota_BoundCred: non-nil cred path through the shim (covers
// the `if cred != nil` true branch in fetchDeepseekQuota/fetchZhipuQuota).
func TestFetchDeepseekQuota_BoundCred(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"is_available":true,"balance_infos":[{"currency":"USD","total_balance":"5.00","granted_balance":"4.00","topped_up_balance":"1.00"}]}`))
	}))
	defer srv.Close()
	home := useTempHome(t)
	writeCred(t, home, "deepseek", "apikey", mustMarshalT(map[string]string{"api_key": "dk"}))
	cfg := &Config{Providers: map[string]Provider{
		"deepseek": {Provider: "deepseek", UsageURL: srv.URL},
	}}
	cred := accountCred{APIKey: "dk"}
	s, err := fetchDeepseekQuota(cfg, "deepseek", cfg.Providers["deepseek"], &cred)
	if err != nil {
		t.Fatal(err)
	}
	if s.Billing != provider.BillingPayG || len(s.Windows) != 1 || s.Windows[0].Total != 5.0 {
		t.Errorf("bound-cred fetch got %+v", s)
	}
}
