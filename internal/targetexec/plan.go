// Package targetexec owns the immutable, target-specific wire preparation
// shared by normal routing, Fusion, and Shadow execution.
package targetexec

import (
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
	}
}

func (plan Plan) Target() configdomain.RouteTarget   { return plan.target }
func (plan Plan) ProviderID() string                 { return plan.providerID }
func (plan Plan) Provider() provider.Provider        { return plan.provider }
func (plan Plan) BaseURL() string                    { return plan.baseURL }
func (plan Plan) UpstreamPath() string               { return plan.upstreamPath }
func (plan Plan) ClientProtocol() protocol.Protocol  { return plan.clientProtocol }
func (plan Plan) BackendProtocol() protocol.Protocol { return plan.backendProtocol }
func (plan Plan) ViaResponsesVerdict() bool          { return plan.viaResponsesVerdict }

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

// ConvertBody converts only cross-protocol traffic. Same-protocol bytes,
// including native Responses traffic to Codex, pass through unchanged.
func (plan Plan) ConvertBody(body []byte) ([]byte, error) {
	if !protocol.NeedsConversion(plan.clientProtocol, plan.backendProtocol) {
		return body, nil
	}
	providerID := plan.providerID
	return protocol.ConvertRequestWithOptions(body, plan.clientProtocol, plan.backendProtocol, protocol.RequestOptions{
		ImageOK:          plan.imageOK,
		ReasoningDialect: protocol.ReasoningDialect(provider.ChatReasoningMode(providerID)),
		CodexShaping:     providerID == "codex",
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
