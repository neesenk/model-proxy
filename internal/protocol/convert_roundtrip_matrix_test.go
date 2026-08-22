package protocol

import (
	"strings"
	"testing"

	"github.com/bytedance/sonic"
)

// Round-trip matrix (ported from Switchyard's lossless_roundtrip idea,
// adapted to pairwise codecs): a request converted A→B→…→A must preserve its
// semantic core — system, model, max_tokens, text blocks, tool_use calls and
// tool_result payloads in order. Byte-exactness is NOT expected cross-format
// (shapes legitimately change); the projection below is the contract.

type reqProjection struct {
	system      string
	model       string
	maxTokens   int
	texts       []string
	toolUses    []string // "name|json-input" in order
	toolResults []string // text payloads in order
}

func projectAnthropicRequest(t *testing.T, body []byte) reqProjection {
	t.Helper()
	root := unmarshalMap(t, body)
	p := reqProjection{
		model:     strOf(root["model"]),
		maxTokens: intOf(root["max_tokens"]),
	}
	if sys, ok := root["system"].(string); ok {
		p.system = sys
	}
	for _, m := range asSliceAny(root["messages"]) {
		for _, b := range asSliceAny(asMap(m)["content"]) {
			bm := asMap(b)
			switch bm["type"] {
			case "text", "":
				if s := strOf(bm["text"]); s != "" {
					p.texts = append(p.texts, s)
				}
			case "tool_use":
				input, _ := sonic.Marshal(bm["input"])
				p.toolUses = append(p.toolUses, strOf(bm["name"])+"|"+string(input))
			case "tool_result":
				// tool_result content is legal as a string OR a block array.
				switch content := bm["content"].(type) {
				case string:
					p.toolResults = append(p.toolResults, content)
				default:
					var parts []string
					for _, c := range asSliceAny(content) {
						if cm := asMap(c); cm != nil {
							parts = append(parts, strOf(cm["text"]))
						}
					}
					p.toolResults = append(p.toolResults, strings.Join(parts, "\n"))
				}
			}
		}
	}
	return p
}

func requireEqualProjection(t *testing.T, tag string, before, after reqProjection) {
	t.Helper()
	if before.system != after.system {
		t.Errorf("%s: system %q → %q", tag, before.system, after.system)
	}
	if before.model != after.model {
		t.Errorf("%s: model %q → %q", tag, before.model, after.model)
	}
	if before.maxTokens != after.maxTokens {
		t.Errorf("%s: max_tokens %d → %d", tag, before.maxTokens, after.maxTokens)
	}
	if strings.Join(before.texts, "\x00") != strings.Join(after.texts, "\x00") {
		t.Errorf("%s: texts %v → %v", tag, before.texts, after.texts)
	}
	if strings.Join(before.toolUses, "\x00") != strings.Join(after.toolUses, "\x00") {
		t.Errorf("%s: tool_uses %v → %v", tag, before.toolUses, after.toolUses)
	}
	if strings.Join(before.toolResults, "\x00") != strings.Join(after.toolResults, "\x00") {
		t.Errorf("%s: tool_results %v → %v", tag, before.toolResults, after.toolResults)
	}
}

func roundtripFixture() []byte {
	return []byte(`{"model":"glm-x","max_tokens":128,"system":"be brief","messages":[
		{"role":"user","content":[{"type":"text","text":"hi"}]},
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"search","input":{"q":"x"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"found"}]},{"type":"text","text":"again"}]},
		{"role":"assistant","content":[{"type":"text","text":"done"}]}]}`)
}

// TestRoundtrip_TwoHopCycles: every distinct A→B→A request cycle over the
// three protocols preserves the semantic projection.
func TestRoundtrip_TwoHopCycles(t *testing.T) {
	fixture := roundtripFixture()
	before := projectAnthropicRequest(t, fixture)
	convert := map[[2]string]func([]byte) ([]byte, error){
		{"anthropic", "openai"}:    convertAnthropicRequestToOpenAI,
		{"openai", "anthropic"}:    convertOpenAIRequestToAnthropic,
		{"anthropic", "responses"}: convertAnthropicRequestToResponses,
		{"responses", "anthropic"}: convertResponsesRequestToAnthropic,
		{"openai", "responses"}:    convertOpenAIRequestToResponses,
		{"responses", "openai"}:    convertResponsesRequestToOpenAI,
	}
	cycles := [][2]string{{"anthropic", "openai"}, {"anthropic", "responses"}}
	for _, c := range cycles {
		tag := c[0] + "→" + c[1] + "→" + c[0]
		leg1, err := convert[c](fixture)
		if err != nil {
			t.Fatalf("%s leg1: %v", tag, err)
		}
		back, err := convert[[2]string{c[1], c[0]}](leg1)
		if err != nil {
			t.Fatalf("%s leg2: %v", tag, err)
		}
		requireEqualProjection(t, tag, before, projectAnthropicRequest(t, back))
	}
}

// TestRoundtrip_ThreeHopCycles: both distinct three-format hop orders from
// anthropic back to anthropic preserve the projection (two conversions in a
// row compound loss differently than a single pair).
func TestRoundtrip_ThreeHopCycles(t *testing.T) {
	fixture := roundtripFixture()
	before := projectAnthropicRequest(t, fixture)
	steps := []struct {
		tag  string
		legs []func([]byte) ([]byte, error)
	}{
		{"a→r→chat→a", []func([]byte) ([]byte, error){
			convertAnthropicRequestToResponses, convertResponsesRequestToOpenAI, convertOpenAIRequestToAnthropic,
		}},
		{"a→chat→r→a", []func([]byte) ([]byte, error){
			convertAnthropicRequestToOpenAI, convertOpenAIRequestToResponses, convertResponsesRequestToAnthropic,
		}},
	}
	for _, s := range steps {
		body := fixture
		for i, leg := range s.legs {
			next, err := leg(body)
			if err != nil {
				t.Fatalf("%s leg %d: %v", s.tag, i+1, err)
			}
			body = next
		}
		requireEqualProjection(t, s.tag, before, projectAnthropicRequest(t, body))
	}
}

// TestRoundtrip_ResponseAnthropicChatCycle: a simple text+usage response
// survives the a→chat→a cycle semantically (content, stop_reason, usage).
func TestRoundtrip_ResponseAnthropicChatCycle(t *testing.T) {
	in := []byte(`{"id":"msg_1","model":"claude-x","role":"assistant","max_tokens":64,"stop_reason":"end_turn","content":[{"type":"text","text":"hello world"}],"usage":{"input_tokens":5,"output_tokens":3}}`)
	leg1, err := convertAnthropicResponseToOpenAI(in)
	if err != nil {
		t.Fatal(err)
	}
	back, err := convertOpenAIResponseToAnthropic(leg1)
	if err != nil {
		t.Fatal(err)
	}
	root := unmarshalMap(t, back)
	if got := strOf(asMap(asSliceAny(root["content"])[0])["text"]); got != "hello world" {
		t.Errorf("roundtrip text = %q", got)
	}
	if root["stop_reason"] != "end_turn" {
		t.Errorf("roundtrip stop_reason = %v", root["stop_reason"])
	}
	usage := asMap(root["usage"])
	if intOf(usage["input_tokens"]) != 5 || intOf(usage["output_tokens"]) != 3 {
		t.Errorf("roundtrip usage = %v", usage)
	}
}
