package wire

import "testing"

func TestParseClosedSet(t *testing.T) {
	for _, want := range []Protocol{Anthropic, OpenAI, Responses} {
		got, ok := Parse(string(want))
		if !ok || got != want {
			t.Errorf("Parse(%q) = (%q, %v), want (%q, true)", want, got, ok, want)
		}
	}
	for _, bad := range []string{"", "ANTHROPIC", "openai-chat", "gemini", " anthropic"} {
		if got, ok := Parse(bad); ok || got != "" {
			t.Errorf("Parse(%q) = (%q, %v), want (\"\", false)", bad, got, ok)
		}
	}
}
