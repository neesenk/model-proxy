package provider

import (
	"net/http"
	"path/filepath"
	"testing"
)

// newTestDeepSeek builds a DeepSeekProvider with a temp auth file so AuthHeaders
// tests don't touch the real ~/.model-proxy store.
func newTestDeepSeek(t *testing.T) *DeepSeekProvider {
	t.Helper()
	return &DeepSeekProvider{
		ApiKeyBase: &ApiKeyBase{authFile: filepath.Join(t.TempDir(), "deepseek_apikey.json")},
		cfg:        &Config{OpenAIBaseURL: "https://api.deepseek.com"},
	}
}

// RewriteRequest is a no-op: the proxy selects the upstream base URL by protocol
// (anthropic_base_url for /messages, openai_base_url otherwise), so
// DeepSeek must not rewrite the URL itself.
func TestDeepSeekRewriteRequest_NoOp(t *testing.T) {
	p := newTestDeepSeek(t)
	for _, tc := range []struct{ url, path string }{
		{"https://api.deepseek.com/anthropic/v1/messages", "/messages"},
		{"https://api.deepseek.com/anthropic/v1/messages?stream=true", "/messages"},
		{"https://api.deepseek.com/chat/completions", "/chat/completions"},
	} {
		if got, _ := p.RewriteRequest(tc.url, nil, tc.path); got != tc.url {
			t.Errorf("RewriteRequest(%q, %q): got %q, want unchanged", tc.url, tc.path, got)
		}
	}
}

// One key must authenticate both endpoints: Bearer for OpenAI, x-api-key for Anthropic.
func TestDeepSeekAuthHeaders_BothSchemes(t *testing.T) {
	p := newTestDeepSeek(t)
	if err := p.SaveKey("sk-test-123"); err != nil {
		t.Fatalf("SaveKey: %v", err)
	}
	req, _ := http.NewRequest("POST", "https://api.deepseek.com/chat/completions", nil)
	if err := p.AuthHeaders(req); err != nil {
		t.Fatalf("AuthHeaders: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer sk-test-123" {
		t.Errorf("Authorization: got %q, want %q", got, "Bearer sk-test-123")
	}
	if got := req.Header.Get("x-api-key"); got != "sk-test-123" {
		t.Errorf("x-api-key: got %q, want %q", got, "sk-test-123")
	}
}
