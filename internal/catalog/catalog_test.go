package catalog

import "testing"

const fixture = `{
  "zhipuai": {"models": {
    "glm-4.6": {"limit":{"context":204800,"output":131072},"modalities":{"input":["text"],"output":["text"]}}
  }},
  "openrouter": {"models": {
    "glm-4.6": {"limit":{"context":999,"output":999},"modalities":{"input":["text"],"output":["text"]}}
  }},
  "anthropic": {"models": {
    "claude-sonnet": {"limit":{"context":200000,"output":8192},"modalities":{"input":["text","image"],"output":["text"]},"features":{"tool_call":true}},
    "claude-haiku": {"limit":{"context":200000,"output":4096},"modalities":{"input":["text"],"output":["text"]}}
  }}
}`

func TestParseCanonicalOwnerAndToolCall(t *testing.T) {
	cat, err := parse([]byte(fixture))
	if err != nil {
		t.Fatal(err)
	}
	if cat.Count() != 3 {
		t.Fatalf("count=%d want 3", cat.Count())
	}
	glm, ok := cat.Lookup("glm-4.6")
	if !ok || glm.Context != 204800 || glm.Output != 131072 {
		t.Fatalf("canonical glm metadata=%+v ok=%v", glm, ok)
	}
	sonnet, ok := cat.Lookup("claude-sonnet")
	if !ok || !sonnet.ToolCall || len(sonnet.Modalities.Input) != 2 {
		t.Fatalf("tool-capable metadata=%+v ok=%v", sonnet, ok)
	}
	haiku, ok := cat.Lookup("claude-haiku")
	if !ok || haiku.ToolCall {
		t.Fatalf("missing tool_call should be false: %+v ok=%v", haiku, ok)
	}
	if _, ok := cat.Lookup("missing"); ok {
		t.Fatal("unknown model unexpectedly found")
	}
	if _, ok := (*Catalog)(nil).Lookup("anything"); ok {
		t.Fatal("nil catalog unexpectedly found a model")
	}
}

func TestNewAndLookupDefensivelyCopy(t *testing.T) {
	input := map[string]Model{"m": {Modalities: Modalities{Input: []string{"text"}, Output: []string{"text"}}}}
	cat := New(input)
	input["m"] = Model{Context: 1}
	got, ok := cat.Lookup("m")
	if !ok || got.Context != 0 || got.Modalities.Input[0] != "text" {
		t.Fatalf("New retained caller data: %+v", got)
	}
	got.Modalities.Input[0] = "image"
	again, _ := cat.Lookup("m")
	if again.Modalities.Input[0] != "text" {
		t.Fatalf("Lookup returned aliased slice: %+v", again)
	}
	if (*Catalog)(nil).Count() != 0 || (*Catalog)(nil).ETag() != "" {
		t.Fatal("nil catalog accessors must be zero-safe")
	}
}

// Regression: provider folding used `for provider := range raw`, so two
// equal-rank providers defining the same model name could win depending on Go's
// random map iteration order. The sorted-first provider must always win,
// deterministically, for equal-rank collisions.
func TestParseEqualRankCollisionIsDeterministic(t *testing.T) {
	conflict := `{
	  "aaa-mirror": {"models": {
	    "shared-model": {"limit":{"context":111,"output":11},"modalities":{"input":["text"],"output":["text"]}}
	  }},
	  "zzz-mirror": {"models": {
	    "shared-model": {"limit":{"context":999,"output":99},"modalities":{"input":["text"],"output":["text"]}}
	  }}
	}`
	for range 10 {
		cat, err := parse([]byte(conflict))
		if err != nil {
			t.Fatal(err)
		}
		m, ok := cat.Lookup("shared-model")
		if !ok || m.Context != 111 || m.Output != 11 {
			t.Fatalf("equal-rank collision winner=%+v ok=%v, want sorted-first provider aaa-mirror (111/11) every run", m, ok)
		}
	}
	// Canonical rank still beats sort order (openrouter is not canonical; zhipuai is).
	cat, err := parse([]byte(`{
	  "openrouter": {"models": {"glm-9.9": {"limit":{"context":999,"output":99}}}},
	  "zhipuai": {"models": {"glm-9.9": {"limit":{"context":204800,"output":131072}}}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := cat.Lookup("glm-9.9"); m.Context != 204800 {
		t.Fatalf("canonical owner lost to sorted-first reseller: %+v", m)
	}
}

func TestParseReasoning(t *testing.T) {
	c, err := parse([]byte(`{"p": {"models": {
		"m-think": {"reasoning": true, "limit": {"context": 1, "output": 1}},
		"m-plain": {"limit": {"context": 1, "output": 1}}
	}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := c.Lookup("m-think"); !m.Reasoning {
		t.Error("m-think Reasoning = false, want true")
	}
	if m, _ := c.Lookup("m-plain"); m.Reasoning {
		t.Error("m-plain Reasoning = true, want false")
	}
	// Disk round-trip must preserve it.
	data, err := c.marshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	c2, err := unmarshalCatalog(data)
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := c2.Lookup("m-think"); !m.Reasoning {
		t.Error("Reasoning lost across disk round-trip")
	}
}

// TestParseTopLevelToolCallAndEfforts pins the CURRENT models.dev api.json
// shape: tool_call lives at the model object's top level (the nested
// features.tool_call shape in the main fixture is the legacy fallback), and
// reasoning_options type "effort" values project onto ReasoningEfforts with
// "none" filtered (it is the thinking-off switch, not an effort level).
func TestParseTopLevelToolCallAndEfforts(t *testing.T) {
	fixture := `{
  "kimi-for-coding": {"models": {
    "k3": {"limit":{"context":1048576,"output":131072},"modalities":{"input":["text","image","video"],"output":["text"]},
           "reasoning":true,"tool_call":true,
           "reasoning_options":[{"type":"toggle"},{"type":"effort","values":["low","high","max"]}]},
    "kimi-for-coding-highspeed": {"limit":{"context":262144,"output":32768},"modalities":{"input":["text","image"],"output":["text"]},
           "reasoning":true,"tool_call":true,"reasoning_options":[]},
    "plain": {"limit":{"context":1000,"output":100},"modalities":{"input":["text"],"output":["text"]}}
  }}
}`
	cat, err := parse([]byte(fixture))
	if err != nil {
		t.Fatal(err)
	}
	k3, _ := cat.Lookup("k3")
	if !k3.ToolCall || !k3.Reasoning {
		t.Fatalf("k3 top-level flags lost: %+v", k3)
	}
	want := []string{"low", "high", "max"}
	if len(k3.ReasoningEfforts) != len(want) {
		t.Fatalf("k3 efforts = %v, want %v", k3.ReasoningEfforts, want)
	}
	for i := range want {
		if k3.ReasoningEfforts[i] != want[i] {
			t.Fatalf("k3 efforts = %v, want %v", k3.ReasoningEfforts, want)
		}
	}
	hs, _ := cat.Lookup("kimi-for-coding-highspeed")
	if len(hs.ReasoningEfforts) != 0 {
		t.Fatalf("highspeed efforts = %v, want none (empty reasoning_options)", hs.ReasoningEfforts)
	}
	plain, _ := cat.Lookup("plain")
	if plain.ToolCall || plain.Reasoning || len(plain.ReasoningEfforts) != 0 {
		t.Fatalf("plain model must stay capability-free: %+v", plain)
	}

	// "none" is filtered from effort values.
	noneFiltered := `{"p":{"models":{"m":{"limit":{"context":1,"output":1},
	  "reasoning_options":[{"type":"effort","values":["none","low","high"]}]}}}}`
	cat2, err := parse([]byte(noneFiltered))
	if err != nil {
		t.Fatal(err)
	}
	m, _ := cat2.Lookup("m")
	if len(m.ReasoningEfforts) != 2 || m.ReasoningEfforts[0] != "low" || m.ReasoningEfforts[1] != "high" {
		t.Fatalf(`efforts with "none" = %v, want [low high]`, m.ReasoningEfforts)
	}
}
