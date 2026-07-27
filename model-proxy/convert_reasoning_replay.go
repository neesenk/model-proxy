package main

// chatAnthropicThinkingReplay extracts signed Anthropic thinking envelopes
// carried through the Chat reasoning_details extension. Only cryptographically
// replayable blocks are accepted: unsigned plaintext must never be invented as
// an Anthropic history block because a following tool_use would be rejected.
func chatAnthropicThinkingReplay(message map[string]any) []map[string]any {
	var out []map[string]any
	for _, raw := range anySlice(message["reasoning_details"]) {
		detail := asMap(raw)
		switch strOpt(detail["type"]) {
		case "thinking", "anthropic_thinking":
			signature := strOpt(detail["signature"])
			thinking := strOpt(detail["thinking"])
			// An empty thinking text replays as an empty thinking block, which
			// Anthropic may reject — skip the item entirely.
			if signature == "" || thinking == "" {
				continue
			}
			out = append(out, map[string]any{
				"type": "thinking", "thinking": thinking, "signature": signature,
			})
		case "redacted_thinking", "anthropic_redacted_thinking":
			data := strOpt(detail["data"])
			if data == "" {
				continue
			}
			out = append(out, map[string]any{"type": "redacted_thinking", "data": data})
		}
	}
	return out
}
