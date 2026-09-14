// convert_responses.go — direct pairwise converters for the OpenAI Responses
// API (/v1/responses), the 3rd wire protocol alongside anthropic (/v1/messages)
// and openai-chat (/v1/chat/completions). Mirrors convert.go's direct-pairwise
// style (sonic + map[string]any, no Core IR); the anthropic↔chat converters
// there are untouched.
//
// Wire shapes (field-level reference: moon-bridge internal/protocol/openai):
//
//	Responses request:  {model, instructions, input:[items], tools, tool_choice,
//	                    reasoning:{effort}, max_output_tokens, temperature, top_p, stream}
//	  input items: {type:"message", role, content:[{type:"input_text"|"output_text"|
//	                "input_image", text|image_url}]}, {type:"function_call", name,
//	                arguments, call_id}, {type:"function_call_output", call_id, output},
//	                {type:"reasoning", summary:[{type:"summary_text",text}], encrypted_content}
//	  tools:       {type:"function", name, description, parameters}
//	Responses response: {id, object:"response", status, model, output:[items], usage}
//	  output items: {type:"message", role:"assistant", content:[{type:"output_text",text}]},
//	                {type:"function_call", id, call_id, name, arguments},
//	                {type:"reasoning", summary:[...]}
//
// The pure pairwise converter consumes a full input history. Proxy expands
// previous_response_id from its bounded local state store before calling it;
// callers that use this helper directly should likewise supply complete input.
package protocol

import (
	"fmt"
	"strings"
	"time"

	sonic "github.com/bytedance/sonic"
)

// ---------------------------------------------------------------------------
// shared helpers
// ---------------------------------------------------------------------------

// defaultAnthropicMaxTokens is the generous max_tokens injected when the
// source request carries no cap — Anthropic 400s "max_tokens required"
// otherwise. Same value as the chat→a direction (convert.go).
const defaultAnthropicMaxTokens = 4096

// responsesRequiredKeys backfills the Response-object fields the official
// schema requires but a synthesized non-streaming response does not otherwise
// populate: created_at (unix seconds; strongly-typed SDKs parse it as a
// no-default int64), error, incomplete_details, tools, tool_choice,
// parallel_tool_calls and metadata. hasIncomplete keeps a real
// incomplete_details set by the caller.
func responsesRequiredKeys(out map[string]any, hasIncomplete bool) {
	out["created_at"] = time.Now().Unix()
	out["error"] = nil
	if !hasIncomplete {
		out["incomplete_details"] = nil
	}
	out["tools"] = []any{}
	out["tool_choice"] = "auto"
	out["parallel_tool_calls"] = false
	out["metadata"] = map[string]any{}
}

// responsesCreatedAt reads a Responses object's created_at for the chat
// `created` field; falls back to now when absent (chat.completion requires
// created).
func responsesCreatedAt(src map[string]any) any {
	if v := src["created_at"]; v != nil {
		return v
	}
	return time.Now().Unix()
}

// copyOpt copies optional top-level keys from src to out when present.
func copyOpt(out, src map[string]any, keys ...string) {
	for _, k := range keys {
		if v, ok := src[k]; ok {
			out[k] = v
		}
	}
}

// firstNonEmpty returns the first non-empty string arg.
func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

// toolIDNormalizer maps raw Responses call_ids to anthropic-charset tool_use
// ids — the same contract as chat→a's normID/normResultID pair (convert.go):
// one raw id always yields the same sanitized id within a conversion
// (tool_use/tool_result pairing survives); two DIFFERENT raw ids that sanitize
// to the same string ("call.a" vs "call_a") get _2/_3 suffixes in order of
// first appearance; id-less uses get a FRESH placeholder per occurrence and
// id-less results consume those placeholders positionally.
type toolIDNormalizer struct {
	idMap        map[string]string
	usedNorm     map[string]bool
	emptyUses    []string
	emptyResults int
}

func newToolIDNormalizer() *toolIDNormalizer {
	return &toolIDNormalizer{idMap: map[string]string{}, usedNorm: map[string]bool{}}
}

func (t *toolIDNormalizer) use(id string) string {
	if id == "" {
		n := sanitizeToolUseID("")
		t.emptyUses = append(t.emptyUses, n)
		return n
	}
	if n, ok := t.idMap[id]; ok {
		return n
	}
	n := sanitizeToolUseID(id)
	for i := 2; t.usedNorm[n]; i++ {
		n = fmt.Sprintf("%s_%d", sanitizeToolUseID(id), i)
	}
	t.usedNorm[n] = true
	t.idMap[id] = n
	return n
}

func (t *toolIDNormalizer) result(id string) string {
	if id == "" && t.emptyResults < len(t.emptyUses) {
		n := t.emptyUses[t.emptyResults]
		t.emptyResults++
		return n
	}
	return t.use(id)
}

// strKey returns m[key] as a string, "" when m or the key is absent (unlike
// strOf, which renders a missing key as "null").
func strKey(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// strOpt is strKey for a bare value: "" for nil/non-string. Use it for
// OPTIONAL protocol fields (call_id/name) — strOf would stringify nil to
// "null", which is a non-empty string and both poisons the field and defeats
// firstNonEmpty fallbacks.
func strOpt(v any) string { s, _ := v.(string); return s }

// backfillToolNames fills empty function_call names from earlier items with
// the same call_id; names still missing afterwards get a convertWarn (an
// empty name is a protocol violation upstream).
func backfillToolNames(d *Diagnostics, items []map[string]any) {
	names := map[string]string{}
	for _, it := range items {
		if it["type"] != "function_call" {
			continue
		}
		id := strOpt(it["call_id"])
		if name := strOpt(it["name"]); name != "" {
			if id != "" {
				names[id] = name
			}
			continue
		}
		if n, ok := names[id]; ok {
			it["name"] = n
		} else {
			warnDiag(d, "tool_name_missing", "function_call missing name (call_id "+id+")")
		}
	}
}

// responsesInputItems normalizes a Responses request `input` field (string
// shorthand | array of items) into a slice of item maps. nil if absent/empty.
func responsesInputItems(v any) []map[string]any {
	switch raw := v.(type) {
	case string:
		if raw == "" {
			return nil
		}
		return []map[string]any{{
			"type": "message", "role": "user",
			"content": []map[string]any{{"type": "input_text", "text": raw}},
		}}
	case []any:
		var out []map[string]any
		for _, it := range raw {
			if m := asMap(it); m != nil {
				// The Responses API accepts message items in shorthand form
				// {role, content} with no type key (pi-ai/openai-responses
				// sends this); normalize so every type-dispatch downstream
				// sees them as messages instead of dropping them as unknown.
				if strOpt(m["type"]) == "" && strOpt(m["role"]) != "" {
					m["type"] = "message"
				}
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

// chatContentText concatenates text from an OpenAI-chat message content (string,
// or array of {type:"text",text} parts). Image parts are ignored here.
func chatContentText(v any) string {
	switch raw := v.(type) {
	case string:
		return raw
	case []any:
		var b strings.Builder
		for _, p := range raw {
			if pm := asMap(p); pm != nil {
				if t, ok := pm["type"].(string); ok && (t == "text" || t == "input_text" || t == "output_text") {
					b.WriteString(strOf(pm["text"]))
				}
			}
		}
		return b.String()
	}
	return ""
}

// parseDataURL splits a "data:<media>;base64,<data>" URL into media type + data.
func parseDataURL(s string) (mediaType, data string, ok bool) {
	if !strings.HasPrefix(s, "data:") {
		return "", "", false
	}
	rest := strings.TrimPrefix(s, "data:")
	semi := strings.Index(rest, ";base64,")
	if semi < 0 {
		return "", "", false
	}
	return rest[:semi], rest[semi+len(";base64,"):], true
}

// thinkingBudgetToEffort maps an anthropic thinking config {type, budget_tokens}
// to a Responses reasoning.effort (best-effort; budget↔effort is not 1:1).
// "" means "do not set reasoning". Thresholds sit at the effortToThinking
// ladder values so each ladder rung round-trips to its own effort.
func thinkingBudgetToEffort(thinking map[string]any) string {
	if t, _ := thinking["type"].(string); t != "" && t != "enabled" {
		return ""
	}
	budget, _ := thinking["budget_tokens"].(float64)
	switch {
	case budget >= 32000:
		return "xhigh"
	case budget >= 16384:
		return "high"
	case budget >= 8192:
		return "medium"
	case budget >= 2048:
		return "low"
	case budget > 0:
		return "minimal"
	}
	return "medium"
}

// normalizeReasoningEffort clamps a non-empty effort outside the canonical
// enum (none|minimal|low|medium|high|xhigh|max) down to "high" — the industry
// clamp-down convention for vendor-specific levels (e.g. codex "persistent").
// "" passes through (no effort expressed); the warning fires at this callsite
// layer so effortToThinking stays pure.
func normalizeReasoningEffort(d *Diagnostics, effort string) string {
	switch effort {
	case "", "none", "minimal", "low", "medium", "high", "xhigh", "max":
		return effort
	}
	warnDiagf(d, "unknown_effort", "unknown reasoning effort %q, clamping to high", effort)
	return "high"
}

// outputConfigEffort reads the adaptive-thinking effort level from an
// anthropic output_config object (Claude Code /effort wire). Unknown strings
// return "" so downstream defaults win (opencodex effortFromOutputConfig).
func outputConfigEffort(outputConfig any) string {
	effort := strOpt(asMap(outputConfig)["effort"])
	switch effort {
	case "minimal", "low", "medium", "high", "xhigh", "max":
		return effort
	case "ultra":
		// Not in the Responses reasoning.effort enum — map to the highest rung.
		return "max"
	}
	return ""
}

// effortToThinking maps a Responses reasoning.effort back to an anthropic
// thinking config (best-effort budget). nil means "do not set thinking"
// (covers "" and "none"); unknown non-empty values clamp down to high — the
// industry convention for vendor-specific levels (callsites warn via
// normalizeReasoningEffort; this function stays pure). The ladder rungs sit
// on the real client budget clusters (pi 1024/2048/8192/16384, opencode
// 16000, kimi 32000). maxTokens is the EFFECTIVE anthropic max_tokens (after
// any default injection): Anthropic requires 1024 ≤ budget_tokens <
// max_tokens, so the ladder value is clamped below maxTokens; when maxTokens
// ≤ 1024 thinking cannot be expressed legally at all and is silently disabled
// (deterministic best-effort, no warning).
func effortToThinking(effort string, maxTokens int) map[string]any {
	budget := 0
	switch effort {
	case "max":
		// Top out just below the output cap (clamped again below; with
		// maxTokens ≤ 1024 the < 1024 check disables thinking).
		budget = maxTokens - 1
	case "xhigh":
		budget = 32000
	case "high":
		budget = 16384
	case "medium":
		budget = 8192
	case "low":
		budget = 2048
	case "minimal":
		budget = 1024
	case "", "none":
		return nil
	default:
		// Unknown non-empty effort (e.g. codex "persistent") → clamp to high.
		budget = 16384
	}
	if maxTokens > 0 && budget > maxTokens-1 {
		budget = maxTokens - 1
	}
	if budget < 1024 {
		return nil
	}
	return map[string]any{"type": "enabled", "budget_tokens": budget}
}

// marshalToolInput serializes an anthropic tool_use input to a JSON arguments
// string; a MISSING input becomes "{}" (sonic renders nil as "null", which
// downstream JSON.parse rejects).
func marshalToolInput(v any) string {
	if v == nil {
		return "{}"
	}
	args, _ := sonic.MarshalString(v)
	if args == "" || args == "null" {
		return "{}"
	}
	return args
}

// thinkOpenTag / thinkCloseTag mark inline thinking blocks that some chat
// upstreams (MiniMax-style) emit inside content instead of a reasoning field.
const thinkOpenTag = "<think>"
const thinkCloseTag = "</think>"

// splitLeadingThinkBlock splits a LEADING <think>…</think> block (leading
// whitespace tolerated) into (reasoning, answer): reasoning trimmed, the
// answer's leading separator whitespace stripped (cc-switch
// split_leading_think_block). ok=false when the text does not START with a
// think block — mid-text blocks are never split.
func splitLeadingThinkBlock(text string) (reasoning, answer string, ok bool) {
	trimmed := strings.TrimLeft(text, " \t\r\n")
	if !strings.HasPrefix(trimmed, thinkOpenTag) {
		return "", "", false
	}
	body := trimmed[len(thinkOpenTag):]
	close := strings.Index(body, thinkCloseTag)
	if close < 0 {
		return "", "", false
	}
	reasoning = strings.TrimSpace(body[:close])
	answer = strings.TrimLeft(body[close+len(thinkCloseTag):], " \t\r\n")
	return reasoning, answer, true
}

// responsesReasoningText extracts (text, signature) from a Responses reasoning
// item's summary array (+ encrypted_content as signature). OpenRouter-style
// items carry the text in content parts ({type:"reasoning_text"}) instead of a
// summary — fall back to those (their streams use reasoning_text.delta too).
func responsesReasoningText(item map[string]any) (text, sig string) {
	if enc, ok := item["encrypted_content"].(string); ok {
		sig = enc
	}
	if sum, ok := item["summary"].([]any); ok {
		var b strings.Builder
		for _, s := range sum {
			if sm := asMap(s); sm != nil {
				b.WriteString(strOf(sm["text"]))
			}
		}
		text = b.String()
	}
	if text == "" {
		if parts, ok := item["content"].([]any); ok {
			var b strings.Builder
			for _, p := range parts {
				if pm := asMap(p); pm != nil {
					b.WriteString(strOf(pm["text"]))
				}
			}
			text = b.String()
		}
	}
	return
}

// chatReasoningText extracts reasoning text from a chat message or stream
// delta, exhausting the vendor spellings (cc-switch codex_chat_common's
// extraction order): reasoning_content > reasoning (string, or an object with
// content/text/summary) > reasoning_details (array/object; OpenRouter-style,
// e.g. aqp). "" when nothing replayable is present.
func chatReasoningText(m map[string]any) string {
	for _, k := range []string{"reasoning_content", "reasoning"} {
		if s := strOpt(m[k]); s != "" {
			return s
		}
	}
	if r := asMap(m["reasoning"]); r != nil {
		for _, k := range []string{"content", "text", "summary"} {
			if s := strOpt(r[k]); s != "" {
				return s
			}
		}
	}
	return reasoningDetailsText(m["reasoning_details"])
}

// reasoningDetailsText extracts text from a reasoning_details value (string,
// object, or array of detail entries joined "\n\n").
func reasoningDetailsText(v any) string {
	switch raw := v.(type) {
	case string:
		return raw
	case map[string]any:
		return reasoningDetailPartText(raw)
	case []any:
		var parts []string
		for _, p := range raw {
			if s := reasoningDetailPartText(p); s != "" {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

// reasoningDetailPartText reads one reasoning_details entry: text/content/
// summary keys carry the replayable text (encrypted entries have none and are
// skipped naturally).
func reasoningDetailPartText(v any) string {
	m := asMap(v)
	if m == nil {
		return strOpt(v)
	}
	for _, k := range []string{"text", "content", "summary"} {
		if s := strOpt(m[k]); s != "" {
			return s
		}
	}
	return ""
}
