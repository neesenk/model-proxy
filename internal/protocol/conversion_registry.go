package protocol

import "io"

// wireProtocol is the closed set of client/upstream wire contracts understood
// by the proxy. Keeping protocol identity typed prevents direction strings from
// being rebuilt independently by request, response, and streaming entrypoints.
type wireProtocol string

const (
	protocolAnthropic wireProtocol = "anthropic"
	protocolOpenAI    wireProtocol = "openai"
	protocolResponses wireProtocol = "responses"
)

var supportedWireProtocols = [...]wireProtocol{
	protocolAnthropic,
	protocolOpenAI,
	protocolResponses,
}

func parseWireProtocol(value string) (wireProtocol, bool) {
	protocol := wireProtocol(value)
	switch protocol {
	case protocolAnthropic, protocolOpenAI, protocolResponses:
		return protocol, true
	default:
		return "", false
	}
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
