package provider

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newStepPlanForTest(t *testing.T, cfg *Config) *StepPlanProvider {
	t.Helper()
	if cfg == nil {
		cfg = &Config{}
	}
	if cfg.BoundAPIKey == "" {
		cfg.BoundAPIKey = "sk-step-testkey1234567890"
	}
	cfg.ProviderID = "step-plan"
	p, err := New(cfg, "step-plan")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p.(*StepPlanProvider)
}

func TestStepPlan_AuthHeaders_DualWrite(t *testing.T) {
	p := newStepPlanForTest(t, nil)
	req := httptest.NewRequest(http.MethodGet, "https://x/v1/models", nil)
	if err := p.AuthHeaders(req); err != nil {
		t.Fatalf("AuthHeaders: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sk-step-testkey1234567890" {
		t.Errorf("Authorization = %q, want Bearer <key>", got)
	}
	if got := req.Header.Get("x-api-key"); got != "sk-step-testkey1234567890" {
		t.Errorf("x-api-key = %q, want <key> (dual-write for the Anthropic gateway)", got)
	}
}

func TestStepPlan_FetchModels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path = %q, want /models", r.URL.Path)
		}
		w.Write([]byte(`{"data":[{"id":"step-5-preview"},{"id":"step-3.7-flash"},{"id":"step-router-v1"}]}`))
	}))
	defer srv.Close()
	p := newStepPlanForTest(t, &Config{OpenAIBaseURL: srv.URL})
	ids, err := p.FetchModels()
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	want := []string{"step-5-preview", "step-3.7-flash", "step-router-v1"}
	if len(ids) != len(want) {
		t.Fatalf("FetchModels = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("FetchModels[%d] = %q, want %q", i, ids[i], want[i])
		}
	}
}

func TestStepPlan_FetchModels_ErrorOnNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"code":"invalid_api_key","message":"invalid api key"}`))
	}))
	defer srv.Close()
	p := newStepPlanForTest(t, &Config{OpenAIBaseURL: srv.URL})
	if _, err := p.FetchModels(); err == nil {
		t.Error("FetchModels on 401 returned nil error; want error so caller falls back to config models")
	}
}

func TestStepPlan_Quota_BillingUnknownWithConsoleURL(t *testing.T) {
	p := newStepPlanForTest(t, nil)
	s, err := p.Quota()
	if err != nil {
		t.Fatalf("Quota: %v", err)
	}
	if s.Billing != BillingUnknown {
		t.Errorf("Billing = %v, want BillingUnknown (no public Credit API)", s.Billing)
	}
	if s.RemainingPct != -1 {
		t.Errorf("RemainingPct = %v, want the -1 unknown sentinel (0 would read as exhausted)", s.RemainingPct)
	}
	joined := strings.Join(s.Notes, "\n")
	if !strings.Contains(joined, stepPlanConsoleURL) {
		t.Errorf("Notes %q missing console URL %s", joined, stepPlanConsoleURL)
	}
}

func TestStepPlan_ProbeRequest_Anthropic(t *testing.T) {
	p := newStepPlanForTest(t, nil)
	pr := p.ProbeRequest("step-5-preview")
	if pr.Path != "/v1/messages" {
		t.Errorf("ProbeRequest.Path = %q, want /v1/messages (anthropic base is the probe base)", pr.Path)
	}
	if pr.Method != http.MethodPost {
		t.Errorf("ProbeRequest.Method = %q, want POST", pr.Method)
	}
	if !strings.Contains(string(pr.Body), "step-5-preview") {
		t.Errorf("ProbeRequest.Body %q missing model id", pr.Body)
	}
}

func TestStepPlan_ExtraHeaders_AnthropicVersion(t *testing.T) {
	p := newStepPlanForTest(t, nil)
	req := httptest.NewRequest(http.MethodPost, "https://x/v1/messages", nil)
	p.ExtraHeaders(req, "/v1/messages")
	if got := req.Header.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want 2023-06-01", got)
	}
}

func TestStepPlan_RewriteRequest_Passthrough(t *testing.T) {
	p := newStepPlanForTest(t, nil)
	url, body := p.RewriteRequest("https://api.stepfun.com/step_plan/v1/messages", []byte(`{"model":"step-5-preview"}`), "/v1/messages")
	if url != "https://api.stepfun.com/step_plan/v1/messages" {
		t.Errorf("RewriteRequest url = %q, want unchanged", url)
	}
	if string(body) != `{"model":"step-5-preview"}` {
		t.Errorf("RewriteRequest body = %q, want unchanged passthrough", body)
	}
}
