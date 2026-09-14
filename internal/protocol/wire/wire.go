// Package wire owns the closed set of wire-protocol identities understood by
// the proxy ("anthropic", "openai", "responses"). It is a leaf package so
// configuration validation (internal/config) can share the single source of
// truth without depending on the conversion machinery in internal/protocol,
// which re-exports these names.
package wire

// Protocol identifies one supported wire protocol.
type Protocol string

const (
	Anthropic Protocol = "anthropic"
	OpenAI    Protocol = "openai"
	Responses Protocol = "responses"
)

// Parse validates a wire protocol name against the closed set.
func Parse(value string) (Protocol, bool) {
	switch p := Protocol(value); p {
	case Anthropic, OpenAI, Responses:
		return p, true
	default:
		return "", false
	}
}
