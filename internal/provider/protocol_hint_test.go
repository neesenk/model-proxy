package provider

import "testing"

// protocol_hint_test.go — leaf tests for the provider-owned protocol hints
// (protocol_hint.go). Sink moved from internal/app/request_routing_test.go; the
// app side keeps only composition-level coverage (hint synthesis into implicit
// routes / wirecap, see internal/app/modelcaps_test.go).

// TestProtocolHint: codex hints "responses" (it speaks the OpenAI Responses API,
// and a real converter now exists); no other provider hints. No provider carries
// a WireProtocolNote today (codex is now convertible, not "unconvertible").
func TestProtocolHint(t *testing.T) {
	if got := ProtocolHint("codex", "gpt-5.6"); got != "responses" {
		t.Errorf("ProtocolHint(codex) = %q, want %q", got, "responses")
	}
	if got := ProtocolHint("typesafe", "m"); got != "decisions" {
		t.Errorf("ProtocolHint(typesafe) = %q, want %q (decisions-API dialect)", got, "decisions")
	}
	for _, id := range []string{"zhipu", "deepseek", "volcengine", "aqp", "kimi-code", "static", ""} {
		if got := ProtocolHint(id, "m"); got != "" {
			t.Errorf("ProtocolHint(%q) = %q, want %q", id, got, "")
		}
	}
	if note := WireProtocolNote("codex"); note != "" {
		t.Errorf("codex wire note = %q, want \"\" (codex is now convertible to responses)", note)
	}
}
