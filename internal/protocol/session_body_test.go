package protocol

import (
	"strings"
	"testing"
)

// TestSessionIDFromBody pins the body-carried session-id fallback for agents
// that send no session header: the three spec-defined fields (Codex
// client_metadata.session_id, OpenAI/Moonshot prompt_cache_key, Anthropic
// metadata.user_id), their precedence, and the defensive rejections.
// Background: github.com/MoonshotAI/kimi-code/issues/3506 — Kimi Code is
// implemented strictly against the OpenAI/Anthropic specs and carries its
// session id in exactly these fields, with no bespoke header.
func TestSessionIDFromBody(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "codex responses shape",
			body: `{"model":"gpt-5.6","stream":true,"client_metadata":{"session_id":" 01a0b26d-fde4-4f6f-8bd5-58cb9a0f8b1e ","thread_id":"t-1"},"input":[]}`,
			want: "01a0b26d-fde4-4f6f-8bd5-58cb9a0f8b1e",
		},
		{
			name: "kimi chat completions prompt_cache_key",
			body: `{"model":"kimi-k3","stream":true,"prompt_cache_key":"kimi-sess-4f9c1a2b","messages":[{"role":"user","content":"hi"}]}`,
			want: "kimi-sess-4f9c1a2b",
		},
		{
			name: "prompt_cache_key on responses wire",
			body: `{"model":"gpt-5.4","prompt_cache_key":"resp-sess-77","input":[]}`,
			want: "resp-sess-77",
		},
		{
			name: "kimi anthropic messages metadata.user_id",
			body: `{"model":"kimi-k3","max_tokens":1024,"metadata":{"user_id":"9f86d081884c7d659a2f"},"messages":[{"role":"user","content":"hi"}]}`,
			want: "9f86d081884c7d659a2f",
		},
		{
			// Claude Code's composite user_id measures 134 chars — inside the
			// 256 cap (and header clients never reach this fallback anyway).
			name: "claude-code-shaped composite user_id",
			body: `{"metadata":{"user_id":"user_` + strings.Repeat("a", 40) + `_account_` + "11111111-1111-4111-8111-111111111111" + `_session_` + "22222222-2222-4222-8222-222222222222" + `"}}`,
			want: "user_" + strings.Repeat("a", 40) + "_account_" + "11111111-1111-4111-8111-111111111111" + "_session_" + "22222222-2222-4222-8222-222222222222",
		},
		{
			name: "explicit session_id outranks prompt_cache_key",
			body: `{"client_metadata":{"session_id":"sess-first"},"prompt_cache_key":"key-second","metadata":{"user_id":"uid-third"}}`,
			want: "sess-first",
		},
		{
			name: "prompt_cache_key outranks metadata.user_id",
			body: `{"prompt_cache_key":"key-second","metadata":{"user_id":"uid-third"}}`,
			want: "key-second",
		},
		{
			// A malformed candidate must not mask a well-formed one
			// (encoding/json keeps the fields it could decode).
			name: "numeric user_id does not mask prompt_cache_key",
			body: `{"metadata":{"user_id":42},"prompt_cache_key":"key-ok"}`,
			want: "key-ok",
		},
		{
			name: "string metadata does not mask session_id",
			body: `{"metadata":"opaque","client_metadata":{"session_id":"sess-ok"}}`,
			want: "sess-ok",
		},
		{
			// openai Responses metadata is a free-form map: unrelated keys
			// are ignored, a user_id key still resolves.
			name: "responses metadata map with user_id key",
			body: `{"metadata":{"code_version":3,"user_id":"map-user"},"input":[]}`,
			want: "map-user",
		},
		{name: "no identity fields", body: `{"model":"m","messages":[]}`, want: ""},
		{name: "empty session_id", body: `{"client_metadata":{"session_id":""}}`, want: ""},
		{name: "whitespace-only prompt_cache_key", body: `{"prompt_cache_key":"   "}`, want: ""},
		{name: "non-string session_id", body: `{"client_metadata":{"session_id":42}}`, want: ""},
		{name: "object prompt_cache_key", body: `{"prompt_cache_key":{"k":"v"}}`, want: ""},
		{name: "oversized ignored", body: `{"prompt_cache_key":"` + strings.Repeat("a", 257) + `"}`, want: ""},
		{name: "control byte rejected", body: `{"prompt_cache_key":"a\nb"}`, want: ""},
		{name: "non-ascii rejected", body: `{"metadata":{"user_id":"会话-1"}}`, want: ""},
		{name: "invalid json", body: `{"client_metadata":`, want: ""},
		{
			// The needle gates the parse; a needle deep inside nested content
			// with no top-level field yields nothing (not an error).
			name: "needle only in nested tool schema",
			body: `{"model":"m","tools":[{"function":{"parameters":{"properties":{"user_id":{"type":"string"}}}}}]}`,
			want: "",
		},
		{name: "needle-less body skips parse", body: `not json at all`, want: ""},
	}
	for _, tc := range cases {
		if got := SessionIDFromBody([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: SessionIDFromBody = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestSessionIDFromBodyCapBoundary pins the length cap's boundary semantics:
// a 256-char id survives, 257 does not.
func TestSessionIDFromBodyCapBoundary(t *testing.T) {
	at := `{"prompt_cache_key":"` + strings.Repeat("a", 256) + `"}`
	if got := SessionIDFromBody([]byte(at)); len(got) != 256 {
		t.Errorf("256-char id: got %d chars, want 256", len(got))
	}
	over := `{"prompt_cache_key":"` + strings.Repeat("a", 257) + `"}`
	if got := SessionIDFromBody([]byte(over)); got != "" {
		t.Errorf("257-char id: got %d chars, want rejected", len(got))
	}
}
