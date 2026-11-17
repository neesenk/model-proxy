package provider

import (
	"bytes"
	"encoding/json"
	"testing"
)

// Regression (observed live 2026-11-16, 2×): a load_balance route fan-out
// landed an openai client request carrying max_tokens 262144 on the zcode
// (BigModel anthropic) leg, which answered a terminal 400 [1210]
// "max_tokens参数非法：限制数值范围[1,131072]" — committed straight to the
// client with no failover, even though the same body was valid on the route's
// other targets. The clamp is provider-owned knowledge: zcode lowers an
// over-limit max_tokens to its backend cap instead of letting a
// target-relative limit break the whole route.

func TestZCodeRewriteRequestClampsMaxTokens(t *testing.T) {
	p := &ZCodeProvider{}
	url := "https://zcode.z.ai/api/v1/ultra/anthropic/v1/messages"
	body := []byte(`{"model":"glm-5.3-flash","max_tokens":262144,"messages":[{"role":"user","content":"hi"}]}`)
	_, out := p.RewriteRequest(url, body, "/v1/messages")

	var parsed struct {
		Model     string          `json:"model"`
		MaxTokens json.RawMessage `json:"max_tokens"`
		Messages  json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatalf("clamped body is not JSON: %v\n%s", err, out)
	}
	var maxTokens int
	if err := json.Unmarshal(parsed.MaxTokens, &maxTokens); err != nil {
		t.Fatalf("max_tokens did not stay an integer: %v", err)
	}
	if maxTokens != 131072 {
		t.Fatalf("max_tokens = %d, want clamped to the zcode cap 131072", maxTokens)
	}
	if parsed.Model != "glm-5.3-flash" {
		t.Fatalf("model field altered: %s", parsed.Model)
	}
	if string(parsed.Messages) == "" {
		t.Fatal("messages field dropped")
	}
}

func TestZCodeRewriteRequestPassthroughWithinCap(t *testing.T) {
	p := &ZCodeProvider{}
	url := "https://zcode.z.ai/api/v1/ultra/anthropic/v1/messages"
	within := []byte(`{"model":"glm-5.3-flash","max_tokens":4096,"messages":[]}`)
	if _, out := p.RewriteRequest(url, within, "/v1/messages"); !bytes.Equal(out, within) {
		t.Fatalf("in-range body must stay byte-identical:\n%s", out)
	}
	absent := []byte(`{"model":"glm-5.3-flash","messages":[]}`)
	if _, out := p.RewriteRequest(url, absent, "/v1/messages"); !bytes.Equal(out, absent) {
		t.Fatalf("body without max_tokens must stay byte-identical:\n%s", out)
	}
}
