package requestlog

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strings"
)

// computeTurnKey returns a fingerprint that identifies one conversational turn.
// It mirrors the frontend's requestExcerpt semantics:
//   - parse the request body as JSON;
//   - read messages[] (openai/chat/anthropic) or input[] / input string (responses);
//   - scan backward for the newest user message that carries actual text;
//   - skip tool_result blocks (anthropic agentic turns report tool results as
//     role:user content blocks);
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
func computeTurnKey(body []byte) string {
	if len(body) == 0 || len(body) > 1500000 {
		return ""
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return ""
	}
	if s, ok := doc["input"].(string); ok {
		if text := strings.TrimSpace(s); text != "" {
			return hashTurnKey(0, text)
		}
		return ""
	}
	var msgs []any
	if m, ok := doc["messages"].([]any); ok {
		msgs = m
	} else if m, ok := doc["input"].([]any); ok {
		msgs = m
	} else {
		return ""
	}
	textCount := 0
	text := ""
	for _, um := range msgs {
		m, ok := um.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := m["role"].(string); role != "user" {
			continue
		}
		if t := lastUserText(m["content"]); t != "" {
			textCount++
			text = t
		}
	}
	if text == "" {
		return ""
	}
	return hashTurnKey(textCount, text)
}

// lastUserText extracts human-readable text from a user message's content.
// content may be a plain string or an array of content blocks; only text
// blocks count, and tool_result blocks are explicitly skipped so that an
// anthropic coding-agent turn still surfaces the human instruction.
func lastUserText(content any) string {
	if s, ok := content.(string); ok {
		return strings.TrimSpace(s)
	}
	blocks, ok := content.([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		block, ok := b.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := block["type"].(string)
		if typ == "tool_result" {
			continue
		}
		if typ != "text" {
			continue
		}
		if t, ok := block["text"].(string); ok {
			if s := strings.TrimSpace(t); s != "" {
				parts = append(parts, s)
			}
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " ")
}

func hashTurnKey(count int, text string) string {
	h := fnv.New64a()
	_, _ = fmt.Fprintf(h, "%d|%s", count, text)
	return fmt.Sprintf("%016x", h.Sum64())
}
