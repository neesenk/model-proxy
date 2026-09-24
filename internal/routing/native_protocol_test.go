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

// TestChatReachableRoutes_DerivedRoutesDropsDecisionsOnly covers the
// production path (RouteTable → ChatReachableRoutes): a typesafe provider's
// models — whether configured with a pure decisions base or the
// openai_base_url gateway fallback form — are decisions-only (ProtocolHint
// short-circuits before base-URL declarations) and must be dropped, while
// chat models survive.
func TestChatReachableRoutes_DerivedRoutesDropsDecisionsOnly(t *testing.T) {
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{
		"zhipu":   {OpenAIBaseURL: "https://z/v1", Models: []string{"glm-5.3"}},
		"ts-pure": {Provider: "typesafe", DecisionsBaseURL: "https://ts/v1", Models: []string{"jev-pure"}},
		"ts-gw":   {Provider: "typesafe", OpenAIBaseURL: "https://gw/v1", Models: []string{"jev-gw"}},
	}}
	kept, dropped := ChatReachableRoutes(cfg, DeriveRoutesFrom(cfg))
	if len(dropped) != 2 || dropped[0] != "jev-gw" || dropped[1] != "jev-pure" {
		t.Fatalf("dropped = %v, want [jev-gw jev-pure]", dropped)
	}
	if _, ok := kept["glm-5.3"]; !ok {
		t.Errorf("chat model glm-5.3 must stay reachable, kept = %v", kept)
	}
	if _, ok := kept["jev-pure"]; ok {
		t.Errorf("decisions-only model jev-pure must be dropped, kept = %v", kept)
	}
}

// TestChatReachableRoutes_ExplicitTargets covers the explicit-route edge
// cases: a pinned protocol wins in both directions, a chat failover target
// rescues an otherwise decisions-only route, and abstaining targets (unknown
// provider, empty route) stay reachable.
func TestChatReachableRoutes_ExplicitTargets(t *testing.T) {
	cfg := &configdomain.Config{Providers: map[string]configdomain.Provider{
		"chat":    {OpenAIBaseURL: "https://x/v1"},
		"ts-pure": {Provider: "typesafe", DecisionsBaseURL: "https://ts/v1"},
	}}
	routes := map[string][]configdomain.RouteTarget{
		"pinned-decisions": {{Provider: "chat", Model: "m", Protocol: "decisions"}},
		"pinned-chat":      {{Provider: "ts-pure", Model: "m", Protocol: "openai"}},
		"mixed-failover":   {{Provider: "ts-pure", Model: "m"}, {Provider: "chat", Model: "m"}},
		"unknown-provider": {{Provider: "ghost", Model: "m"}},
		"no-targets":       {},
	}
	kept, dropped := ChatReachableRoutes(cfg, routes)
	if len(dropped) != 1 || dropped[0] != "pinned-decisions" {
		t.Fatalf("dropped = %v, want [pinned-decisions]", dropped)
	}
	for _, name := range []string{"pinned-chat", "mixed-failover", "unknown-provider", "no-targets"} {
		if _, ok := kept[name]; !ok {
			t.Errorf("%s must stay reachable, kept = %v", name, kept)
		}
	}
}
