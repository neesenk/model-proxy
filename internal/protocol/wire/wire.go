// Package wire owns the closed set of wire-protocol identities understood by
// the proxy ("anthropic", "openai", "responses", "decisions"). It is a leaf
// package so configuration validation (internal/config) can share the single
// source of truth without depending on the conversion machinery in
// internal/protocol, which re-exports these names.
package wire

// Protocol identifies one supported wire protocol.
type Protocol string

const (
	Anthropic Protocol = "anthropic"
	OpenAI    Protocol = "openai"
	Responses Protocol = "responses"
	// Decisions is the System One decisions protocol ({model, state, questions}
	// in, typed answers out — no text, no streaming, no tools). It is
	// deliberately unrelated to the chat protocol family: cross-protocol
	// conversion with decisions is registered as fail-closed stubs.
	Decisions Protocol = "decisions"
)

// Parse validates a wire protocol name against the closed set.
func Parse(value string) (Protocol, bool) {
	switch p := Protocol(value); p {
	case Anthropic, OpenAI, Responses, Decisions:
		return p, true
	default:
		return "", false
	}
}
