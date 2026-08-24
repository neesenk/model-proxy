package protocol

import (
	"encoding/json"
	"io"
	"strings"
)

// Protocol identifies one supported wire protocol.
type Protocol string

const (
	Anthropic Protocol = "anthropic"
	OpenAI    Protocol = "openai"
	Responses Protocol = "responses"
)

// Parse validates a wire protocol name.
func Parse(value string) (Protocol, bool) {
	proto, ok := parseWireProtocol(value)
	return Protocol(proto), ok
}

// ForPath identifies the protocol implied by an inbound API path.
func ForPath(path string) Protocol {
	switch {
	case strings.HasPrefix(path, "/v1/messages"):
		return Anthropic
	case strings.HasPrefix(path, "/v1/chat/completions"):
		return OpenAI
	case strings.HasPrefix(path, "/v1/responses"):
		return Responses
	default:
		return ""
	}
}

// NeedsConversion reports whether two known, distinct protocols need a codec.
func NeedsConversion(client, backend Protocol) bool {
	return needsConversion(string(client), string(backend))
}

// BackendPath returns the upstream path for a backend protocol.
func BackendPath(backend Protocol) string {
	return backendPath(string(backend))
}

// ReasoningDialect selects the target Chat API's reasoning-effort shape.
type ReasoningDialect string

const (
	ReasoningEffort         ReasoningDialect = "reasoning_effort"
	ReasoningThinking       ReasoningDialect = "thinking"
	ReasoningEnableThinking ReasoningDialect = "enable_thinking"
	ReasoningOpenRouter     ReasoningDialect = "openrouter"
)

// RequestOptions carries target-specific conversion behavior supplied by the
// transport. The protocol package deliberately has no provider dependency.
type RequestOptions struct {
	ImageOK          bool
	ReasoningDialect ReasoningDialect
	CodexShaping     bool
	// Diag, when set, collects the conversion's structured diagnostics
	// (lossy-but-degraded mappings with stable codes). See diagnostics.go.
	Diag *Diagnostics
	// StrictLossy refuses the conversion when any lossy diagnostic fires —
	// the target is skipped (capability-scanner semantics) instead of
	// silently degrading request content.
	StrictLossy bool
}

type convertReqOpts = RequestOptions

// ConvertRequest converts a request with the permissive default image policy.
func ConvertRequest(body []byte, client, backend Protocol) ([]byte, error) {
	return convertRequestFor(body, string(client), string(backend), RequestOptions{ImageOK: true})
}

// ConvertRequestWithOptions converts a request using target-specific options.
func ConvertRequestWithOptions(body []byte, client, backend Protocol, opts RequestOptions) ([]byte, error) {
	return convertRequestFor(body, string(client), string(backend), opts)
}

// ResponseContext is opaque request-derived context needed to restore
// Responses namespace and custom-tool shapes in the reverse direction.
type ResponseContext struct {
	value r2cCtx
}

// NewResponseContext derives reverse-conversion context from the original
// client request.
func NewResponseContext(client, backend Protocol, originalBody []byte) ResponseContext {
	return ResponseContext{value: r2cCtxFor(string(client), string(backend), originalBody)}
}

// ConvertResponse converts a non-streaming backend response to the client
// protocol.
func ConvertResponse(body []byte, client, backend Protocol, context ResponseContext) ([]byte, error) {
	return convertResponseNS(body, string(client), string(backend), context.value)
}

// ConvertSSE wraps a backend SSE reader with the selected reverse converter.
func ConvertSSE(reader io.Reader, client, backend Protocol, model string, context ResponseContext) io.Reader {
	return convertSSEReaderNS(reader, string(client), string(backend), model, context.value)
}

// ConvertErrorResponse converts an upstream HTTP error envelope to the client
// protocol.
func ConvertErrorResponse(body []byte, client, backend Protocol, status int) ([]byte, error) {
	return convertErrorResponse(body, string(client), string(backend), status)
}

// WantsStream reports whether a request body asks for streaming output.
func WantsStream(body []byte) bool {
	return requestWantsStream(body)
}

// AggregateSSE folds a complete captured SSE stream into a JSON response.
func AggregateSSE(raw []byte, proto Protocol) ([]byte, error) {
	return aggregateSSEToResponse(raw, string(proto))
}

// ResponseToSSE expands a non-streaming JSON response into its protocol's SSE
// event sequence.
func ResponseToSSE(body []byte, proto Protocol) ([]byte, error) {
	return responseToSSE(body, string(proto))
}

// ShrinkImages bounds inline image data URLs in a request body.
func ShrinkImages(body []byte, maxBytes, maxDimension int) ([]byte, bool) {
	return shrinkRequestImages(body, maxBytes, maxDimension)
}

// ExtractResponseText returns assistant-visible text from a non-streaming
// response. It preserves Fusion's pre-extraction shape acceptance: Responses
// has its own output-item walk; the other protocols share the legacy
// Anthropic-content-then-Chat-string fallback.
func ExtractResponseText(body []byte, proto Protocol) string {
	if proto == Responses {
		var root map[string]any
		if json.Unmarshal(body, &root) != nil {
			return ""
		}
		var out strings.Builder
		for _, item := range responsesOutputItems(root) {
			if strOpt(item["type"]) != "message" {
				continue
			}
			for _, raw := range anySlice(item["content"]) {
				part := asMap(raw)
				switch strOpt(part["type"]) {
				case "output_text", "text":
					out.WriteString(responsesTextWithCitationLinks(part))
				case "refusal":
					out.WriteString(strOpt(part["refusal"]))
				}
			}
		}
		return out.String()
	}

	var root struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Choices []struct {
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if json.Unmarshal(body, &root) != nil {
		return ""
	}
	var out strings.Builder
	for _, block := range root.Content {
		if block.Type == "text" {
			out.WriteString(block.Text)
		}
	}
	if out.Len() > 0 {
		return out.String()
	}
	if len(root.Choices) > 0 {
		var text string
		if json.Unmarshal(root.Choices[0].Message.Content, &text) == nil {
			return text
		}
	}
	return ""
}

// UnsupportedError describes a request feature that cannot be represented by
// a target protocol without semantic loss.
type UnsupportedError = unsupportedConversionError

// AsUnsupported extracts an unsupported-conversion error.
func AsUnsupported(err error) (*UnsupportedError, bool) {
	return asUnsupportedConversion(err)
}

// ResponsesStateStore is the bounded Responses previous_response_id store.
type ResponsesStateStore = responsesStateStore

// ResponsesStateCaptureLimit bounds a captured response body before state
// extraction.
const ResponsesStateCaptureLimit = 2 << 20

// NewResponsesStateStore opens a Responses continuation store.
func NewResponsesStateStore(path string) *ResponsesStateStore {
	return newResponsesStateStore(path)
}

// ResponsesStatePath derives the state path from the quota-state path.
func ResponsesStatePath(quotaPath string) string {
	return responsesStatePath(quotaPath)
}

// PreviousResponseID extracts previous_response_id from a Responses request.
func PreviousResponseID(body []byte) string {
	return responsesPreviousID(body)
}

// Close flushes and stops the store.
func (s *responsesStateStore) Close() {
	s.close()
}

// Expand resolves previous_response_id and returns the complete stateless
// request body, request history, and cache-hit status.
func (s *responsesStateStore) Expand(body []byte, session string) ([]byte, []any, bool, error) {
	return s.expand(body, session)
}

// RecordJSON records a non-streaming Responses result for continuation.
func (s *responsesStateStore) RecordJSON(session string, requestHistory []any, body []byte) bool {
	return s.recordJSON(session, requestHistory, body)
}

// RecordSSE records a captured Responses SSE result for continuation.
func (s *responsesStateStore) RecordSSE(session string, requestHistory []any, body []byte) bool {
	return s.recordSSE(session, requestHistory, body)
}
