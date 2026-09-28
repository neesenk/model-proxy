package protocol

import (
	"bytes"
	"encoding/json"
	"strings"
)

// sessionIDNeedles pre-filters SessionIDFromBody's JSON parse: a body carrying
// none of the raw key spellings cannot yield an id, so multi-megabyte
// conversations skip the full unmarshal entirely (the ExtractModel pattern).
var sessionIDNeedles = [][]byte{
	[]byte(`"client_metadata"`),
	[]byte(`"prompt_cache_key"`),
	[]byte(`"user_id"`),
}

// maxBodySessionIDLen bounds a body-carried session id (an opaque identity
// token — UUID/hash shaped — never free text): oversized values are ignored
// rather than truncated, because a truncated id would silently split one
// session across two identities. 256 is Anthropic's documented cap for
// metadata.user_id (Claude Code's composite "user_..._account_..._session_..."
// form measures 134); every other field's real-world values are UUIDs.
const maxBodySessionIDLen = 256

// SessionIDFromBody returns the stable conversation id that spec-conforming
// agents ride in the request BODY when they send no session header. The
// OpenAI and Anthropic API specs each define exactly one such field, and
// agents implemented strictly against them (Kimi Code, Codex, ...) carry
// their per-session id there instead of any bespoke header
// (github.com/MoonshotAI/kimi-code/issues/3506), checked in this order:
//
//   - client_metadata.session_id — Codex's Responses-protocol convention
//     (first: the only field that says "session" outright);
//   - prompt_cache_key — OpenAI Chat Completions/Responses ("Used to cache
//     responses for similar requests to optimize your cache hit rates.
//     Replaces the user field"); Moonshot's coding-agent guidance: "this is
//     typically a session id or task id representing a single session; if
//     the session is exited and later resumed, this value should remain the
//     same";
//   - metadata.user_id — Anthropic Messages ("A stable identifier for the
//     end user or session"); coding agents are told to pass the session id
//     and keep it constant for the whole session.
//
// The result feeds observation surfaces (live events, request log, guard
// session dimension) and providers' upstream session-affinity headers. It is
// CLIENT-CONTROLLED and must never feed trusted security decisions (block
// table, repeat interception, session scan) or the routing sticky key, which
// stays x-claude-code-session-id. Bodies carrying none of the fields return
// "" — header-sending agents (Claude Code, pi, OpenCode) resolve theirs via
// the request-log header allowlist instead.
func SessionIDFromBody(body []byte) string {
	anyNeedle := false
	for _, needle := range sessionIDNeedles {
		if bytes.Contains(body, needle) {
			anyNeedle = true
			break
		}
	}
	if !anyNeedle {
		return ""
	}
	var doc struct {
		ClientMetadata struct {
			SessionID any `json:"session_id"`
		} `json:"client_metadata"`
		PromptCacheKey any `json:"prompt_cache_key"`
		Metadata       struct {
			UserID any `json:"user_id"`
		} `json:"metadata"`
	}
	// Best-effort by design: encoding/json keeps every field it managed to
	// decode when another field's shape mismatches, so one malformed candidate
	// (a numeric session_id, a string metadata) never masks the others.
	_ = json.Unmarshal(body, &doc)
	if sid := bodySessionToken(doc.ClientMetadata.SessionID); sid != "" {
		return sid
	}
	if key := bodySessionToken(doc.PromptCacheKey); key != "" {
		return key
	}
	return bodySessionToken(doc.Metadata.UserID)
}

// bodySessionToken validates one candidate leaf: a string, trimmed, printable
// ASCII only, and at most maxBodySessionIDLen bytes. Anything else (numbers,
// objects, ids with control bytes or non-ASCII) counts as absent — session
// ids are opaque tokens minted by clients (UUIDs, hashes), never prose, and
// the value must survive being echoed into an upstream header.
func bodySessionToken(v any) string {
	s, ok := v.(string)
	if !ok {
		return ""
	}
	s = strings.TrimSpace(s)
	if len(s) == 0 || len(s) > maxBodySessionIDLen {
		return ""
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return ""
		}
	}
	return s
}
