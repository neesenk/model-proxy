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
