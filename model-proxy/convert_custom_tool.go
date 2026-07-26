// convert_custom_tool.go — custom/freeform tool support for the
// responses↔chat pair (cc-switch transform_codex_chat.rs port). Codex CLI's
// main tools (shell/apply_patch) are Responses CUSTOM tools
// {type:"custom", name, description} whose call carries a RAW string input,
// not JSON arguments. The r→chat request converter wraps each custom tool as
// a one-parameter function ({input: string}) and encodes custom_tool_call
// history as arguments {"input": <raw>}; the chat→r response converter
// unwraps calls to known custom tools back into custom_tool_call items /
// response.custom_tool_call_input.* stream events, with progressive JSON
// unwrapping for live preview (opencodex freeformPartialInput).
package main

import (
	"strings"

	sonic "github.com/bytedance/sonic"
)

// customToolParameters is the fixed wrapper schema for a custom tool.
var customToolParameters = map[string]any{
	"type":                 "object",
	"properties":           map[string]any{"input": map[string]any{"type": "string"}},
	"required":             []string{"input"},
	"additionalProperties": false,
}

// responsesCustomToolSet returns the names of the custom/freeform tools
// declared in a responses request body (top-level tools + input
// additional_tools items — codex 0.145 declares tools only there; nil when
// none). Custom tools are never namespaced, container subtools included.
func responsesCustomToolSet(reqBody []byte) map[string]bool {
	var src map[string]any
	if err := sonic.Unmarshal(reqBody, &src); err != nil {
		return nil
	}
	var out map[string]bool
	for _, e := range nsExpandTools(responsesRequestTools(src)) {
		if strOpt(e.tm["type"]) != "custom" {
			continue
		}
		if name := strOpt(e.tm["name"]); name != "" {
			if out == nil {
				out = map[string]bool{}
			}
			out[name] = true
		}
	}
	return out
}

// wrapCustomTool maps a Responses custom tool to a chat function tool:
// {type:"function", function:{name, description?, parameters:{input:string}}}.
func wrapCustomTool(tm map[string]any) map[string]any {
	fn := map[string]any{
		"name":       strOpt(tm["name"]),
		"parameters": customToolParameters,
	}
	if d, ok := tm["description"]; ok {
		fn["description"] = d
	}
	return map[string]any{"type": "function", "function": fn}
}

// wrapCustomCallArguments encodes a custom_tool_call's raw input string as
// chat arguments JSON: {"input": "<raw>"}.
func wrapCustomCallArguments(rawInput string) string {
	b, _ := sonic.MarshalString(map[string]any{"input": rawInput})
	return b
}

// unwrapCustomCallArguments decodes chat arguments {"input": "<raw>"} back to
// the raw input string; on any parse failure it falls back to the raw
// arguments string (never loses content).
func unwrapCustomCallArguments(arguments string) string {
	var m map[string]any
	if err := sonic.UnmarshalString(arguments, &m); err == nil {
		if s, ok := m["input"].(string); ok {
			return s
		}
	}
	return arguments
}

// ---------------------------------------------------------------------------
// progressive unwrapping (stream): arguments arrive as partial JSON of
// {"input": "<string>"}; emit the unescaped string CONTENT as it becomes
// decodable, holding incomplete trailing escape sequences for the next chunk.
// ---------------------------------------------------------------------------

// partialInputUnwrapper progressively unwraps one tool call's arguments.
// Idempotent design: feed it the FULL accumulated raw arguments each time and
// it returns the full unwrapped-so-far string; the caller diffs for deltas.
type partialInputUnwrapper struct {
	fallback bool // arguments did not match the {"input": " prefix → raw passthrough
}

// newPartialInputUnwrapper returns a fresh progressive unwrapper.
func newPartialInputUnwrapper() *partialInputUnwrapper { return &partialInputUnwrapper{} }

// unwrap returns the decodable prefix of the unwrapped input. complete=true
// (done frame) also strips a lone closing quote and forces trailing escapes.
func (u *partialInputUnwrapper) unwrap(raw string, complete bool) string {
	if u.fallback {
		return raw
	}
	content, ok := stripInputPrefix(raw)
	if !ok {
		if complete || len(raw) > 32 {
			// Enough bytes to know the prefix will never match: pass through.
			u.fallback = true
			return raw
		}
		return "" // still a possible prefix — wait for more bytes
	}
	// A trailing `"}` means the arguments are finished even mid-stream
	// (trailing whitespace after the string's closing quote is tolerated,
	// mirroring the prefix's whitespace tolerance).
	if rest, ok := stripClosingQuoteBrace(content); ok {
		content = rest
	} else if complete {
		content = stripInputSuffix(content)
	}
	return unescapeHold(content, complete)
}

// stripClosingQuoteBrace strips a trailing `"}` sequence, allowing whitespace
// between the closing quote and the brace and after the brace.
func stripClosingQuoteBrace(s string) (string, bool) {
	t := strings.TrimRight(s, " \t\n\r")
	if !strings.HasSuffix(t, "}") {
		return s, false
	}
	t = strings.TrimRight(t[:len(t)-1], " \t\n\r")
	if strings.HasSuffix(t, `"`) {
		return t[:len(t)-1], true
	}
	return s, false
}

// stripInputPrefix matches {"input": " with optional whitespace, returning
// the remainder. ok=false when raw is too short to decide or mismatches.
func stripInputPrefix(raw string) (rest string, ok bool) {
	i := 0
	skipWS := func() {
		for i < len(raw) && (raw[i] == ' ' || raw[i] == '\t' || raw[i] == '\n' || raw[i] == '\r') {
			i++
		}
	}
	match := func(c byte) bool {
		if i >= len(raw) || raw[i] != c {
			return false
		}
		i++
		return true
	}
	// Compare against the compact pattern but tolerate whitespace between
	// tokens ({"input": " vs {"input":" etc.). The opening quote of the string
	// value is matched separately after the loop.
	for _, c := range []byte(`{"input":`) {
		skipWS()
		if !match(c) {
			return "", false
		}
	}
	skipWS()
	if i >= len(raw) || raw[i] != '"' {
		return "", false
	}
	i++
	return raw[i:], true
}

// stripInputSuffix removes the closing `"}` (or trailing `"`) when complete.
func stripInputSuffix(s string) string {
	if strings.HasSuffix(s, `"}`) {
		return s[:len(s)-2]
	}
	if strings.HasSuffix(s, `"`) {
		return s[:len(s)-1]
	}
	return s
}

// unescapeHold unescapes JSON string content, HOLDING an incomplete trailing
// escape sequence (`\`, `\uXX`) unless complete is set (then it is emitted
// verbatim — the stream is over, better raw than lost).
func unescapeHold(s string, complete bool) string {
	var b strings.Builder
	i := 0
	for i < len(s) {
		c := s[i]
		if c != '\\' {
			b.WriteByte(c)
			i++
			continue
		}
		if i+1 >= len(s) {
			if complete {
				b.WriteByte(c)
			}
			break
		}
		switch s[i+1] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case '/':
			b.WriteByte('/')
		case '\\':
			b.WriteByte('\\')
		case '"':
			b.WriteByte('"')
		case 'u':
			if i+6 > len(s) {
				if complete {
					b.WriteString(s[i:])
				}
				return b.String()
			}
			if r, ok := decodeHex4(s[i+2 : i+6]); ok {
				b.WriteRune(r)
			}
			i += 4 // fall through to the common i += 2
		default:
			// Unknown escape: keep both bytes (JSON would reject, but
			// content fidelity beats strictness here).
			b.WriteByte(c)
			b.WriteByte(s[i+1])
		}
		i += 2
	}
	return b.String()
}

// decodeHex4 parses 4 hex digits into a rune (BMP only; surrogate pairs pass
// through as-is, matching JSON-decoder leniency needs of a preview stream).
func decodeHex4(s string) (rune, bool) {
	v := 0
	for _, c := range []byte(s) {
		v <<= 4
		switch {
		case c >= '0' && c <= '9':
			v |= int(c - '0')
		case c >= 'a' && c <= 'f':
			v |= int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v |= int(c-'A') + 10
		default:
			return 0, false
		}
	}
	return rune(v), true
}
