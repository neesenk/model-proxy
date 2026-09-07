// Package targetexec owns the immutable, target-specific wire preparation
// shared by normal routing, Fusion, and Shadow execution.
package targetexec

import (
	"bytes"
	"encoding/json"
	"net/http"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/protocol"
	"model-proxy/internal/provider"
)

// PlanInput contains the already-resolved target facts needed to prepare a
// request for one upstream. Provider/config resolution deliberately remains in
// the composition root.
type PlanInput struct {
	Target              configdomain.RouteTarget
	ProviderConfig      configdomain.Provider
	Provider            provider.Provider
	ClientProtocol      protocol.Protocol
	BackendProtocol     protocol.Protocol
	ViaResponsesVerdict bool
	ClientPath          string
	ImageOK             bool
	Diag                *protocol.Diagnostics
	StrictLossy         bool
}

// Plan is an immutable wire plan for one resolved target.
type Plan struct {
	target              configdomain.RouteTarget
	provider            provider.Provider
	providerID          string
	configuredHeaders   map[string]string
	targetModel         string
	clientProtocol      protocol.Protocol
	backendProtocol     protocol.Protocol
	viaResponsesVerdict bool
	baseURL             string
	upstreamPath        string
	imageOK             bool
	// diag collects the request conversion's structured diagnostics; strict
	// refuses lossy conversions (typed error → the forward path skips this
	// target, capability-scanner semantics). Both come from application
	// config via PlanInput.
	diag        *protocol.Diagnostics
	strictLossy bool
}

// NewPlan derives the upstream endpoint and retains the facts needed for
// request/response conversion.
func NewPlan(input PlanInput) Plan {
	baseURL := input.ProviderConfig.OpenAIBaseURL
	if input.BackendProtocol == protocol.Anthropic && input.ProviderConfig.AnthropicBaseURL != "" {
		baseURL = input.ProviderConfig.AnthropicBaseURL
	}
	upstreamPath := input.ClientPath
	if protocol.NeedsConversion(input.ClientProtocol, input.BackendProtocol) {
		upstreamPath = protocol.BackendPath(input.BackendProtocol)
	}
	headers := make(map[string]string, len(input.ProviderConfig.Headers))
	for key, value := range input.ProviderConfig.Headers {
		headers[key] = value
	}
	return Plan{
		target:              input.Target,
		provider:            input.Provider,
		providerID:          input.ProviderConfig.Provider,
		configuredHeaders:   headers,
		targetModel:         input.Target.Model,
		clientProtocol:      input.ClientProtocol,
		backendProtocol:     input.BackendProtocol,
		viaResponsesVerdict: input.ViaResponsesVerdict,
		baseURL:             baseURL,
		upstreamPath:        upstreamPath,
		imageOK:             input.ImageOK,
		diag:                input.Diag,
		strictLossy:         input.StrictLossy,
	}
}

func (plan Plan) Target() configdomain.RouteTarget { return plan.target }
func (plan Plan) ProviderID() string               { return plan.providerID }

// ConversionDiag exposes the plan's diagnostics collector (nil when the
// application did not enable diagnostic collection).
func (plan Plan) ConversionDiag() *protocol.Diagnostics { return plan.diag }
func (plan Plan) Provider() provider.Provider           { return plan.provider }
func (plan Plan) BaseURL() string                       { return plan.baseURL }
func (plan Plan) UpstreamPath() string                  { return plan.upstreamPath }
func (plan Plan) ClientProtocol() protocol.Protocol     { return plan.clientProtocol }
func (plan Plan) BackendProtocol() protocol.Protocol    { return plan.backendProtocol }
func (plan Plan) ViaResponsesVerdict() bool             { return plan.viaResponsesVerdict }

// ApplyConfiguredHeaders copies static target headers without exposing the
// generation-owned provider config map for mutation.
func (plan Plan) ApplyConfiguredHeaders(header http.Header) {
	for key, value := range plan.configuredHeaders {
		header.Set(key, value)
	}
}

// RewriteModel preserves the called model when a target has no replacement
// model, as Shadow targets may do.
func (plan Plan) RewriteModel(body []byte, calledModel string) []byte {
	if plan.targetModel == "" || plan.targetModel == calledModel {
		return body
	}
	// Fast path: model is the first top-level key in every known LLM client
	// (same assumption as protocol.ExtractModel), so the value can be spliced
	// in place — no full unmarshal/marshal of a body that is 99% unrelated
	// messages/tools content.
	if out, ok := spliceModelValue(body, plan.targetModel); ok {
		return out
	}
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		return body
	}
	value["model"] = plan.targetModel
	out, err := json.Marshal(value)
	if err != nil {
		return body
	}
	return out
}

// spliceModelValue replaces the top-level "model" value in body when it is
// the FIRST key and a string, returning the spliced bytes. It reports
// ok=false for any other layout (model absent, not first, non-string value,
// malformed JSON) so the caller falls back to a full parse.
func spliceModelValue(body []byte, targetModel string) (out []byte, ok bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, false
	}
	keyTok, err := dec.Token()
	if err != nil {
		return nil, false
	}
	if key, _ := keyTok.(string); key != "model" {
		return nil, false
	}
	// dec.InputOffset() is now just past the key's closing quote; the value
	// literal starts after the colon and any whitespace.
	valueStart := int(dec.InputOffset())
	for valueStart < len(body) && isJSONSpace(body[valueStart]) {
		valueStart++
	}
	if valueStart >= len(body) || body[valueStart] != ':' {
		return nil, false
	}
	valueStart++
	for valueStart < len(body) && isJSONSpace(body[valueStart]) {
		valueStart++
	}
	if valueStart >= len(body) || body[valueStart] != '"' {
		return nil, false
	}
	valTok, err := dec.Token()
	if err != nil {
		return nil, false
	}
	if _, isString := valTok.(string); !isString {
		return nil, false
	}
	valueEnd := int(dec.InputOffset())
	encoded, err := json.Marshal(targetModel)
	if err != nil {
		return nil, false
	}
	out = make([]byte, 0, len(body)-(valueEnd-valueStart)+len(encoded))
	out = append(out, body[:valueStart]...)
	out = append(out, encoded...)
	out = append(out, body[valueEnd:]...)
	return out, true
}

func isJSONSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// ConvertBody converts only cross-protocol traffic. Same-protocol bytes,
// including native Responses traffic to Codex, pass through unchanged.
func (plan Plan) ConvertBody(body []byte) ([]byte, error) {
	if !protocol.NeedsConversion(plan.clientProtocol, plan.backendProtocol) {
		return body, nil
	}
	providerID := plan.providerID
	effortProfile := provider.ChatEffortProfile(providerID, plan.targetModel)
	return protocol.ConvertRequestWithOptions(body, plan.clientProtocol, plan.backendProtocol, protocol.RequestOptions{
		ImageOK:             plan.imageOK,
		ReasoningDialect:    protocol.ReasoningDialect(provider.ChatReasoningMode(providerID)),
		ReasoningEffortEnum: effortProfile.Enum,
		ReasoningEffortOnly: effortProfile.EnumOnly,
		CodexShaping:        providerID == "codex",
		Diag:                plan.diag,
		StrictLossy:         plan.strictLossy,
	})
}

// ResponseContext derives opaque reverse-conversion state from the original
// client request.
func (plan Plan) ResponseContext(origBody []byte) protocol.ResponseContext {
	return protocol.NewResponseContext(plan.clientProtocol, plan.backendProtocol, origBody)
}

// ExtractResponseText returns assistant-visible non-streaming response text.
func (plan Plan) ExtractResponseText(body []byte) string {
	return protocol.ExtractResponseText(body, plan.backendProtocol)
}
