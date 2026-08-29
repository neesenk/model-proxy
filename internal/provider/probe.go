package provider

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// probe.go provides the default implementations of ProbeRequest / ExtraHeaders /
// FilterModelIDs via baseProbe, which every provider embeds. This keeps each
// provider's probe/filter special knowledge inside its own file (override) or in
// the shared default here - never in `if prov.Provider == ...` branches of the
// internal/app composition root.

// baseProbe is the default implementation of ProbeRequest / ExtraHeaders /
// FilterModelIDs. Embed it in a provider to get OpenAI-style defaults; override
// the methods that differ.
//
// Defaults:
//   - ProbeRequest: POST /chat/completions + openAIProbeBody (minimal OpenAI chat)
//   - ExtraHeaders: no-op
//   - FilterModelIDs: passthrough (no static drops)
type baseProbe struct{}

// ProbeRequest returns the default OpenAI-style probe: POST /chat/completions
// with a minimal non-streaming chat body. Providers that speak a different chat
// shape (codex /responses, aqp /v1/messages) override this.
func (baseProbe) ProbeRequest(modelID string) ProbeRequest {
	return ProbeRequest{
		Method: http.MethodPost,
		Path:   "/chat/completions",
		Body:   openAIProbeBody(modelID),
	}
}

// ExtraHeaders is a no-op by default. Providers that need per-request headers
// (aqp: anthropic-version + x-compass-request-id) override it.
func (baseProbe) ExtraHeaders(req *http.Request, path string) {}

// FilterModelIDs passes the list through unchanged by default. Providers with
// static policy rules (volcengine: drop *-latest / lite / mini) override it.
func (baseProbe) FilterModelIDs(ids []string) (kept, dropped []string) {
	return ids, nil
}

// newRequestID returns a random UUID v4 string (lowercase hex). Used for aqp's
// per-request x-compass-request-id. Pure crypto/rand + fmt - no main-package
// dependency, so it lives here in the provider package.
func newRequestID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// openAIProbeBody is a minimal non-streaming OpenAI chat request.
func openAIProbeBody(model string) []byte {
	b, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens": 1,
		"stream":     false,
	})
	return b
}

// anthropicProbeBody is a minimal Anthropic messages request (no
// anthropic-version in body - it's a header, set by the provider's ExtraHeaders).
func anthropicProbeBody(model string) []byte {
	b, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
	})
	return b
}

// codexProbeBody is a minimal codex /responses request. The codex backend
// speaks the OpenAI Responses API shape: the user turn goes in `input`, NOT
// `messages` (rejected with "Unsupported parameter: messages"), and `input`
// MUST be a list (a bare string is rejected with "Input must be a list"). It
// also requires stream:true and rejects max_tokens; store:false is injected by
// CodexProvider.RewriteRequest.
func codexProbeBody(model string) []byte {
	b, _ := json.Marshal(map[string]any{
		"model":  model,
		"input":  []map[string]string{{"role": "user", "content": "hi"}},
		"stream": true,
	})
	return b
}
