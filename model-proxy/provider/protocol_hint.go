package provider

// protocol_hint.go — provider-specific knowledge about which WIRE protocol a
// provider's models speak when it differs from what a route target would
// otherwise assume (the client's own protocol). Keeps the main package free of
// `if providerID == "codex" ...` branches (see AGENTS.md).

// ProtocolHint returns the protocol an implicit/explicit route target should
// declare for this provider's model, or "" when the provider speaks whatever
// the client speaks (no hint needed — the common case). Filled into implicit
// routes by the daemon (proxy.go / main.go) and surfaced by doctor/config check
// when an explicit target is missing the declaration.
//
// `model` is accepted for future per-model rules (e.g. a suffix that forces a
// different API shape on an otherwise uniform provider); no provider needs it
// yet.
//
// codex speaks the OpenAI Responses API, NOT chat completions. It is hinted
// "responses" so an anthropic/chat client reaching a codex target is converted
// to a Responses body (convert_responses.go) rather than passthrough'd into a
// body codex rejects ("Unsupported parameter: messages"). This hint is only
// correct because a real Responses converter exists; without it the hint would
// turn an obvious failure into a misleading one (see git history).
func ProtocolHint(providerID, model string) string {
	_ = model
	switch providerID {
	case "codex":
		return "responses"
	}
	return ""
}

// ChatReasoningMode returns how a responses reasoning.effort should be
// rendered for this provider's CHAT endpoint (r→chat conversion): the chat
// dialects differ per vendor family (cc-switch mapReasoningEffort).
//
//	"reasoning_effort" (default) — flat reasoning_effort field, kept as-is
//	"thinking"        — thinking: {type:"enabled"|"disabled"} (zhipu/volcengine/kimi-code/deepseek)
//	"enable_thinking" — enable_thinking: bool (qwen-plan)
//	"openrouter"      — native reasoning: {effort} object (aqp / OpenRouter 系)
func ChatReasoningMode(providerID string) string {
	switch providerID {
	case "zhipu", "volcengine", "kimi-code", "deepseek":
		return "thinking"
	case "qwen-plan":
		return "enable_thinking"
	case "aqp", "shopee":
		return "openrouter"
	}
	return "reasoning_effort"
}

// WireProtocolNote describes a provider whose wire protocol our protocol system
// cannot express AND cannot convert to — emitted as an honest marker ("this
// client family can't be served") instead of a wrong conversion hint. "" when
// the provider's wire is covered (passthrough or conversion). codex is now
// covered (Responses converter), so it no longer carries a note.
func WireProtocolNote(providerID string) string {
	_ = providerID
	return ""
}
