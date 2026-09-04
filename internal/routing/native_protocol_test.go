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

func TestNativeProtocolsWithVerdict(t *testing.T) {
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{
		"both":        {OpenAIBaseURL: "https://x/v1", AnthropicBaseURL: "https://x/anthropic/v1"},
		"openai-only": {OpenAIBaseURL: "https://x/v1"},
	}}
	v := func(chat, anth, resp Tri) *ModelProtocolVerdict {
		return &ModelProtocolVerdict{Chat: chat, Anthropic: anth, Responses: resp}
	}
	cases := []struct {
		name   string
		target configdomain.RouteTarget
		v      *ModelProtocolVerdict
		want   map[string]bool
	}{
		{"nil verdict = static (both endpoints)", configdomain.RouteTarget{Provider: "both", Model: "m"}, nil, map[string]bool{"anthropic": true, "openai": true}},
		{"probe no overrides declared endpoint", configdomain.RouteTarget{Provider: "both", Model: "m"}, v(TriNo, TriYes, TriUnknown), map[string]bool{"anthropic": true}},
		{"probe yes claims responses statically unclaimable", configdomain.RouteTarget{Provider: "openai-only", Model: "m"}, v(TriYes, TriNo, TriYes), map[string]bool{"openai": true, "responses": true}},
		{"probe no on undeclared endpoint stays empty", configdomain.RouteTarget{Provider: "openai-only", Model: "m"}, v(TriYes, TriNo, TriNo), map[string]bool{"openai": true}},
		{"unknown leg falls back to declaration", configdomain.RouteTarget{Provider: "both", Model: "m"}, v(TriUnknown, TriNo, TriUnknown), map[string]bool{"openai": true}},
		{"explicit protocol ignores verdict", configdomain.RouteTarget{Provider: "both", Model: "m", Protocol: "responses"}, v(TriYes, TriYes, TriNo), map[string]bool{"responses": true}},
		{"all legs no = empty set (abstain)", configdomain.RouteTarget{Provider: "both", Model: "m"}, v(TriNo, TriNo, TriNo), map[string]bool{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := NativeProtocolsWithVerdict(cfg, c.target, c.v)
			if len(got) != len(c.want) {
				t.Fatalf("NativeProtocolsWithVerdict = %v, want %v", got, c.want)
			}
			for proto := range c.want {
				if !got[proto] {
					t.Errorf("NativeProtocolsWithVerdict = %v, missing %q", got, proto)
				}
			}
		})
	}
}
