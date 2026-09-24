package provider

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newQwenPlanForTest(t *testing.T, cfg *Config) *QwenPlanProvider {
	t.Helper()
	if cfg == nil {
		cfg = &Config{}
	}
	if cfg.BoundAPIKey == "" {
		cfg.BoundAPIKey = "sk-sp-testkey1234567890"
	}
	cfg.ProviderID = "qwen-plan"
	p, err := New(cfg, "qwen-plan")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p.(*QwenPlanProvider)
}

func TestQwenPlan_AuthHeaders_DualWrite(t *testing.T) {
	p := newQwenPlanForTest(t, nil)
	req := httptest.NewRequest(http.MethodGet, "https://x/v1/models", nil)
	if err := p.AuthHeaders(req); err != nil {
		t.Fatalf("AuthHeaders: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sk-sp-testkey1234567890" {
		t.Errorf("Authorization = %q, want Bearer <key>", got)
	}
	if got := req.Header.Get("x-api-key"); got != "sk-sp-testkey1234567890" {
		t.Errorf("x-api-key = %q, want <key> (dual-write for the Anthropic gateway)", got)
	}
}

func TestQwenPlan_FetchModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path = %q, want /models", r.URL.Path)
		}
		w.Write([]byte(`{"data":[{"id":"qwen3.7-max"},{"id":"glm-5.2"},{"id":"deepseek-v4-pro"}]}`))
	}))
	defer srv.Close()
	p := newQwenPlanForTest(t, &Config{OpenAIBaseURL: srv.URL})
	ids, err := p.FetchModels()
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	want := []string{"qwen3.7-max", "glm-5.2", "deepseek-v4-pro"}
	if len(ids) != len(want) {
		t.Fatalf("FetchModels = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("FetchModels[%d] = %q, want %q", i, ids[i], want[i])
		}
	}
}

func TestQwenPlan_FetchModels_ErrorOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"code":"InvalidApiKey","message":"No API-key provided."}`))
	}))
	defer srv.Close()
	p := newQwenPlanForTest(t, &Config{OpenAIBaseURL: srv.URL})
	if _, err := p.FetchModels(); err == nil {
		t.Error("FetchModels on 401 returned nil error; want error so caller falls back to config models")
	}
}

func TestQwenPlan_Quota_BillingUnknownWithConsoleURL(t *testing.T) {
	p := newQwenPlanForTest(t, nil)
	s, err := p.Quota()
	if err != nil {
		t.Fatalf("Quota: %v", err)
	}
	if s.Billing != BillingUnknown {
		t.Errorf("Billing = %v, want BillingUnknown (no public Credits API)", s.Billing)
	}
	if s.RemainingPct != -1 {
		t.Errorf("RemainingPct = %v, want the -1 unknown sentinel (0 would read as exhausted)", s.RemainingPct)
	}
	if len(s.Windows) != 0 {
		t.Errorf("Windows = %v, want none (unmeasured)", s.Windows)
	}
	joined := strings.Join(s.Notes, "\n")
	if !strings.Contains(joined, "platform.qianwenai.com/home/billing/subscription/token-plan-individual") {
		t.Errorf("Notes missing console URL:\n%s", joined)
	}
	if !strings.Contains(joined, "console") {
		t.Errorf("Notes missing console-only hint:\n%s", joined)
	}
}

func TestQwenPlan_Usage_PrintsConsoleURL(t *testing.T) {
	p := newQwenPlanForTest(t, &Config{Models: []string{"qwen3.7-max", "glm-5.2"}})
	out := captureStdoutProvider(func() { _ = p.Usage() })
	if !contains(out, "platform.qianwenai.com/home/billing/subscription/token-plan-individual") {
		t.Errorf("usage output missing console URL:\n%s", out)
	}
	if !contains(out, "qwen-plan") {
		t.Errorf("usage output missing provider name:\n%s", out)
	}
}

func TestQwenPlan_RewriteRequest_Passthrough(t *testing.T) {
	p := newQwenPlanForTest(t, nil)
	const inURL = "https://up/chat/completions"
	inBody := []byte(`{"model":"qwen3.7-max"}`)
	outURL, outBody := p.RewriteRequest(inURL, inBody, "/chat/completions")
	if outURL != inURL || string(outBody) != string(inBody) {
		t.Errorf("RewriteRequest altered passthrough: url=%q body=%q", outURL, outBody)
	}
}

func TestQwenPlan_Logout_BoundNoOp(t *testing.T) {
	// Bound base (pool virtual) → DeleteKey is a no-op (never touches the file).
	p := newQwenPlanForTest(t, nil)
	if err := p.Logout(); err != nil {
		t.Errorf("Logout: %v", err)
	}
}

// probeModelCallable selects the anthropic base when anthropic_base_url is set, so
// qwen-plan is probed over the ANTHROPIC protocol. Its ProbeRequest must return
// the anthropic /v1/messages path + body (not baseProbe's OpenAI /chat/completions,
// which would hit .../apps/anthropic/chat/completions and 404 for every model).
func TestQwenPlan_ProbeRequest_AnthropicShape(t *testing.T) {
	p := newQwenPlanForTest(t, nil)
	pr := p.ProbeRequest("qwen3.7-max")
	if pr.Method != http.MethodPost || pr.Path != "/v1/messages" {
		t.Errorf("ProbeRequest = %+v, want POST /v1/messages (anthropic; probe uses the anthropic base)", pr)
	}
	if !strings.Contains(string(pr.Body), `"messages"`) || strings.Contains(string(pr.Body), `"stream"`) {
		t.Errorf("ProbeRequest.Body not the anthropic messages shape: %s", pr.Body)
	}
}

// The anthropic-compatible endpoint requires anthropic-version; the probe has no
// client request to inherit it from, so the provider must set it via ExtraHeaders.
func TestQwenPlan_ExtraHeaders_AnthropicVersion(t *testing.T) {
	p := newQwenPlanForTest(t, nil)
	req := httptest.NewRequest(http.MethodPost, "https://x/apps/anthropic/v1/messages", nil)
	p.ExtraHeaders(req, "/v1/messages")
	if got := req.Header.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want 2023-06-01", got)
	}
}
