package takeover

import (
	"testing"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/routing"
	runtimewire "model-proxy/internal/runtime/wirecap"
)

// protocol_consistency_test.go — executable form of the "layers must never
// disagree" contract on native_protocol.go: over the full cross-product of
// client protocol × anthropic-base presence × per-leg verdicts, the runtime
// decision matrix (runtimewire.ResolveModel — what forward uses live) must
// never pass a request through a leg whose probe verdict is a concluded no,
// and routing.NativeProtocolsWithVerdict (what takeover uses offline) must
// never claim such a leg native. Verdict-unknown legs may legitimately differ
// (forward probes/passthroughs while pending, takeover abstains) — only
// concluded negatives are binding on both layers.

func consistencyConfig(hasAnthropicBase bool) *configdomain.Config {
	p := configdomain.Provider{Provider: "openai-compatible", OpenAIBaseURL: "https://api.example.com"}
	if hasAnthropicBase {
		p.AnthropicBaseURL = "https://claude.example.com"
	}
	return &configdomain.Config{
		Providers: map[string]configdomain.Provider{"p": p},
	}
}

// TestProtocolLayersNeverPassThroughProbedNoLegs enumerates the full verdict
// cross-product and asserts the one-directional binding contract for every
// concluded-no leg.
func TestProtocolLayersNeverPassThroughProbedNoLegs(t *testing.T) {
	verdicts := []runtimewire.Verdict{runtimewire.Unknown, runtimewire.Yes, runtimewire.No}
	clientProtos := []string{"anthropic", "responses", "openai"}
	for _, clientProto := range clientProtos {
		for _, hasBase := range []bool{true, false} {
			cfg := consistencyConfig(hasBase)
			target := configdomain.RouteTarget{Provider: "p", Model: "m"}
			for _, chat := range verdicts {
				for _, anth := range verdicts {
					for _, resp := range verdicts {
						mc := runtimewire.ModelProtocols{Chat: chat, Anthropic: anth, Responses: resp}
						v := &routing.ModelProtocolVerdict{Chat: triOf(chat), Anthropic: triOf(anth), Responses: triOf(resp)}

						native := routing.NativeProtocolsWithVerdict(cfg, target, v)
						got, viaResponses := runtimewire.ResolveModel(clientProto, hasBase, mc, true,
							runtimewire.Capabilities{BaseURL: "https://api.example.com"}, true)

						for leg, verdict := range map[string]runtimewire.Verdict{
							"openai":    chat,
							"anthropic": anth,
							"responses": resp,
						} {
							if verdict != runtimewire.No {
								continue
							}
							if native[leg] {
								t.Errorf("takeover claims %s native despite probed no (client=%s base=%v verdicts=%+v)",
									leg, clientProto, hasBase, mc)
							}
							passthrough := got == clientProto && !viaResponses
							// A probed-no leg may only be ridden when NO probed-yes
							// or unconcluded alternative exists (all-no: forward
							// deliberately lets the upstream error surface —
							// intentional behavior #31).
							hasAlternative := chat != runtimewire.No || anth != runtimewire.No || resp != runtimewire.No
							if passthrough && got == leg && hasAlternative {
								t.Errorf("forward passthroughs %s despite probed no while an alternative leg exists (client=%s base=%v verdicts=%+v got=%q)",
									leg, clientProto, hasBase, mc, got)
							}
						}
					}
				}
			}
		}
	}
}
