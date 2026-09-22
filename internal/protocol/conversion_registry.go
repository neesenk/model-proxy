package protocol

import (
	"io"

	"model-proxy/internal/protocol/wire"
)

// wireProtocol is the closed set of client/upstream wire contracts understood
// by the proxy. The canonical definition lives in the leaf package
// internal/protocol/wire (shared with config validation); the alias keeps
// protocol identity typed so direction strings are not rebuilt independently
// by request, response, and streaming entrypoints.
type wireProtocol = wire.Protocol

const (
	protocolAnthropic = wire.Anthropic
	protocolOpenAI    = wire.OpenAI
	protocolResponses = wire.Responses
	protocolDecisions = wire.Decisions
)

var supportedWireProtocols = [...]wireProtocol{
	protocolAnthropic,
	protocolOpenAI,
	protocolResponses,
	protocolDecisions,
}

func parseWireProtocol(value string) (wireProtocol, bool) {
	return wire.Parse(value)
}

type conversionPair struct {
	client  wireProtocol
	backend wireProtocol
}

// protocolConversion defines all three reverse-aware paths for one declared
// client→backend pair:
//   - request converts client bytes to backend bytes;
//   - response converts backend JSON to client JSON;
//   - stream converts backend SSE to client SSE.
//
// Pair-specific codecs remain deliberately specialized: hosted tools,
// reasoning dialects, and namespace restoration are not safely representable
// by one lowest-common-denominator message IR.
type protocolConversion struct {
	request  func([]byte, convertReqOpts) ([]byte, error)
	response func([]byte, r2cCtx) ([]byte, error)
	stream   func(io.Reader, string, r2cCtx) io.Reader
}

var protocolConversions = map[conversionPair]protocolConversion{
	{client: protocolAnthropic, backend: protocolOpenAI}: {
		request: func(body []byte, opts convertReqOpts) ([]byte, error) {
			return convertAnthropicRequestToOpenAIV(body, opts.ImageOK, opts.Diag)
		},
		response: func(body []byte, _ r2cCtx) ([]byte, error) {
			return convertOpenAIResponseToAnthropic(body)
		},
		stream: func(reader io.Reader, model string, _ r2cCtx) io.Reader {
			return newOpenAIToAnthropicSSE(reader, model)
		},
	},
	{client: protocolOpenAI, backend: protocolAnthropic}: {
		request: func(body []byte, o convertReqOpts) ([]byte, error) {
			return convertOpenAIRequestToAnthropic(body, o.Diag)
		},
		response: func(body []byte, _ r2cCtx) ([]byte, error) {
			return convertAnthropicResponseToOpenAI(body)
		},
		stream: func(reader io.Reader, model string, _ r2cCtx) io.Reader {
			return newAnthropicToOpenAISSE(reader, model)
		},
	},
	{client: protocolAnthropic, backend: protocolResponses}: {
		request: func(body []byte, opts convertReqOpts) ([]byte, error) {
			return convertAnthropicRequestToResponsesV(body, opts.ImageOK, opts.Diag)
		},
		response: func(body []byte, _ r2cCtx) ([]byte, error) {
			return convertResponsesToAnthropic(body)
		},
		stream: func(reader io.Reader, model string, _ r2cCtx) io.Reader {
			return newResponsesToAnthropicSSE(reader, model)
		},
	},
	{client: protocolOpenAI, backend: protocolResponses}: {
		request: func(body []byte, o convertReqOpts) ([]byte, error) {
			return convertOpenAIRequestToResponses(body, o.Diag)
		},
		response: func(body []byte, _ r2cCtx) ([]byte, error) {
			return convertResponsesToOpenAI(body)
		},
		stream: func(reader io.Reader, model string, _ r2cCtx) io.Reader {
			return newResponsesToOpenAISSE(reader, model)
		},
	},
	{client: protocolResponses, backend: protocolAnthropic}: {
		request: func(body []byte, o convertReqOpts) ([]byte, error) {
			return convertResponsesRequestToAnthropic(body, o.Diag)
		},
		response: func(body []byte, context r2cCtx) ([]byte, error) {
			return convertAnthropicResponseToResponsesNS(body, context)
		},
		stream: func(reader io.Reader, model string, context r2cCtx) io.Reader {
			return newAnthropicToResponsesSSENS(reader, model, context)
		},
	},
	{client: protocolResponses, backend: protocolOpenAI}: {
		request: func(body []byte, opts convertReqOpts) ([]byte, error) {
			return convertResponsesRequestToOpenAIFor(body, opts)
		},
		response: func(body []byte, context r2cCtx) ([]byte, error) {
			return convertOpenAIResponseToResponsesNS(body, context)
		},
		stream: func(reader io.Reader, model string, context r2cCtx) io.Reader {
			return newOpenAIToResponsesSSENS(reader, model, context)
		},
	},
	// Every pair involving decisions is a registered fail-closed stub: the
	// registry's completeness contract (N×(N-1) pairs) is met, while any
	// attempted chat↔decisions conversion refuses with a typed unsupported
	// error so the forward path skips the target instead of passing a chat
	// body to a decisions endpoint (or vice versa).
	{client: protocolDecisions, backend: protocolAnthropic}: decisionsConversionRefused(protocolDecisions, protocolAnthropic),
	{client: protocolDecisions, backend: protocolOpenAI}:    decisionsConversionRefused(protocolDecisions, protocolOpenAI),
	{client: protocolDecisions, backend: protocolResponses}: decisionsConversionRefused(protocolDecisions, protocolResponses),
	{client: protocolAnthropic, backend: protocolDecisions}: decisionsConversionRefused(protocolAnthropic, protocolDecisions),
	{client: protocolOpenAI, backend: protocolDecisions}:    decisionsConversionRefused(protocolOpenAI, protocolDecisions),
	{client: protocolResponses, backend: protocolDecisions}: decisionsConversionRefused(protocolResponses, protocolDecisions),
}

// decisionsConversionRefused builds the fail-closed stub for one pair
// involving the decisions protocol: decisions carries typed state/questions,
// not chat messages, so no cross-protocol conversion can exist. Request and
// response refuse with a typed unsupported error; the stream function is
// unreachable (request conversion fails first) and only satisfies the
// registry's non-nil contract.
func decisionsConversionRefused(client, backend wireProtocol) protocolConversion {
	refuse := func() error {
		return unsupportedFeature(string(client), string(backend), "decisions_protocol",
			"the decisions protocol (System One typed questions) has no chat-protocol conversion")
	}
	return protocolConversion{
		request:  func([]byte, convertReqOpts) ([]byte, error) { return nil, refuse() },
		response: func([]byte, r2cCtx) ([]byte, error) { return nil, refuse() },
		stream:   func(r io.Reader, _ string, _ r2cCtx) io.Reader { return r },
	}
}

func lookupProtocolConversion(client, backend string) (protocolConversion, bool) {
	clientProtocol, clientOK := parseWireProtocol(client)
	backendProtocol, backendOK := parseWireProtocol(backend)
	if !clientOK || !backendOK || clientProtocol == backendProtocol {
		return protocolConversion{}, false
	}
	conversion, ok := protocolConversions[conversionPair{
		client:  clientProtocol,
		backend: backendProtocol,
	}]
	return conversion, ok
}
