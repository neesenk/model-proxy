package provider

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
		{"https://ark.cn-beijing.volces.com/api/plan/compatible/v1/messages", "/messages"},
		{"https://ark.cn-beijing.volces.com/api/plan/v3/chat/completions", "/chat/completions"},
	} {
		if got, _ := p.RewriteRequest(tc.url, nil, tc.path); got != tc.url {
			t.Errorf("RewriteRequest(%q): got %q, want unchanged", tc.url, got)
		}
	}
}

// One key must authenticate both endpoints: Bearer for OpenAI, x-api-key for the
// Anthropic-compatible endpoint.
func TestVolcengineAuthHeaders_BothSchemes(t *testing.T) {
	p := newTestVolcengine(t)
	if err := p.SaveKey("ark-test-key"); err != nil {
		t.Fatalf("SaveKey: %v", err)
	}
	req, _ := http.NewRequest("POST", "https://ark.cn-beijing.volces.com/api/plan/v3/chat/completions", nil)
	if err := p.AuthHeaders(req); err != nil {
		t.Fatalf("AuthHeaders: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer ark-test-key" {
		t.Errorf("Authorization: got %q, want Bearer ark-test-key", got)
	}
	if got := req.Header.Get("x-api-key"); got != "ark-test-key" {
		t.Errorf("x-api-key: got %q, want ark-test-key", got)
	}
}

// TestValidateVolcengineAKSK exercises the AK/SK validation wrapper's full
// round-trip (sign + GET + status check) against a mock Volcengine OpenAPI by
// pointing volcengineOpenAPIBase at an httptest server. nil on 200 + parseable
// AFP result; error on non-200. The V4 signing itself is covered by
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
}
