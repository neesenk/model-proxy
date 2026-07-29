// Package targetexec owns the immutable, target-specific wire preparation
// shared by normal routing, Fusion, and Shadow execution.
package targetexec

import (
	"encoding/json"

	"model-proxy/internal/protocol"
	"model-proxy/provider"
)

// PlanInput contains the already-resolved target facts needed to prepare a
// request for one upstream. Provider/config resolution deliberately remains in
// the composition root.
type PlanInput struct {
	TargetModel      string
	ProviderID       string
	ClientProtocol   protocol.Protocol
	BackendProtocol  protocol.Protocol
	OpenAIBaseURL    string
	AnthropicBaseURL string
	ClientPath       string
	ImageOK          bool
}

// Plan is an immutable wire plan for one resolved target.
type Plan struct {
	targetModel     string
	clientProtocol  protocol.Protocol
	backendProtocol protocol.Protocol
	baseURL         string
	upstreamPath    string
	providerID      string
	imageOK         bool
}

// NewPlan derives the upstream endpoint and retains the facts needed for
// request/response conversion.
func NewPlan(input PlanInput) Plan {
	baseURL := input.OpenAIBaseURL
	if input.BackendProtocol == protocol.Anthropic && input.AnthropicBaseURL != "" {
		baseURL = input.AnthropicBaseURL
	}
	upstreamPath := input.ClientPath
	if protocol.NeedsConversion(input.ClientProtocol, input.BackendProtocol) {
		upstreamPath = protocol.BackendPath(input.BackendProtocol)
	}
	return Plan{
		targetModel:     input.TargetModel,
		clientProtocol:  input.ClientProtocol,
		backendProtocol: input.BackendProtocol,
		baseURL:         baseURL,
		upstreamPath:    upstreamPath,
		providerID:      input.ProviderID,
		imageOK:         input.ImageOK,
	}
}

func (plan Plan) BaseURL() string                    { return plan.baseURL }
func (plan Plan) UpstreamPath() string               { return plan.upstreamPath }
func (plan Plan) ClientProtocol() protocol.Protocol  { return plan.clientProtocol }
func (plan Plan) BackendProtocol() protocol.Protocol { return plan.backendProtocol }

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
	return protocol.ConvertRequestWithOptions(body, plan.clientProtocol, plan.backendProtocol, protocol.RequestOptions{
		ImageOK:          plan.imageOK,
		ReasoningDialect: protocol.ReasoningDialect(provider.ChatReasoningMode(plan.providerID)),
		CodexShaping:     plan.providerID == "codex",
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
