package main

import (
	"strings"
	"testing"

	"model-proxy/provider"
)

// TestProtocolHint: no provider currently hints (codex's wrong "openai" hint
// was retracted); codex instead carries the honest wire-protocol note.
func TestProtocolHint(t *testing.T) {
	for _, id := range []string{"codex", "zhipu", "deepseek", "volcengine", "aqp", "kimi-code", "static", ""} {
		if got := provider.ProtocolHint(id, "m"); got != "" {
			t.Errorf("ProtocolHint(%q) = %q, want \"\"", id, got)
		}
	}
	if note := provider.WireProtocolNote("codex"); !strings.Contains(note, "Responses") {
		t.Errorf("codex wire note = %q, want a Responses-API marker", note)
	}
	if note := provider.WireProtocolNote("zhipu"); note != "" {
		t.Errorf("zhipu wire note = %q, want \"\"", note)
	}
}

// TestImplicitRoute_ProtocolHintFilled: implicit routes get no protocol today
// (codex's hint was retracted — a chat conversion does not make codex usable).
func TestImplicitRoute_ProtocolHintFilled(t *testing.T) {
	cfg := &Config{Providers: map[string]Provider{
		"codex": {Provider: "codex", OpenAIBaseURL: "https://x", Models: []string{"gpt-5.6"}},
		"zhipu": {Provider: "zhipu", OpenAIBaseURL: "https://y", Models: []string{"glm-5"}},
	}}
	implicit, _ := synthesizeImplicitRoutesFrom(cfg, map[string]bool{"codex": true, "zhipu": true})
	if got := implicit["gpt-5.6"].Protocol; got != "" {
		t.Errorf("codex implicit protocol = %q, want unset (no valid hint)", got)
	}
	if got := implicit["glm-5"].Protocol; got != "" {
		t.Errorf("zhipu implicit protocol = %q, want unset", got)
	}
}

// TestConfigRoutingWarnings: the marker classes fire precisely — (1)
// reasoning-replay model behind a declared protocol conversion, (2) the codex
// wire-protocol note (honest marker, not a conversion suggestion).
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
	if !strings.Contains(joined, `route "no-proto"`) || !strings.Contains(joined, "Responses") {
		t.Errorf("missing codex wire note, warns = %v", warns)
	}
	if strings.Contains(joined, "add protocol:") {
		t.Errorf("the retracted conversion suggestion must not appear, warns = %v", warns)
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
