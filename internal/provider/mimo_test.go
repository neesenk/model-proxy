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
// serves the OpenAI endpoint, the anthropic endpoint and the /models
// validation probe — a gateway reading only its own documented header must not
// 401.
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
	req, _ := http.NewRequest("GET", "https://api.xiaomimimo.com/v1/models", nil)
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
		req, _ := http.NewRequest("GET", "https://api.xiaomimimo.com/v1/models", nil)
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

// FetchModels lists models via the OpenAI-compatible /models endpoint (the
// same endpoint login validates the key against, since MiMo has no
// API-key-authenticated billing endpoint).
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

// Quota is unmeasured by design: MiMo's balance endpoint is cookie-gated
// (console SSO), NOT API-key authenticated — verified live (401 loginUrl
// redirect for every auth-header spelling, 404 on the inference host). The
// snapshot must be BillingUnknown carrying the console URL, and must NEVER
// claim a measured (or zero) balance.
func TestMiMoQuota_ConsoleOnlyUnknown(t *testing.T) {
	p := newTestMiMo(t, &Config{OpenAIBaseURL: "https://api.xiaomimimo.com/v1"})
	s, err := p.Quota()
	if err != nil {
		t.Fatalf("Quota error: %v", err)
	}
	if s.Billing != BillingUnknown {
		t.Errorf("Billing = %v, want BillingUnknown (console-only)", s.Billing)
	}
	if s.RemainingPct != -1 {
		t.Errorf("RemainingPct = %v, want -1 (unmeasured)", s.RemainingPct)
	}
	if len(s.Windows) != 0 {
		t.Errorf("windows = %+v, want none (no API-key billing endpoint)", s.Windows)
	}
	joined := strings.Join(s.Notes, "\n")
	if !strings.Contains(joined, mimoConsoleURL) {
		t.Errorf("Notes %q missing console URL %s", joined, mimoConsoleURL)
	}
}

// Quota must not perform any HTTP call: a console-only provider that dialed
// out would 404 (the bug this shape replaces) or, worse, tempt a cookie
// scrape. Pinning the no-network contract.
func TestMiMoQuota_NoHTTPCall(t *testing.T) {
	dialed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dialed = true
		w.Write([]byte(`{"balance":999}`))
	}))
	defer srv.Close()
	// Any URL the provider might be tempted to poll points at the tripwire.
	p := newTestMiMo(t, &Config{
		OpenAIBaseURL: srv.URL,
		UsageURL:      srv.URL + "/api/v1/balance",
	})
	if err := p.SaveKey("sk-mimo-net"); err != nil {
		t.Fatalf("SaveKey: %v", err)
	}
	if _, err := p.Quota(); err != nil {
		t.Fatalf("Quota: %v", err)
	}
	if dialed {
		t.Error("Quota performed an HTTP call — console-only providers must not poll")
	}
}

// Usage() must print the "Provider: <name>" first line (the usage-display
// contract), the console pointer, and never fail on an unmeasured snapshot.
func TestMiMoUsage_PrintsProviderLine(t *testing.T) {
	p := newTestMiMo(t, &Config{Models: []string{"mimo-v2.6-pro"}})
	out := captureStdoutProvider(func() {
		if err := p.Usage(); err != nil {
			t.Fatalf("Usage: %v", err)
		}
	})
	for _, want := range []string{"Provider:  ", "mimo", mimoConsoleURL, "mimo-v2.6-pro"} {
		if !strings.Contains(out, want) {
			t.Errorf("usage output missing %q:\n%s", want, out)
		}
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
	req, _ := http.NewRequest("GET", "https://api.xiaomimimo.com/v1/models", nil)
	if err := p.AuthHeaders(req); err == nil {
		t.Error("AuthHeaders after Logout: want error (key deleted), got nil")
	}
}
