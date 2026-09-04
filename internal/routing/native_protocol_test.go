package routing

import (
	"testing"

	configdomain "model-proxy/internal/config"
)

func TestNativeProtocols(t *testing.T) {
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{
		"openai-only": {OpenAIBaseURL: "https://x/v1"},
		"both":        {OpenAIBaseURL: "https://x/v1", AnthropicBaseURL: "https://x/anthropic/v1"},
		"claude-ish":  {AnthropicBaseURL: "https://x/anthropic/v1"},
		"codex":       {OpenAIBaseURL: "https://x/v1", Provider: "codex"},
	}}
	cases := []struct {
		name   string
		target configdomain.RouteTarget
		want   map[string]bool
	}{
		{"explicit protocol pins the backend", configdomain.RouteTarget{Provider: "both", Model: "m", Protocol: "responses"}, map[string]bool{"responses": true}},
		{"hint applies when undeclared (codex)", configdomain.RouteTarget{Provider: "codex", Model: "m"}, map[string]bool{"responses": true}},
		{"openai endpoint ⇒ openai native only", configdomain.RouteTarget{Provider: "openai-only", Model: "m"}, map[string]bool{"openai": true}},
		{"anthropic endpoint ⇒ anthropic native only", configdomain.RouteTarget{Provider: "claude-ish", Model: "m"}, map[string]bool{"anthropic": true}},
		{"both endpoints ⇒ both native", configdomain.RouteTarget{Provider: "both", Model: "m"}, map[string]bool{"anthropic": true, "openai": true}},
		{"unknown provider abstains", configdomain.RouteTarget{Provider: "ghost", Model: "m"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := NativeProtocols(cfg, c.target)
			if len(got) != len(c.want) {
				t.Fatalf("NativeProtocols = %v, want %v", got, c.want)
			}
			for proto := range c.want {
				if !got[proto] {
					t.Errorf("NativeProtocols = %v, missing %q", got, proto)
				}
			}
		})
	}
}
