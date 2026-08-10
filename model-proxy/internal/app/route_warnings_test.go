package app

import (
	"strings"
	"testing"

	"model-proxy/provider"
)

// TestProtocolHint: codex hints "responses" (it speaks the OpenAI Responses API,
// and a real converter now exists); no other provider hints. No provider carries
// a WireProtocolNote today (codex is now convertible, not "unconvertible").
func TestProtocolHint(t *testing.T) {
	if got := provider.ProtocolHint("codex", "gpt-5.6"); got != "responses" {
		t.Errorf("ProtocolHint(codex) = %q, want \"responses\"", got)
	}
	for _, id := range []string{"zhipu", "deepseek", "volcengine", "aqp", "kimi-code", "static", ""} {
		if got := provider.ProtocolHint(id, "m"); got != "" {
			t.Errorf("ProtocolHint(%q) = %q, want \"\"", id, got)
		}
	}
	if note := provider.WireProtocolNote("codex"); note != "" {
		t.Errorf("codex wire note = %q, want \"\" (codex is now convertible to responses)", note)
	}
}

// TestImplicitRoute_ProtocolHintFilled: implicit codex routes auto-declare
// protocol:"responses" (so an anthropic/chat client is converted, not left to
// send a body codex rejects); non-codex providers stay unset.
func TestImplicitRoute_ProtocolHintFilled(t *testing.T) {
	cfg := &Config{Providers: map[string]Provider{
		"codex": {Provider: "codex", OpenAIBaseURL: "https://x", Models: []string{"gpt-5.6"}},
		"zhipu": {Provider: "zhipu", OpenAIBaseURL: "https://y", Models: []string{"glm-5"}},
	}}
	implicit, _ := synthesizeImplicitRoutesFrom(cfg, map[string]bool{"codex": true, "zhipu": true})
	if got := implicit["gpt-5.6"].Protocol; got != "responses" {
		t.Errorf("codex implicit protocol = %q, want \"responses\"", got)
	}
	if got := implicit["glm-5"].Protocol; got != "" {
		t.Errorf("zhipu implicit protocol = %q, want unset", got)
	}
}

// TestConfigRoutingWarnings: the reasoning-replay marker fires precisely for a
// reasoning model behind a declared protocol conversion. A codex target WITHOUT
// protocol: does NOT warn — the forward path auto-resolves it to responses
// (resolvedBackendProto/ProtocolHint), so it converts without user action.
func TestConfigRoutingWarnings(t *testing.T) {
	cfg := &Config{Providers: map[string]Provider{
		"codex": {Provider: "codex", OpenAIBaseURL: "https://x"},
		"aqp":   {Provider: "aqp", OpenAIBaseURL: "https://y"},
	}}
	expanded := map[string][]RouteTarget{
		"k2":        {{Provider: "aqp", Model: "kimi-k2-thinking", Protocol: "openai"}},
		"plain":     {{Provider: "aqp", Model: "glm-5", Protocol: "openai"}},
		"no-proto":  {{Provider: "codex", Model: "gpt-5.6"}},         // auto-resolves → no warning
		"reason-ok": {{Provider: "aqp", Model: "deepseek-reasoner"}}, // no conversion → no warning
		// responses targets preserve reasoning, so the "currently dropped"
		// message would be a false positive for them.
		"resp-think": {{Provider: "codex", Model: "kimi-k2-thinking", Protocol: "responses"}},
	}
	warns := ConfigRoutingWarnings(cfg, expanded)
	joined := strings.Join(warns, "\n")
	if !strings.Contains(joined, `route "k2"`) || !strings.Contains(joined, "reasoning-required") {
		t.Errorf("missing reasoning marker, warns = %v", warns)
	}
	if strings.Contains(joined, `route "no-proto"`) {
		t.Errorf("codex auto-resolves (no protocol: needed) — must NOT warn, warns = %v", warns)
	}
	if strings.Contains(joined, `route "resp-think"`) {
		t.Errorf("responses targets preserve reasoning — must NOT warn, warns = %v", warns)
	}
	if strings.Contains(joined, `route "plain"`) || strings.Contains(joined, `route "reason-ok"`) {
		t.Errorf("false positive, warns = %v", warns)
	}
	if len(warns) != 1 {
		t.Errorf("len(warns) = %d, want 1 (only the k2 reasoning marker; codex auto-resolves): %v", len(warns), warns)
	}
}

// TestNewProxy_RouteWarningsAppended: boot-time wiring — hazard warnings land
// on p.routeWarnings (surfaced via /api/status + `models` CLI).
func TestNewProxy_RouteWarningsAppended(t *testing.T) {
	cfg := &Config{Providers: map[string]Provider{
		"aqp": {Provider: "aqp", OpenAIBaseURL: "https://y"},
	}, Routes: map[string][]RouteTarget{
		"k2": {{Provider: "aqp", Model: "kimi-k2-thinking", Protocol: "openai"}},
	}}
	p := newTestProxy(t, cfg)
	found := false
	for _, w := range p.routeWarnings {
		if strings.Contains(w, "reasoning-required") {
			found = true
		}
	}
	if !found {
		t.Errorf("routeWarnings missing the reasoning marker: %v", p.routeWarnings)
	}
}
