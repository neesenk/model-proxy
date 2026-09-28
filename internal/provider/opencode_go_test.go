package provider

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// newTestOpenCodeGo builds an OpenCodeGoProvider with a temp auth file so
// AuthHeaders tests don't touch the real ~/.model-proxy store.
func newTestOpenCodeGo(t *testing.T, cfg *Config) *OpenCodeGoProvider {
	t.Helper()
	if cfg == nil {
		cfg = &Config{}
	}
	if cfg.ProviderID == "" {
		cfg.ProviderID = "opencode-go"
	}
	p, err := New(cfg, "opencode-go")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	op := p.(*OpenCodeGoProvider)
	op.ApiKeyBase = &ApiKeyBase{authFile: filepath.Join(t.TempDir(), "opencode-go_apikey.json")}
	return op
}

// The two Go legs read DIFFERENT auth headers (verified live 2026-09 against
// /zen/go): the chat/completions + /models leg reads Authorization: Bearer,
// the /v1/messages anthropic leg reads x-api-key ONLY (Bearer-only answers
// "Missing API key."). AuthHeaders must dual-write both so one config serves
// every leg.
func TestOpenCodeGoAuthHeaders_DualWrite(t *testing.T) {
	p := newTestOpenCodeGo(t, &Config{OpenAIBaseURL: "https://opencode.ai/zen/go/v1"})
	if err := p.SaveKey("sk-opencode-go-123"); err != nil {
		t.Fatalf("SaveKey: %v", err)
	}
	req, _ := http.NewRequest("POST", "https://opencode.ai/zen/go/v1/messages", nil)
	if err := p.AuthHeaders(req); err != nil {
		t.Fatalf("AuthHeaders: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sk-opencode-go-123" {
		t.Errorf("Authorization = %q, want Bearer <key> (chat leg)", got)
	}
	if got := req.Header.Get("x-api-key"); got != "sk-opencode-go-123" {
		t.Errorf("x-api-key = %q, want the raw key (anthropic leg reads x-api-key only)", got)
	}
}

// AuthHeaders must fail clearly when not logged in (no cred file) — the pool
// build path binds keys in memory, the file path must not silently send an
// empty credential.
func TestOpenCodeGoAuthHeaders_NotLoggedIn(t *testing.T) {
	p := newTestOpenCodeGo(t, &Config{OpenAIBaseURL: "https://opencode.ai/zen/go/v1"})
	req, _ := http.NewRequest("GET", "https://opencode.ai/zen/go/v1/models", nil)
	if err := p.AuthHeaders(req); err == nil {
		t.Error("AuthHeaders: want error when not logged in, got nil")
	}
}

// A pool-unrolled virtual binds its OWN key: AuthHeaders must use the bound
// key and never fall back to another account's file (credential isolation).
func TestOpenCodeGoAuthHeaders_BoundKeyIsolation(t *testing.T) {
	bound := &OpenCodeGoProvider{
		ApiKeyBase: NewApiKeyBaseWithKey("opencode-go", "BOUND-KEY-A"),
		cfg:        &Config{OpenAIBaseURL: "https://opencode.ai/zen/go/v1"},
	}
	other := &OpenCodeGoProvider{
		ApiKeyBase: NewApiKeyBaseWithKey("opencode-go", "BOUND-KEY-B"),
		cfg:        &Config{OpenAIBaseURL: "https://opencode.ai/zen/go/v1"},
	}
	for _, tc := range []struct {
		p    *OpenCodeGoProvider
		want string
	}{{bound, "BOUND-KEY-A"}, {other, "BOUND-KEY-B"}} {
		req, _ := http.NewRequest("GET", "https://opencode.ai/zen/go/v1/models", nil)
		if err := tc.p.AuthHeaders(req); err != nil {
			t.Fatalf("AuthHeaders: %v", err)
		}
		if got := req.Header.Get("Authorization"); got != "Bearer "+tc.want {
			t.Errorf("Authorization = %q, want %q (bound key leaked across virtuals?)", got, tc.want)
		}
		if got := req.Header.Get("x-api-key"); got != tc.want {
			t.Errorf("x-api-key = %q, want the bound key", got)
		}
	}
}

// RewriteRequest is a no-op: the proxy selects the upstream base URL by
// protocol, so opencode-go must not rewrite the URL itself (deepseek pattern).
func TestOpenCodeGoRewriteRequest_NoOp(t *testing.T) {
	p := newTestOpenCodeGo(t, nil)
	for _, tc := range []struct{ url, path string }{
		{"https://opencode.ai/zen/go/v1/messages", "/v1/messages"},
		{"https://opencode.ai/zen/go/v1/chat/completions", "/chat/completions"},
	} {
		if got, _ := p.RewriteRequest(tc.url, nil, tc.path); got != tc.url {
			t.Errorf("RewriteRequest(%q, %q) = %q, want unchanged", tc.url, tc.path, got)
		}
	}
}

// FetchModels lists the Go catalog via the OpenAI-compatible /models endpoint
// (bare ids, owned_by "opencode").
func TestOpenCodeGoFetchModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path = %q, want /models", r.URL.Path)
		}
		w.Write([]byte(`{"data":[{"id":"kimi-k3"},{"id":"glm-5.3"},{"id":"gpt-6-luna"}]}`))
	}))
	defer srv.Close()
	p := newTestOpenCodeGo(t, &Config{OpenAIBaseURL: srv.URL})
	if err := p.SaveKey("sk-opencode-go-list"); err != nil {
		t.Fatalf("SaveKey: %v", err)
	}
	ids, err := p.FetchModels()
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	want := []string{"kimi-k3", "glm-5.3", "gpt-6-luna"}
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
func TestOpenCodeGoFetchModels_ErrorOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"type":"error","error":{"type":"AuthError","message":"Invalid API key."}}`))
	}))
	defer srv.Close()
	p := newTestOpenCodeGo(t, &Config{OpenAIBaseURL: srv.URL})
	if err := p.SaveKey("sk-opencode-go-bad"); err != nil {
		t.Fatalf("SaveKey: %v", err)
	}
	if _, err := p.FetchModels(); err == nil {
		t.Error("FetchModels on 401 returned nil error; want error so caller falls back to config models")
	}
}

// ProbeRequest keeps the baseProbe OpenAI default (deepseek parity): the
// probe framework selects the anthropic base + /v1/messages when
// anthropic_base_url is set, so a provider-side override would be dead weight.
func TestOpenCodeGoProbeRequest_DefaultOpenAIShape(t *testing.T) {
	p := newTestOpenCodeGo(t, nil)
	pr := p.ProbeRequest("kimi-k3")
	if pr.Path != "/chat/completions" {
		t.Errorf("ProbeRequest.Path = %q, want the baseProbe default /chat/completions", pr.Path)
	}
	if !strings.Contains(string(pr.Body), "kimi-k3") {
		t.Errorf("ProbeRequest.Body %q missing model id", pr.Body)
	}
}

// ExtraHeaders sets anthropic-version (the whitelist does not carry it) and
// mirrors the client's session identity into x-opencode-session (Go REQUIRES
// one; the forward whitelist already passes native session headers through).
// Precedence: an explicit x-opencode-session from the client wins, then
// native session headers, then the spec-defined BODY fields
// (prompt_cache_key / metadata.user_id / client_metadata.session_id — Kimi
// Code carries its stable id exactly there, see
// github.com/MoonshotAI/kimi-code/issues/3506), and only a body with no
// identity at all (the proxy's own probes) gets a per-request mp-<uuid>
// synthesized (upstream 400s MissingSessionID otherwise).
func TestOpenCodeGoExtraHeaders_SessionMirror(t *testing.T) {
	p := newTestOpenCodeGo(t, nil)

	// Claude Code's native header mirrors into x-opencode-session.
	req, _ := http.NewRequest("POST", "https://opencode.ai/zen/go/v1/messages", nil)
	req.Header.Set("x-claude-code-session-id", "sess-cc-1")
	p.ExtraHeaders(req, nil, "/v1/messages")
	if got := req.Header.Get("x-opencode-session"); got != "sess-cc-1" {
		t.Errorf("x-opencode-session = %q, want mirror of x-claude-code-session-id", got)
	}
	if got := req.Header.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want 2023-06-01", got)
	}

	// pi-style x-session-id mirrors when claude-code's is absent.
	req2, _ := http.NewRequest("POST", "https://opencode.ai/zen/go/v1/messages", nil)
	req2.Header.Set("x-session-id", "sess-pi-2")
	p.ExtraHeaders(req2, nil, "/v1/messages")
	if got := req2.Header.Get("x-opencode-session"); got != "sess-pi-2" {
		t.Errorf("x-opencode-session = %q, want mirror of x-session-id", got)
	}

	// An already-present x-opencode-session is never overwritten.
	req3, _ := http.NewRequest("POST", "https://opencode.ai/zen/go/v1/messages", nil)
	req3.Header.Set("x-opencode-session", "sess-own")
	req3.Header.Set("x-session-id", "sess-other")
	p.ExtraHeaders(req3, nil, "/v1/messages")
	if got := req3.Header.Get("x-opencode-session"); got != "sess-own" {
		t.Errorf("x-opencode-session = %q, want the client's own value preserved", got)
	}

	// Spec-conforming header-less agents: the stable id rides in the BODY.
	// Kimi Code on the OpenAI wire (prompt_cache_key, chat + responses legs):
	// mirrored verbatim so one conversation keeps one routing/prompt-cache
	// lane (the per-request synthesized id would defeat upstream caching).
	reqK, _ := http.NewRequest("POST", "https://opencode.ai/zen/go/v1/chat/completions", nil)
	p.ExtraHeaders(reqK, []byte(`{"model":"kimi-k3","prompt_cache_key":"kimi-sess-9a2f","messages":[{"role":"user","content":"hi"}]}`), "/chat/completions")
	if got := reqK.Header.Get("x-opencode-session"); got != "kimi-sess-9a2f" {
		t.Errorf("x-opencode-session = %q, want body prompt_cache_key mirrored", got)
	}
	// Kimi Code on the Anthropic wire (metadata.user_id) and Codex on
	// Responses (client_metadata.session_id) mirror the same way.
	reqM, _ := http.NewRequest("POST", "https://opencode.ai/zen/go/v1/messages", nil)
	p.ExtraHeaders(reqM, []byte(`{"model":"kimi-k3","metadata":{"user_id":"hashed-user-3"},"messages":[]}`), "/v1/messages")
	if got := reqM.Header.Get("x-opencode-session"); got != "hashed-user-3" {
		t.Errorf("x-opencode-session = %q, want body metadata.user_id mirrored", got)
	}
	reqC, _ := http.NewRequest("POST", "https://opencode.ai/zen/go/v1/responses", nil)
	p.ExtraHeaders(reqC, []byte(`{"model":"gpt-5.6","client_metadata":{"session_id":"codex-sess-7"},"input":[]}`), "/responses")
	if got := reqC.Header.Get("x-opencode-session"); got != "codex-sess-7" {
		t.Errorf("x-opencode-session = %q, want body client_metadata.session_id mirrored", got)
	}
	// A native session header outranks the body identity.
	reqH, _ := http.NewRequest("POST", "https://opencode.ai/zen/go/v1/chat/completions", nil)
	reqH.Header.Set("x-session-id", "sess-native")
	p.ExtraHeaders(reqH, []byte(`{"prompt_cache_key":"kimi-sess-9a2f"}`), "/chat/completions")
	if got := reqH.Header.Get("x-opencode-session"); got != "sess-native" {
		t.Errorf("x-opencode-session = %q, want native header to outrank body identity", got)
	}
	// A body with no identity fields at all still synthesizes (probe shape).
	reqN, _ := http.NewRequest("POST", "https://opencode.ai/zen/go/v1/messages", nil)
	p.ExtraHeaders(reqN, []byte(`{"model":"kimi-k3","messages":[]}`), "/v1/messages")
	if got := reqN.Header.Get("x-opencode-session"); got == "" || !strings.HasPrefix(got, "mp-") {
		t.Errorf("x-opencode-session = %q, want synthesized mp-<uuid> for identity-less body", got)
	}

	// No session header anywhere → a per-request synthesized id keeps the
	// request routable (upstream 400s MissingSessionID otherwise): marked with
	// the mp- prefix and fresh on every call (no conversation key to be stable
	// across).
	req4, _ := http.NewRequest("POST", "https://opencode.ai/zen/go/v1/messages", nil)
	p.ExtraHeaders(req4, nil, "/v1/messages")
	sess4 := req4.Header.Get("x-opencode-session")
	if sess4 == "" || !strings.HasPrefix(sess4, "mp-") {
		t.Errorf("x-opencode-session = %q, want synthesized mp-<uuid> for header-less agents", sess4)
	}
	req5, _ := http.NewRequest("POST", "https://opencode.ai/zen/go/v1/chat/completions", nil)
	p.ExtraHeaders(req5, nil, "/chat/completions")
	sess5 := req5.Header.Get("x-opencode-session")
	if sess5 == "" || sess5 == sess4 {
		t.Errorf("x-opencode-session = %q, want a fresh synthesized id per request (!= %q)", sess5, sess4)
	}
}

// Quota is unmeasured by design: Go's usage windows are console-only (docs:
// "You can track your current usage in the console"). The snapshot must be
// BillingUnknown carrying the console URL, and must NEVER claim a measured
// (or zero) balance.
func TestOpenCodeGoQuota_ConsoleOnlyUnknown(t *testing.T) {
	p := newTestOpenCodeGo(t, &Config{OpenAIBaseURL: "https://opencode.ai/zen/go/v1"})
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
		t.Errorf("windows = %+v, want none (no API-key usage endpoint)", s.Windows)
	}
	joined := strings.Join(s.Notes, "\n")
	if !strings.Contains(joined, opencodeGoConsoleURL) {
		t.Errorf("Notes %q missing console URL %s", joined, opencodeGoConsoleURL)
	}
}

// Quota must not perform any HTTP call: a console-only provider that dialed
// out would only fetch the marketing SPA. Pinning the no-network contract.
func TestOpenCodeGoQuota_NoHTTPCall(t *testing.T) {
	dialed := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dialed = true
		w.Write([]byte(`{"usage":999}`))
	}))
	defer srv.Close()
	// Any URL the provider might be tempted to poll points at the tripwire.
	p := newTestOpenCodeGo(t, &Config{
		OpenAIBaseURL: srv.URL,
		UsageURL:      srv.URL + "/zen/go/v1/usage",
	})
	if err := p.SaveKey("sk-opencode-go-net"); err != nil {
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
// contract), the console pointer + limit structure, and the config models.
func TestOpenCodeGoUsage_PrintsProviderLine(t *testing.T) {
	p := newTestOpenCodeGo(t, &Config{Models: []string{"kimi-k3"}})
	out := captureStdoutProvider(func() {
		if err := p.Usage(); err != nil {
			t.Fatalf("Usage: %v", err)
		}
	})
	for _, want := range []string{"Provider:  ", "opencode-go", opencodeGoConsoleURL, "kimi-k3", "5h=20%"} {
		if !strings.Contains(out, want) {
			t.Errorf("usage output missing %q:\n%s", want, out)
		}
	}
}

// Go's chat endpoint has no verified vendor-specific reasoning field shape —
// the honest registration is the DEFAULT flat reasoning_effort dialect and no
// ChatEffortProfile (no entry in either registry; pins protocol_hint.go).
func TestOpenCodeGoReasoningDialect(t *testing.T) {
	if got := ChatReasoningMode("opencode-go"); got != "reasoning_effort" {
		t.Errorf("ChatReasoningMode(opencode-go) = %q, want the default \"reasoning_effort\" (no verified vendor shape)", got)
	}
	if p := ChatEffortProfile("opencode-go", "glm-5.3"); len(p.Enum) > 0 || p.EnumOnly {
		t.Errorf("ChatEffortProfile(opencode-go) = %+v, want the zero profile (no vendor effort enum)", p)
	}
}

// The registry wiring: provider_id "opencode-go" builds an OpenCodeGoProvider.
func TestOpenCodeGoRegistered(t *testing.T) {
	p, err := New(&Config{ProviderID: "opencode-go"}, "opencode-go")
	if err != nil {
		t.Fatalf("New(opencode-go): %v", err)
	}
	if _, ok := p.(*OpenCodeGoProvider); !ok {
		t.Fatalf("New(opencode-go) returned %T", p)
	}
}

// Logout deletes the stored key (apikey pool contract).
func TestOpenCodeGoLogout_DeletesKey(t *testing.T) {
	p := newTestOpenCodeGo(t, &Config{OpenAIBaseURL: "https://opencode.ai/zen/go/v1"})
	if err := p.SaveKey("sk-opencode-go-bye"); err != nil {
		t.Fatalf("SaveKey: %v", err)
	}
	if err := p.Logout(); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	req, _ := http.NewRequest("GET", "https://opencode.ai/zen/go/v1/models", nil)
	if err := p.AuthHeaders(req); err == nil {
		t.Error("AuthHeaders after Logout: want error (key deleted), got nil")
	}
}
