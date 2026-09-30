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
//   - ProbeRequest: POST /chat/completions + OpenAIProbeBody (minimal OpenAI chat)
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
		Body:   OpenAIProbeBody(modelID),
	}
}

// ExtraHeaders is a no-op by default. Providers that need per-request headers
// (aqp: anthropic-version + x-compass-request-id) override it.
func (baseProbe) ExtraHeaders(req *http.Request, _ []byte, _ string, path string) {}

// anthropicMessagesProbe is baseProbe with the ANTHROPIC dialect swapped in:
// probe POST /v1/messages + AnthropicProbeBody, and anthropic-version set on
// every upstream request (forward + probe — the probe has no client request to
// inherit the header from, and anthropic-compatible endpoints reject requests
// without it; harmless on the OpenAI path). Embed it instead of baseProbe when
// anthropic_base_url is the provider's primary path for Claude Code: the probe
// MUST go to /v1/messages there because OpenAI's /chat/completions path on the
// anthropic base 404s for every model. Providers whose ExtraHeaders does MORE
// (aqp: + x-compass-request-id; zcode: + client fingerprint) keep baseProbe
// and their own overrides.
type anthropicMessagesProbe struct{ baseProbe }

// ProbeRequest returns the anthropic messages shape, overriding baseProbe's
// OpenAI /chat/completions default (see the type doc for why).
func (anthropicMessagesProbe) ProbeRequest(modelID string) ProbeRequest {
	return ProbeRequest{
		Method: http.MethodPost,
		Path:   "/v1/messages",
		Body:   AnthropicProbeBody(modelID),
	}
}

// ExtraHeaders sets anthropic-version on every upstream request.
func (anthropicMessagesProbe) ExtraHeaders(req *http.Request, _ []byte, _ string, path string) {
	req.Header.Set("anthropic-version", "2023-06-01")
}

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

// OpenAIProbeBody is a minimal non-streaming OpenAI chat request.
func OpenAIProbeBody(model string) []byte {
	b, _ := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens": 1,
		"stream":     false,
	})
	return b
}

// AnthropicProbeBody is a minimal Anthropic messages request (no
// anthropic-version in body - it's a header, set by the provider's ExtraHeaders).
func AnthropicProbeBody(model string) []byte {
	b, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 1,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
	})
	return b
}

// ResponsesProbeBody is a minimal OpenAI Responses API request in the GENERIC
// shape (string input, no streaming). Providers whose responses endpoint
// demands a dialect (codex: input list + stream:true) keep their own shape in
// their ProbeRequest override, which the probe layer prefers over this one.
func ResponsesProbeBody(model string) []byte {
	b, _ := json.Marshal(map[string]any{
		"model":             model,
		"input":             "hi",
		"max_output_tokens": 16,
		"store":             false,
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
