// Package protocol implements request, response, error, and streaming
// conversion among Anthropic Messages, OpenAI Chat Completions, and OpenAI
// Responses wire protocols.
//
// Provider selection, HTTP transport, routing, and response writing remain in
// the composition root. Target-specific behavior is injected through
// RequestOptions.
package protocol
