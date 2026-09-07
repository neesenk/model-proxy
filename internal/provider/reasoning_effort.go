package provider

import "strings"

// reasoning_effort.go — provider-specific knowledge about which reasoning
// effort LEVELS a provider's chat endpoint accepts beyond the on/off switch
// shape (ChatReasoningMode in protocol_hint.go). Same boundary rule: vendor
// knowledge lives here and flows to the protocol converters as plain data via
// targetexec.Plan → protocol.RequestOptions (internal/protocol stays a leaf).
//
// Sources (vendor docs, 2026-09):
//   - deepseek: https://api-docs.deepseek.com/guides/thinking_mode
//   - zhipu:    https://docs.z.ai/guides/capabilities/thinking
//   - kimi:     https://platform.kimi.com/docs/guide/use-thinking-models
//   - qwen:     https://help.aliyun.com (deep-thinking / 深度思考 guide)

// EffortProfile describes how a provider's CHAT endpoint accepts reasoning
// effort LEVELS beyond the on/off switch shape (ChatReasoningMode).
type EffortProfile struct {
	// Enum maps canonical effort (minimal|low|medium|high|xhigh|max, and
	// "none" where the vendor expresses off as an enum value) to the
	// vendor-accepted string. nil = endpoint has no level knob.
	Enum map[string]string
	// EnumOnly: emit the enum INSTEAD of the thinking switch (kimi-k3
	// rejects thinking+reasoning_effort together; the enum replaces it).
	EnumOnly bool
}

// ChatEffortProfile returns the effort-level profile for this provider's CHAT
// endpoint (r→chat conversion), layered on top of ChatReasoningMode's switch
// shape. The zero profile (nil Enum) means "switch only" — the default for
// every provider whose chat endpoint either passes reasoning_effort through
// natively (aqp/shopee OpenRouter dialect) or has too fragmented per-model
// subsets to pin down (volcengine). Register a profile ONLY when the vendor's
// chat endpoint accepts a non-pass-through enum or replaces the switch.
func ChatEffortProfile(providerID, model string) EffortProfile {
	switch providerID {
	case "deepseek":
		// Vendor enum low|high|max; the switch stays (thinking.type).
		return EffortProfile{Enum: map[string]string{
			"minimal": "low", "low": "low", "medium": "high",
			"high": "high", "xhigh": "high", "max": "max",
		}}
	case "zhipu":
		// glm-5.3 rejects thinking.type:"disabled" outright and accepts only
		// low|high|max, so "off" is expressed as the low enum value. glm-5.2
		// accepts the full enum (vendor maps internally). Older/other models
		// keep the bare switch.
		if strings.HasPrefix(model, "glm-5.3") {
			return EffortProfile{Enum: map[string]string{
				"none": "low", "minimal": "low", "low": "low", "medium": "high",
				"high": "high", "xhigh": "high", "max": "max",
			}}
		}
		if strings.HasPrefix(model, "glm-5.2") {
			return EffortProfile{Enum: map[string]string{
				"minimal": "minimal", "low": "low", "medium": "medium",
				"high": "high", "xhigh": "xhigh", "max": "max",
			}}
		}
	case "kimi-code":
		// kimi-k3 accepts low|high|max and rejects thinking+reasoning_effort
		// together, so the enum REPLACES the switch ("off" → low). kimi-k2.x
		// keeps the bare thinking switch.
		if strings.HasPrefix(model, "kimi-k3") {
			return EffortProfile{
				Enum: map[string]string{
					"none": "low", "minimal": "low", "low": "low", "medium": "high",
					"high": "high", "xhigh": "high", "max": "max",
				},
				EnumOnly: true,
			}
		}
	case "qwen-plan":
		// qwen3.8 accepts xhigh|medium|low (help.aliyun.com deep-thinking).
		// Gated on the "3.8" family substring — config.yaml's preset list
		// carries qwen3.8-max-preview; no other qwen model name contains it.
		if strings.Contains(model, "3.8") {
			return EffortProfile{Enum: map[string]string{
				"minimal": "low", "low": "low", "medium": "medium",
				"high": "xhigh", "xhigh": "xhigh", "max": "xhigh",
			}}
		}
	}
	return EffortProfile{}
}
