package provider

import (
	"net/http"
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
