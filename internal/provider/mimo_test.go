package provider

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// newTestMiMo builds a MiMoProvider with a temp auth file so AuthHeaders tests
// don't touch the real ~/.model-proxy store.
func newTestMiMo(t *testing.T, cfg *Config) *MiMoProvider {
	t.Helper()
	if cfg == nil {
		cfg = &Config{}
	}
	if cfg.ProviderID == "" {
		cfg.ProviderID = "mimo"
	}
	p, err := New(cfg, "mimo")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mp := p.(*MiMoProvider)
	mp.ApiKeyBase = &ApiKeyBase{authFile: filepath.Join(t.TempDir(), "mimo_apikey.json")}
	return mp
}

// MiMo documents THREE auth shapes (Bearer + the vendor's own api-key header +
// the Anthropic SDK's x-api-key). All must carry the same key so one config
// serves the OpenAI endpoint, the anthropic endpoint and the /api/v1 usage
// GETs — a gateway reading only its own documented header must not 401.
func TestMiMoAuthHeaders_AllDocumentedShapes(t *testing.T) {
	p := newTestMiMo(t, &Config{OpenAIBaseURL: "https://api.xiaomimimo.com/v1"})
	if err := p.SaveKey("sk-mimo-123"); err != nil {
		t.Fatalf("SaveKey: %v", err)
	}
	req, _ := http.NewRequest("POST", "https://api.xiaomimimo.com/anthropic/v1/messages", nil)
	if err := p.AuthHeaders(req); err != nil {
		t.Fatalf("AuthHeaders: %v", err)
	}
	for _, h := range []string{"Authorization", "x-api-key", "api-key"} {
		want := "sk-mimo-123"
		if h == "Authorization" {
			want = "Bearer sk-mimo-123"
		}
		if got := req.Header.Get(h); got != want {
			t.Errorf("%s = %q, want %q", h, got, want)
		}
	}
}

// AuthHeaders must fail clearly when not logged in (no cred file) — the pool
// build path binds keys in memory, the file path must not silently send an
// empty credential.
func TestMiMoAuthHeaders_NotLoggedIn(t *testing.T) {
	p := newTestMiMo(t, &Config{OpenAIBaseURL: "https://api.xiaomimimo.com/v1"})
	req, _ := http.NewRequest("GET", "https://api.xiaomimimo.com/api/v1/balance", nil)
	if err := p.AuthHeaders(req); err == nil {
		t.Error("AuthHeaders: want error when not logged in, got nil")
	}
}

// A pool-unrolled virtual binds its OWN key: AuthHeaders must use the bound
// key and never fall back to another account's file (credential isolation).
func TestMiMoAuthHeaders_BoundKeyIsolation(t *testing.T) {
	bound := &MiMoProvider{
		ApiKeyBase: NewApiKeyBaseWithKey("mimo", "BOUND-KEY-A"),
		cfg:        &Config{OpenAIBaseURL: "https://api.xiaomimimo.com/v1"},
	}
	// A second virtual with a different bound key must not see the first's.
	other := &MiMoProvider{
		ApiKeyBase: NewApiKeyBaseWithKey("mimo", "BOUND-KEY-B"),
		cfg:        &Config{OpenAIBaseURL: "https://api.xiaomimimo.com/v1"},
	}
	for _, tc := range []struct {
		p    *MiMoProvider
		want string
	}{{bound, "Bearer BOUND-KEY-A"}, {other, "Bearer BOUND-KEY-B"}} {
		req, _ := http.NewRequest("GET", "https://api.xiaomimimo.com/api/v1/balance", nil)
		if err := tc.p.AuthHeaders(req); err != nil {
			t.Fatalf("AuthHeaders: %v", err)
		}
		if got := req.Header.Get("Authorization"); got != tc.want {
			t.Errorf("Authorization = %q, want %q (bound key leaked across virtuals?)", got, tc.want)
		}
		if got := req.Header.Get("api-key"); got != strings.TrimPrefix(tc.want, "Bearer ") {
			t.Errorf("api-key = %q, want the bound key", got)
		}
	}
}

// RewriteRequest is a no-op: the proxy selects the upstream base URL by
// protocol, so MiMo must not rewrite the URL itself (deepseek pattern).
func TestMiMoRewriteRequest_NoOp(t *testing.T) {
	p := newTestMiMo(t, nil)
	for _, tc := range []struct{ url, path string }{
		{"https://api.xiaomimimo.com/anthropic/v1/messages", "/v1/messages"},
		{"https://api.xiaomimimo.com/v1/chat/completions", "/chat/completions"},
	} {
		if got, _ := p.RewriteRequest(tc.url, nil, tc.path); got != tc.url {
			t.Errorf("RewriteRequest(%q, %q) = %q, want unchanged", tc.url, tc.path, got)
		}
	}
}

// FetchModels lists models via the OpenAI-compatible /models endpoint.
func TestMiMoFetchModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path = %q, want /models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-mimo-list" {
			t.Errorf("Authorization = %q, want Bearer <key>", got)
		}
		w.Write([]byte(`{"data":[{"id":"mimo-v2.6-pro"},{"id":"mimo-v2.6-flash"},{"id":"mimo-v2.5"}]}`))
	}))
	defer srv.Close()
	p := newTestMiMo(t, &Config{OpenAIBaseURL: srv.URL})
	if err := p.SaveKey("sk-mimo-list"); err != nil {
		t.Fatalf("SaveKey: %v", err)
	}
	ids, err := p.FetchModels()
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	want := []string{"mimo-v2.6-pro", "mimo-v2.6-flash", "mimo-v2.5"}
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
// display) falls back to the config models: list instead of writing an empty
// list.
func TestMiMoFetchModels_ErrorOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer srv.Close()
	p := newTestMiMo(t, &Config{OpenAIBaseURL: srv.URL})
	if err := p.SaveKey("sk-mimo-bad"); err != nil {
		t.Fatalf("SaveKey: %v", err)
	}
	if _, err := p.FetchModels(); err == nil {
		t.Error("FetchModels on 401 returned nil error; want error so caller falls back to config models")
	}
}

// ProbeRequest keeps the baseProbe OpenAI default (deepseek parity): the probe
// framework selects the anthropic base + /v1/messages when anthropic_base_url
// is set, so a provider-side override would be dead weight.
func TestMiMoProbeRequest_DefaultOpenAIShape(t *testing.T) {
	p := newTestMiMo(t, nil)
	pr := p.ProbeRequest("mimo-v2.6-pro")
	if pr.Path != "/chat/completions" {
		t.Errorf("ProbeRequest.Path = %q, want the baseProbe default /chat/completions", pr.Path)
	}
	if !strings.Contains(string(pr.Body), "mimo-v2.6-pro") {
		t.Errorf("ProbeRequest.Body %q missing model id", pr.Body)
	}
}

// ExtraHeaders sets anthropic-version: the forward path's header whitelist does
// not carry the client's anthropic-version, so a strict Anthropic-compatible
// gateway would 400 without the provider re-adding it (the kimi-code/step-plan
// lesson; deepseek is the exception because its endpoint ignores the header).
func TestMiMoExtraHeaders_AnthropicVersion(t *testing.T) {
	p := newTestMiMo(t, nil)
	req, _ := http.NewRequest("POST", "https://api.xiaomimimo.com/anthropic/v1/messages", nil)
	p.ExtraHeaders(req, "/v1/messages")
	if got := req.Header.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want 2023-06-01", got)
	}
}

// Quota on a healthy balance: BillingPayG with ONE unmeasured money window
// (deepseek contract — a balance is not a windowed budget, so the surplus
// scheduler ranks MiMo as a strict last resort). Only the balance endpoint may
// be contacted: no Token Plan endpoint exists in pay-as-you-go mode.
func TestMiMoQuota_BalanceOnlyPayG(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/balance" {
			t.Errorf("unexpected path %q — pay-as-you-go mode must not poll a Token Plan endpoint", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-mimo-quota" {
			t.Errorf("Authorization = %q, want Bearer <key>", got)
		}
		w.Write([]byte(`{"data":{"balance":"182.50","currency":"CNY","granted_balance":"50.00","topped_up_balance":"132.50"}}`))
	}))
	defer srv.Close()
	p := newTestMiMo(t, &Config{UsageURL: srv.URL + "/api/v1/balance"})
	if err := p.SaveKey("sk-mimo-quota"); err != nil {
		t.Fatalf("SaveKey: %v", err)
	}
	s, err := p.Quota()
	if err != nil {
		t.Fatalf("Quota returned error %v; want a snapshot (the Quota contract)", err)
	}
	if s.Billing != BillingPayG {
		t.Errorf("Billing = %v, want BillingPayG (pay-as-you-go)", s.Billing)
	}
	if s.RemainingPct != -1 {
		t.Errorf("RemainingPct = %v, want -1 (no windowed budget)", s.RemainingPct)
	}
	if len(s.Windows) != 1 {
		t.Fatalf("windows = %+v, want exactly one money window", s.Windows)
	}
	w := s.Windows[0]
	if w.Kind != "money" || w.Total != 182.5 || w.RemainingPct != -1 || w.Ultimate {
		t.Errorf("window = %+v, want money kind, total 182.5, unmeasured, not Ultimate", w)
	}
	if len(w.Details) != 2 || w.Details[0].Used != 50 || w.Details[1].Used != 132.5 {
		t.Errorf("details = %+v, want granted 50 / topped-up 132.5", w.Details)
	}
}

// An unrecognizable balance body (wrong shape, not an error) must yield an
// EMPTY PayG snapshot — never a fabricated zero balance that would read as
// "out of money" in the UI.
func TestMiMoQuota_UnrecognizedBodyIsEmptyPayG(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	p := newTestMiMo(t, &Config{UsageURL: srv.URL + "/api/v1/balance"})
	if err := p.SaveKey("sk-mimo-shape"); err != nil {
		t.Fatalf("SaveKey: %v", err)
	}
	s, err := p.Quota()
	if err != nil {
		t.Fatalf("Quota error: %v", err)
	}
	if s.Billing != BillingPayG {
		t.Errorf("Billing = %v, want BillingPayG", s.Billing)
	}
	if len(s.Windows) != 0 {
		t.Errorf("windows = %+v, want none (no recognizable balance field)", s.Windows)
	}
}

// Configured provider headers must ride on the balance GET (the zhipu/kimi-code
// pattern: a mirror or gateway that needs an extra header still gets a quota).
func TestMiMoQuota_ConfiguredHeadersRideAlong(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Mimo-Mirror"); got != "tenant-7" {
			t.Errorf("X-Mimo-Mirror = %q, want the configured provider header", got)
		}
		if got := r.Header.Get("Accept"); got != "application/json" {
			t.Errorf("Accept = %q, want application/json", got)
		}
		w.Write([]byte(`{"balance":1}`))
	}))
	defer srv.Close()
	p := newTestMiMo(t, &Config{
		UsageURL: srv.URL + "/api/v1/balance",
		Headers:  map[string]string{"X-Mimo-Mirror": "tenant-7"},
	})
	if err := p.SaveKey("sk-mimo-hdr"); err != nil {
		t.Fatalf("SaveKey: %v", err)
	}
	if _, err := p.Quota(); err != nil {
		t.Fatalf("Quota: %v", err)
	}
}

// A failing balance fetch fails the WHOLE snapshot as BillingUnknown carrying
// the error — never a non-nil error (the scheduler poll must stay alive) and
// never a snapshot that claims PayG while the fetch failed.
func TestMiMoQuota_BalanceFailureIsBillingUnknown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"invalid api key"}}`))
	}))
	defer srv.Close()
	p := newTestMiMo(t, &Config{UsageURL: srv.URL + "/api/v1/balance"})
	if err := p.SaveKey("sk-mimo-bad"); err != nil {
		t.Fatalf("SaveKey: %v", err)
	}
	s, err := p.Quota()
	if err != nil {
		t.Fatalf("Quota returned error %v; want a BillingUnknown snapshot", err)
	}
	if s.Billing != BillingUnknown {
		t.Errorf("Billing = %v, want BillingUnknown", s.Billing)
	}
	if s.Err == "" {
		t.Error("Err empty — the failure reason must ride on the snapshot")
	}
}

// Without usage_url there is nothing to poll: BillingUnknown + console notes
// (never a bogus zero balance).
func TestMiMoQuota_NoUsageURL(t *testing.T) {
	p := newTestMiMo(t, &Config{})
	s, err := p.Quota()
	if err != nil {
		t.Fatalf("Quota error: %v", err)
	}
	if s.Billing != BillingUnknown {
		t.Errorf("Billing = %v, want BillingUnknown", s.Billing)
	}
	joined := strings.Join(s.Notes, "\n")
	if !strings.Contains(joined, mimoConsoleURL) {
		t.Errorf("Notes %q missing console URL %s", joined, mimoConsoleURL)
	}
}

// --- parsers ---

func TestParseMiMoBalance(t *testing.T) {
	t.Run("flat", func(t *testing.T) {
		ws := ParseMiMoBalance([]byte(`{"balance":182.5,"currency":"CNY"}`))
		if len(ws) != 1 || ws[0].Total != 182.5 || ws[0].Label != "CNY" {
			t.Fatalf("windows = %+v, want one CNY money window of 182.5", ws)
		}
		if ws[0].Kind != "money" || ws[0].RemainingPct != -1 {
			t.Errorf("window = %+v, want money kind, unmeasured", ws[0])
		}
	})
	t.Run("data envelope with numeric strings and split", func(t *testing.T) {
		ws := ParseMiMoBalance([]byte(`{"data":{"total_balance":"182.50","granted_balance":"50.00","topped_up_balance":"132.50","currency":"CNY"}}`))
		if len(ws) != 1 || ws[0].Total != 182.5 {
			t.Fatalf("windows = %+v, want one money window of 182.5", ws)
		}
		if len(ws[0].Details) != 2 || ws[0].Details[0].Used != 50 || ws[0].Details[1].Used != 132.5 {
			t.Errorf("details = %+v, want granted 50 / topped-up 132.5", ws[0].Details)
		}
	})
	t.Run("whitespace-only currency falls back to Balance", func(t *testing.T) {
		ws := ParseMiMoBalance([]byte(`{"balance":10,"currency":"   "}`))
		if len(ws) != 1 || ws[0].Label != "Balance" {
			t.Errorf("windows = %+v, want the Balance label (blank currency)", ws)
		}
	})
	t.Run("unrecognized body", func(t *testing.T) {
		for _, body := range []string{`{"ok":true}`, `not json`, `[1,2]`, `null`} {
			if ws := ParseMiMoBalance([]byte(body)); ws != nil {
				t.Errorf("ParseMiMoBalance(%s) = %+v, want nil", body, ws)
			}
		}
	})
}

// Usage() must print the "Provider: <name>" first line (the usage-display
// contract) and never fail on an unmeasured snapshot.
func TestMiMoUsage_PrintsProviderLine(t *testing.T) {
	p := newTestMiMo(t, &Config{Models: []string{"mimo-v2.6-pro"}})
	if err := p.Usage(); err != nil {
		t.Fatalf("Usage: %v", err)
	}
}

// The measured display path must render from the same Quota() the scheduler
// polls — the CLI is the human face of the polled numbers, not a second parser.
func TestMiMoUsage_MeasuredWindows(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/balance" {
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(`{"balance":"182.50","currency":"CNY","granted_balance":"50.00"}`))
	}))
	defer srv.Close()
	p := newTestMiMo(t, &Config{UsageURL: srv.URL + "/api/v1/balance", Models: []string{"mimo-v2.6-pro"}})
	if err := p.SaveKey("sk-mimo-usage"); err != nil {
		t.Fatalf("SaveKey: %v", err)
	}
	out := captureStdoutProvider(func() {
		if err := p.Usage(); err != nil {
			t.Fatalf("Usage: %v", err)
		}
	})
	for _, want := range []string{"Provider:  ", "mimo", "CNY", "182", "granted", "unmeasured"} {
		if !strings.Contains(out, want) {
			t.Errorf("usage output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Token Plan") {
		t.Errorf("usage output mentions Token Plan in pay-as-you-go mode:\n%s", out)
	}
}

// MiMo's chat endpoint takes the thinking switch (thinking.type
// enabled|disabled) with NO effort ladder — the r→chat dialect must render
// `thinking`, and no ChatEffortProfile may be registered (there is no vendor
// enum to map onto). Pins the protocol_hint.go registration.
func TestMiMoReasoningDialect(t *testing.T) {
	if got := ChatReasoningMode("mimo"); got != "thinking" {
		t.Errorf("ChatReasoningMode(mimo) = %q, want \"thinking\" (vendor docs: thinking.type enabled|disabled, no effort dial)", got)
	}
	if p := ChatEffortProfile("mimo", "mimo-v2.6-pro"); len(p.Enum) > 0 || p.EnumOnly {
		t.Errorf("ChatEffortProfile(mimo) = %+v, want the zero profile (no vendor effort enum)", p)
	}
}

// Logout deletes the stored key (apikey pool contract).
func TestMiMoLogout_DeletesKey(t *testing.T) {
	p := newTestMiMo(t, &Config{OpenAIBaseURL: "https://api.xiaomimimo.com/v1"})
	if err := p.SaveKey("sk-mimo-bye"); err != nil {
		t.Fatalf("SaveKey: %v", err)
	}
	if err := p.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	req, _ := http.NewRequest("GET", "https://api.xiaomimimo.com/api/v1/balance", nil)
	if err := p.AuthHeaders(req); err == nil {
		t.Error("AuthHeaders after Logout: want error (key deleted), got nil")
	}
}
