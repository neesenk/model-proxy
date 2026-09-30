package forward

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// sessionEchoUpstream answers with an anthropic-shaped 200 and records the
// session-affinity headers of every request it receives.
type sessionEchoUpstream struct {
	*fakeUpstream
	mu              sync.Mutex
	resolvedSession []string
	opencodeSession []string
}

func newSessionEchoUpstream(t *testing.T) *sessionEchoUpstream {
	t.Helper()
	u := &sessionEchoUpstream{}
	u.fakeUpstream = newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.resolvedSession = append(u.resolvedSession, r.Header.Get("x-test-resolved-session"))
		u.opencodeSession = append(u.opencodeSession, r.Header.Get("x-opencode-session"))
		u.mu.Unlock()
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	})
	return u
}

func (u *sessionEchoUpstream) sessions() (resolved, opencode []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.resolvedSession...), append([]string(nil), u.opencodeSession...)
}

// TestServeExtraHeadersSessionIDStableAcrossConversion: an agent that carries
// its session identity ONLY in a body spec field (Kimi Code's
// prompt_cache_key, MoonshotAI/kimi-code#3506) hits a cross-protocol target.
// The converted upstream body drops that field, so a provider re-deriving the
// identity from the final body would land on a per-request random fallback
// and shard one conversation across upstream prompt-cache lanes. The executor
// must hand ExtraHeaders the client session resolved ONCE from the ORIGINAL
// body: stable across requests and equal to clientSession.
func TestServeExtraHeadersSessionIDStableAcrossConversion(t *testing.T) {
	h := newHarness()
	up := newSessionEchoUpstream(t)
	cfg := &Config{
		Providers: map[string]Provider{
			"a": {AnthropicBaseURL: up.srv.URL, Provider: "test-static"},
		},
		Routes: map[string][]RouteTarget{
			"m": {{Provider: "a", Model: "ma", Protocol: "anthropic"}},
		},
	}
	snap := h.snapshot(cfg)
	body := `{"model":"m","prompt_cache_key":"kimi-sess-9a2f","messages":[{"role":"user","content":"hi"}]}`

	for i := 0; i < 2; i++ {
		w := h.serve(snap, "openai", "/v1/chat/completions", body, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200 (body %s)", i, w.Code, w.Body.String())
		}
	}
	if up.hits() != 2 {
		t.Fatalf("upstream hits = %d, want 2", up.hits())
	}
	// The leg really was protocol-converted: the original session field did
	// NOT survive into the upstream body, so only the passed session id can
	// keep the lane stable.
	if strings.Contains(up.lastBody(), "prompt_cache_key") {
		t.Fatalf("converted upstream body unexpectedly kept prompt_cache_key: %s", up.lastBody())
	}
	resolved, _ := up.sessions()
	for i, got := range resolved {
		if got != "kimi-sess-9a2f" {
			t.Fatalf("request %d: ExtraHeaders session = %q, want the original body identity %q (stable across the conversation)", i, got, "kimi-sess-9a2f")
		}
	}
}

// TestServeUpstreamWhitelistPassesOpenCodeSession: a client that sets
// x-opencode-session explicitly must have it forwarded — the "explicit client
// value always wins" rule in OpenCodeGoProvider.ExtraHeaders is only
// reachable when the client→upstream whitelist carries the header.
func TestServeUpstreamWhitelistPassesOpenCodeSession(t *testing.T) {
	h := newHarness()
	up := newSessionEchoUpstream(t)
	cfg := &Config{
		Providers: map[string]Provider{
			"a": {AnthropicBaseURL: up.srv.URL, Provider: "test-static"},
		},
		Routes: map[string][]RouteTarget{
			"m": {{Provider: "a", Model: "ma", Protocol: "anthropic"}},
		},
	}
	snap := h.snapshot(cfg)
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`

	w := h.serve(snap, "openai", "/v1/chat/completions", body, map[string]string{"x-opencode-session": "sess-explicit"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	_, opencode := up.sessions()
	if len(opencode) != 1 || opencode[0] != "sess-explicit" {
		t.Fatalf("upstream x-opencode-session = %v, want the client's explicit value forwarded", opencode)
	}
}
