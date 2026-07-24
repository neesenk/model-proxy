package main

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

// TestConfigRoutingWarnings: the marker classes fire precisely — (1)
// reasoning-replay model behind a declared protocol conversion, (2) an explicit
// codex target missing its protocol: declaration (now a "add protocol: responses"
// nudge, since codex is convertible when declared).
func TestConfigRoutingWarnings(t *testing.T) {
	cfg := &Config{Providers: map[string]Provider{
		"codex": {Provider: "codex", OpenAIBaseURL: "https://x"},
		"aqp":   {Provider: "aqp", OpenAIBaseURL: "https://y"},
	}}
	expanded := map[string][]RouteTarget{
		"k2":        {{Provider: "aqp", Model: "kimi-k2-thinking", Protocol: "openai"}},
		"plain":     {{Provider: "aqp", Model: "glm-5", Protocol: "openai"}},
		"no-proto":  {{Provider: "codex", Model: "gpt-5.6"}},
		"reason-ok": {{Provider: "aqp", Model: "deepseek-reasoner"}}, // no conversion → no warning
	}
	warns := configRoutingWarnings(cfg, expanded)
	joined := strings.Join(warns, "\n")
	if !strings.Contains(joined, `route "k2"`) || !strings.Contains(joined, "reasoning-required") {
		t.Errorf("missing reasoning marker, warns = %v", warns)
	}
	if !strings.Contains(joined, `route "no-proto"`) || !strings.Contains(joined, "add protocol: responses") {
		t.Errorf("missing codex add-protocol nudge, warns = %v", warns)
	}
	if strings.Contains(joined, `route "plain"`) || strings.Contains(joined, `route "reason-ok"`) {
		t.Errorf("false positive, warns = %v", warns)
	}
	if len(warns) != 2 {
		t.Errorf("len(warns) = %d, want 2: %v", len(warns), warns)
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
