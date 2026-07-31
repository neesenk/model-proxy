package protocol

import (
	"bytes"
	"encoding/json"
)

func ExtractModel(body []byte) string {
	dec := json.NewDecoder(bytes.NewReader(body))
	// Consume the opening {.
	dec.Token()
	// model is the first top-level key in every known LLM client (Claude Code,
	// codex, openai SDK, ...), so the fast path reads just 3 tokens (~40 bytes)
	// and returns — it never touches the messages/tools/system content that makes
	// up 99% of a real body. On a 200KB body this is ~800ns vs ~523µs for a full
	// json.Unmarshal (653× faster). Falls back to Unmarshal if the first key
	// isn't "model" (non-standard key order — safe, rare).
	firstTok, _ := dec.Token()
	if key, ok := firstTok.(string); ok && key == "model" {
		valTok, _ := dec.Token()
		if s, ok := valTok.(string); ok {
			return s
		}
		return ""
	}
	// Non-standard key order: full parse (rare, correct).
	var v struct {
		Model string `json:"model"`
	}
	json.Unmarshal(body, &v)
	return v.Model
}
