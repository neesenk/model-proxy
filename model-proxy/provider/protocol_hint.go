package provider

// protocol_hint.go — provider-specific knowledge about which WIRE protocol a
// provider's models speak when it differs from what a route target would
// otherwise assume (the client's own protocol). Keeps the main package free of
// `if providerID == "codex" ...` branches (see AGENTS.md).

// ProtocolHint returns the protocol an implicit/explicit route target should
// declare for this provider's model, or "" when the provider speaks whatever
// the client speaks (no hint needed — the common case). Filled into implicit
// routes by the daemon and surfaced by doctor/config check when an explicit
// target is missing the declaration.
//
// `model` is accepted for future per-model rules (e.g. a suffix that forces a
// different API shape on an otherwise uniform provider); no provider needs it
// yet.
//
// NOTE: codex was REMOVED from this table (it used to hint "openai"). The hint
// was wrong: our protocol converter produces CHAT COMPLETIONS bodies, which
// codex's Responses-only backend rejects ("Unsupported parameter: messages") —
// the hint turned an obvious failure into a misleading one. See
// WireProtocolNote for the honest marker.
func ProtocolHint(providerID, model string) string {
	_ = model
	return ""
}

// WireProtocolNote describes a provider whose wire protocol our 2-value
// protocol system (anthropic / openai-chat) cannot express — emitted as an
// honest marker ("this client family can't be served") instead of a wrong
// conversion hint. "" when the provider's wire is covered by the 2-value
// system.
func WireProtocolNote(providerID string) string {
	switch providerID {
	case "codex":
		return "codex speaks the OpenAI Responses API (/responses, input-list body) — only Responses-speaking clients (e.g. codex CLI) can use it; anthropic / chat-completions clients would need a conversion that does not exist yet"
	default:
		return ""
	}
}
