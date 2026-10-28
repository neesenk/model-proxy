package requestlog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"io"
	"regexp"
	"strings"
)

// ComputeTurnKey returns a fingerprint that identifies one conversational turn.
// It mirrors the frontend's requestExcerpt semantics:
//   - parse the request body as JSON;
//   - read messages[] (openai/chat/anthropic) or input[] / input string (responses);
//   - scan backward for the newest user message that carries actual text;
//   - skip tool_result blocks (anthropic agentic turns report tool results as
//     role:user content blocks);
//   - strip agent-injected envelopes (<system-reminder>/<git-context>) so that
//     reminders a coding agent appends as role:user text do not register as a
//     new human turn;
//   - hash the real-user-text message count plus the extracted text.
//
// The count is the number of user messages carrying actual text — NOT the
// total message count: within one agentic turn every follow-up request appends
// assistant/tool messages, so a total-count hash would differ per request and
// degenerate the Trace segmentation to one segment per request. The
// real-user-text count stays constant across a turn's sub-requests and still
// increments when a new human instruction arrives, so two consecutive turns
// that send the same literal text (e.g. "continue") produce different keys.
// An unparseable body or one with no textual user content yields an empty key.
//
// The body is decoded with a streaming token walk instead of one
// json.Unmarshal into map[string]any: this runs on every forwarded request
// (forward's route-escalation signal and the request log both call it), and
// materializing a 1.5MB body as boxed maps/slices was a per-request
// multi-MB allocation spike. Semantics are pinned to the map-based original:
// duplicate top-level keys keep the last occurrence, "input" as a string wins
// over any messages array, non-array/non-string values count as absent, and
// trailing garbage after the top-level object fails the parse.
//
// ComputeTurnKey is the single authoritative source for turn fingerprints; it
// is consumed by both the request log and the route escalation repeat_turn signal.
func ComputeTurnKey(body []byte) string {
	if len(body) == 0 || len(body) > 1500000 {
		return ""
	}
	doc, ok := scanTurnDoc(body)
	if !ok {
		return ""
	}
	if doc.inputString != nil {
		if text := strings.TrimSpace(*doc.inputString); text != "" {
			return hashTurnKey(0, text)
		}
		return ""
	}
	var msgs []json.RawMessage
	switch {
	case doc.messages != nil:
		msgs = doc.messages
	case doc.inputArray != nil:
		msgs = doc.inputArray
	default:
		return ""
	}
	textCount := 0
	text := ""
	for _, raw := range msgs {
		var m struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		if m.Role != "user" {
			continue
		}
		if t := lastUserText(m.Content); t != "" {
			textCount++
			text = t
		}
	}
	if text == "" {
		return ""
	}
	return hashTurnKey(textCount, text)
}

// turnDoc is the streaming decode's view of a request body: only the two keys
// ComputeTurnKey reads, with last-occurrence-wins overwrite matching the
// map-based original. A nil field means the key was absent (or carried a
// non-matching value type).
type turnDoc struct {
	inputString *string
	messages    []json.RawMessage
	inputArray  []json.RawMessage
}

// scanTurnDoc streams the top-level object, skipping unread keys' values as
// raw bytes and decoding messages/input arrays element-by-element (only user
// messages are ever unmarshaled into structures). ok is false for any body
// json.Unmarshal into map[string]any would reject.
func scanTurnDoc(body []byte) (turnDoc, bool) {
	var doc turnDoc
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		return doc, false
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return doc, false
	}
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return doc, false
		}
		key, ok := keyTok.(string)
		if !ok {
			return doc, false
		}
		switch key {
		case "messages", "input":
			// Last occurrence wins, exactly like the map-based original:
			// a repeated key's earlier value is forgotten before this one
			// records what the decoded type allows.
			if key == "input" {
				doc.inputString, doc.inputArray = nil, nil
			} else {
				doc.messages = nil
			}
			value, isString, ok := scanTurnArray(dec)
			if !ok {
				return doc, false
			}
			if isString {
				// Only "input" may be a string; "messages": "..." matches no
				// type assertion in the original and counts as absent.
				if key == "input" {
					doc.inputString = &value.str
				}
			} else if value.arr != nil {
				if key == "messages" {
					doc.messages = value.arr
				} else {
					doc.inputArray = value.arr
				}
			}
		default:
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return doc, false
			}
		}
	}
	if _, err := dec.Token(); err != nil { // closing '}'
		return doc, false
	}
	if _, err := dec.Token(); err != io.EOF { // trailing garbage fails, like Unmarshal
		return doc, false
	}
	return doc, true
}

// turnValue is scanTurnArray's result: exactly one of arr/str is meaningful,
// selected by isString.
type turnValue struct {
	arr []json.RawMessage
	str string
}

// scanTurnArray consumes the value following a "messages"/"input" key. String
// and array values stream straight off the decoder (no whole-value copy);
// any other value's remainder is discarded but must still be valid JSON.
func scanTurnArray(dec *json.Decoder) (value turnValue, isString bool, ok bool) {
	tok, err := dec.Token()
	if err != nil {
		return value, false, false
	}
	switch t := tok.(type) {
	case string:
		value.str = t
		return value, true, true
	case json.Delim:
		if t != '[' {
			// Object/other container as "messages"/"input": the original's
			// type assertions fail — skip the rest of the value.
			return value, false, skipTurnValue(dec, t)
		}
		for dec.More() {
			var elem json.RawMessage
			if err := dec.Decode(&elem); err != nil {
				return value, false, false
			}
			value.arr = append(value.arr, elem)
		}
		if _, err := dec.Token(); err != nil { // closing ']'
			return value, false, false
		}
		return value, false, true
	default:
		// null/number/bool: fully consumed scalars.
		return value, false, true
	}
}

// skipTurnValue discards the remainder of a JSON value whose first token
// (an opening delimiter) was already consumed.
func skipTurnValue(dec *json.Decoder, open json.Delim) bool {
	depth := 1
	for depth > 0 {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		if delim, ok := tok.(json.Delim); ok {
			switch delim {
			case '[', '{':
				depth++
			case ']', '}':
				depth--
			}
		}
	}
	return true
}

// lastUserText extracts human-readable text from a user message's content.
// content may be a plain string or an array of content blocks; only text
// blocks count, and tool_result blocks are explicitly skipped so that an
// anthropic coding-agent turn still surfaces the human instruction.
func lastUserText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	if content[0] == '"' {
		var s string
		if err := json.Unmarshal(content, &s); err != nil {
			return ""
		}
		return strings.TrimSpace(stripInjectedEnvelopes(s))
	}
	if content[0] != '[' {
		return ""
	}
	var blocks []json.RawMessage
	if err := json.Unmarshal(content, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, raw := range blocks {
		var block struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if err := json.Unmarshal(raw, &block); err != nil {
			// Non-object elements (numbers, strings) carried no text in the
			// map-based original either — skip them.
			continue
		}
		if block.Type == "tool_result" {
			continue
		}
		if block.Type != "text" {
			continue
		}
		if s := strings.TrimSpace(stripInjectedEnvelopes(block.Text)); s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ")
}

// injectedEnvelopeRE matches the machine-generated envelopes coding agents
// (kimi-code-cli, claude code) append as role:user text: <system-reminder>
// and <git-context> blocks, closed or running to the end of the string.
// Without stripping, every appended reminder bumps the real-user-text count
// or replaces the newest user text, splitting one conversational turn into
// many Trace segments.
var injectedEnvelopeRE = regexp.MustCompile(`(?s)<(?:system-reminder|git-context)(?:\s[^>]*)?>.*?(?:</(?:system-reminder|git-context)\s*>|$)`)

func stripInjectedEnvelopes(s string) string {
	return injectedEnvelopeRE.ReplaceAllString(s, " ")
}

func hashTurnKey(count int, text string) string {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%d|%s", count, text)
	return fmt.Sprintf("%016x", h.Sum64())
}
