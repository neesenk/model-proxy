package provider

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// newTestVolcengine builds a VolcengineProvider with a temp auth file so the
// AuthHeaders test doesn't touch the real ~/.model-proxy store.
func newTestVolcengine(t *testing.T) *VolcengineProvider {
	t.Helper()
	return &VolcengineProvider{
		ApiKeyBase: &ApiKeyBase{authFile: filepath.Join(t.TempDir(), "volcengine_apikey.json")},
		cfg:        &Config{OpenAIBaseURL: "https://ark.cn-beijing.volces.com/api/plan/v3"},
	}
}

// RewriteRequest is a no-op (the proxy selects the base by protocol).
func TestVolcengineRewriteRequest_NoOp(t *testing.T) {
	p := newTestVolcengine(t)
	for _, tc := range []struct{ url, path string }{
		{"https://ark.cn-beijing.volces.com/api/plan/v1/messages", "/messages"},
		{"https://ark.cn-beijing.volces.com/api/plan/v3/chat/completions", "/chat/completions"},
	} {
		if got, _ := p.RewriteRequest(tc.url, nil, tc.path); got != tc.url {
			t.Errorf("RewriteRequest(%q): got %q, want unchanged", tc.url, got)
		}
	}
}

// One key must authenticate BOTH protocol legs. The proxy selects the upstream
// base by protocol — chat-family clients go to openai_base_url
// (/chat/completions on the Ark plan v3 endpoint), anthropic clients to
// anthropic_base_url (/v1/messages) — and AuthHeaders sets Bearer AND x-api-key
// on every request regardless of the leg (the OpenAI endpoint ignores
// x-api-key; the Anthropic-compatible endpoint reads it). Per
// docs/engineering/testing.md: both schemes asserted on both protocol paths,
// exact values — deleting either Header.Set must fail a leg.
func TestVolcengineAuthHeaders_BothSchemes(t *testing.T) {
	p := newTestVolcengine(t)
	if err := p.SaveKey("ark-test-key"); err != nil {
		t.Fatalf("SaveKey: %v", err)
	}
	for _, tc := range []struct{ name, url string }{
		{"openai leg /chat/completions", "https://ark.cn-beijing.volces.com/api/plan/v3/chat/completions"},
		{"anthropic leg /v1/messages", "https://ark.cn-beijing.volces.com/api/plan/v1/messages"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest("POST", tc.url, nil)
			if err := p.AuthHeaders(req); err != nil {
				t.Fatalf("AuthHeaders: %v", err)
			}
			if got := req.Header.Get("Authorization"); got != "Bearer ark-test-key" {
				t.Errorf("Authorization: got %q, want %q", got, "Bearer ark-test-key")
			}
			if got := req.Header.Get("x-api-key"); got != "ark-test-key" {
				t.Errorf("x-api-key: got %q, want %q", got, "ark-test-key")
			}
		})
	}
}

// TestValidateVolcengineAKSK exercises the AK/SK validation wrapper's full
// round-trip (sign + GET + status check) against a mock Volcengine OpenAPI by
// pointing volcengineOpenAPIBase at an httptest server. nil on 200 + parseable
// AFP result; error on non-200 AND on 200 + ResponseMetadata.Error (the
// Volcengine OpenAPI business-error shape). The V4 signing itself is covered by
// volcengine_sign_test.go — here it only needs to produce a request the mock answers.
func TestValidateVolcengineAKSK(t *testing.T) {
	orig := volcengineOpenAPIBase
	defer func() { volcengineOpenAPIBase = orig }()

	// 200 + a parseable AFP body → nil (the pair signs + the control plane accepts).
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ResponseMetadata":{},"Result":{"PlanType":"agent","AFPMonthly":{"Quota":100,"Used":1,"ResetTime":0}}}`))
	}))
	defer ok.Close()
	volcengineOpenAPIBase = ok.URL
	if err := ValidateVolcengineAKSK("AKtest", "SKtest"); err != nil {
		t.Errorf("200 + AFP body: want nil, got %v", err)
	}

	// Non-200 → error (the pair is rejected).
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		w.Write([]byte(`{"ResponseMetadata":{"Error":{"Code":"InvalidAccessKey","Message":"bad AK"}}}`))
	}))
	defer bad.Close()
	volcengineOpenAPIBase = bad.URL
	if err := ValidateVolcengineAKSK("AKtest", "SKtest"); err == nil {
		t.Error("401: want error, got nil")
	}

	// 200 + ResponseMetadata.Error → business error (Volcengine OpenAPI
	// reports failures as HTTP 200 with an Error payload). Must surface the
	// Code/Message, never the SecretKey.
	bizErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ResponseMetadata":{"RequestId":"req-1","Error":{"Code":"InvalidAccessKey","Message":"ak/sk rejected"}},"Result":{"PlanType":"agent"}}`))
	}))
	defer bizErr.Close()
	volcengineOpenAPIBase = bizErr.URL
	err := ValidateVolcengineAKSK("AKtest", "SKtest-supersecret")
	if err == nil {
		t.Fatal("200 + metadata.Error: want error, got nil")
	}
	for _, want := range []string{"GetAFPUsage", "InvalidAccessKey", "ak/sk rejected"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("200 + metadata.Error: error %q missing %q", err.Error(), want)
		}
	}
	if strings.Contains(err.Error(), "SKtest-supersecret") {
		t.Errorf("200 + metadata.Error: error leaks SecretKey: %q", err.Error())
	}
}

// probeModelCallable selects the anthropic base when anthropic_base_url is set, so
// volcengine is probed over the ANTHROPIC protocol. Its ProbeRequest must return
// the anthropic /v1/messages path + body (not baseProbe's OpenAI /chat/completions,
// which would hit .../api/plan/chat/completions and 404 for every model).
func TestVolcengineProbeRequest_AnthropicShape(t *testing.T) {
	p := newTestVolcengine(t)
	pr := p.ProbeRequest("doubao-seed-2.0-pro")
	if pr.Method != http.MethodPost || pr.Path != "/v1/messages" {
		t.Errorf("ProbeRequest = %+v, want POST /v1/messages (anthropic; probe uses the anthropic base)", pr)
	}
	if !strings.Contains(string(pr.Body), `"messages"`) || strings.Contains(string(pr.Body), `"stream"`) {
		t.Errorf("ProbeRequest.Body not the anthropic messages shape: %s", pr.Body)
	}
}

// The anthropic-compatible endpoint requires anthropic-version; the probe has no
// client request to inherit it from, so the provider must set it via ExtraHeaders.
func TestVolcengineExtraHeaders_AnthropicVersion(t *testing.T) {
	p := newTestVolcengine(t)
	req := httptest.NewRequest(http.MethodPost, "https://ark.cn-beijing.volces.com/api/plan/v1/messages", nil)
	p.ExtraHeaders(req, nil, "", "/v1/messages")
	if got := req.Header.Get("anthropic-version"); got != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want 2023-06-01", got)
	}
}
