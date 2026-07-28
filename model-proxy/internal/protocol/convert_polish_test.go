package protocol

import (
	"bytes"
	"io"
	"log"
	"strings"
	"testing"
)

// captureConvertLog runs fn with the log output captured and the convertWarn
// dedup map cleared, returning everything convertWarn emitted. The map is
// mutated in place (sync.Map must not be copied): keys are saved, deleted,
// then restored.
func captureConvertLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	oldOut := log.Writer()
	oldFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	var saved []string
	convertWarnSeen.Range(func(k, _ any) bool {
		if s, ok := k.(string); ok {
			saved = append(saved, s)
		}
		convertWarnSeen.Delete(k)
		return true
	})
	defer func() {
		log.SetOutput(oldOut)
		log.SetFlags(oldFlags)
		convertWarnSeen.Range(func(k, _ any) bool {
			convertWarnSeen.Delete(k)
			return true
		})
		for _, k := range saved {
			convertWarnSeen.Store(k, struct{}{})
		}
	}()
	fn()
	return buf.String()
}

// TestSanitizeToolUseID: legal ids pass through untouched, illegal characters
// become "_", an empty id gets a stable toolu_ placeholder, and mapping is
// deterministic (tool_use/tool_result pairing stays joinable).
func TestSanitizeToolUseID(t *testing.T) {
	if got := sanitizeToolUseID("call_abc-DEF_123"); got != "call_abc-DEF_123" {
		t.Errorf("legal id rewritten: %q", got)
	}
	if got := sanitizeToolUseID("functions.Bash:0"); got != "functions_Bash_0" {
		t.Errorf("illegal chars = %q want functions_Bash_0", got)
	}
	if got := sanitizeToolUseID("a b/c.d:e"); got != "a_b_c_d_e" {
		t.Errorf("mixed illegal chars = %q want a_b_c_d_e", got)
	}
	// F2 fix: two empty ids must produce DIFFERENT sanitized ids (unique counter),
	// not the same sha256(nil) constant — a collision breaks tool_use↔tool_result
	// pairing when a response has multiple empty-id tool calls.
	e1, e2 := sanitizeToolUseID(""), sanitizeToolUseID("")
	if e1 == e2 || !strings.HasPrefix(e1, "toolu_empty_") || !strings.HasPrefix(e2, "toolu_empty_") {
		t.Errorf("empty ids = %q/%q want distinct toolu_empty_<n>", e1, e2)
	}
	if sanitizeToolUseID("functions.Bash:0") != sanitizeToolUseID("functions.Bash:0") {
		t.Error("same raw id must always map to the same sanitized id")
	}
}

// TestConvertRequest_OpenAIToAnthropic_ToolIDSanitize: within one conversion the
// assistant tool_use id and its tool message's tool_use_id map to the SAME
// sanitized id (per-call memo); an empty id gets the toolu_ placeholder on both.
func TestConvertRequest_OpenAIToAnthropic_ToolIDSanitize(t *testing.T) {
	in := []byte(`{"model":"gpt","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","tool_calls":[{"id":"functions.Bash:0","type":"function","function":{"name":"Bash","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"functions.Bash:0","content":"ok"}
	]}`)
	out, err := convertOpenAIRequestToAnthropic(in)
	if err != nil {
		t.Fatal(err)
	}
	o := mustJSON(t, out)
	msgs := objOf(t, o, "messages").([]any)
	tu := objOf(t, msgs[1], "content").([]any)[0].(map[string]any)
	tr := objOf(t, msgs[2], "content").([]any)[0].(map[string]any)
	if tu["id"] != "functions_Bash_0" {
		t.Errorf("tool_use id = %v want functions_Bash_0", tu["id"])
	}
	if tr["tool_use_id"] != tu["id"] {
		t.Errorf("tool_result id %v != tool_use id %v (pairing broken)", tr["tool_use_id"], tu["id"])
	}

	// Empty ids on both sides pair up via the same placeholder.
	in2 := []byte(`{"model":"gpt","messages":[
		{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"Bash","arguments":"{}"}}]},
		{"role":"tool","content":"ok"}
	]}`)
	out2, err := convertOpenAIRequestToAnthropic(in2)
	if err != nil {
		t.Fatal(err)
	}
	o2 := mustJSON(t, out2)
	msgs2 := objOf(t, o2, "messages").([]any)
	var tu2, tr2 map[string]any
	for _, m := range msgs2 {
		mm := m.(map[string]any)
		for _, b := range mm["content"].([]any) {
			bm := b.(map[string]any)
			if bm["type"] == "tool_use" {
				tu2 = bm
			}
			if bm["type"] == "tool_result" {
				tr2 = bm
			}
		}
	}
	if tu2 == nil || tr2 == nil {
		t.Fatalf("missing tool_use/tool_result in: %+v", msgs2)
	}
	id2, _ := tu2["id"].(string)
	if !strings.HasPrefix(id2, "toolu_") || tr2["tool_use_id"] != id2 {
		t.Errorf("empty-id pairing: tool_use id=%v tool_result id=%v", tu2["id"], tr2["tool_use_id"])
	}
}

// TestConvertResponse_ToolIDSanitize: response-side ids are sanitized too — the
// non-streaming tool_use block and the streaming tool block start (clients echo
// these ids back in history).
func TestConvertResponse_ToolIDSanitize(t *testing.T) {
	oaiResp := []byte(`{"id":"a","model":"gpt","choices":[{"message":{"role":"assistant","tool_calls":[{"id":"functions.Bash:0","type":"function","function":{"name":"Bash","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`)
	ant, err := convertOpenAIResponseToAnthropic(oaiResp)
	if err != nil {
		t.Fatal(err)
	}
	tu := objOf(t, mustJSON(t, ant), "content").([]any)[0].(map[string]any)
	if tu["id"] != "functions_Bash_0" {
		t.Errorf("non-stream tool_use id = %v want functions_Bash_0", tu["id"])
	}

	stream := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"x.y:2\",\"function\":{\"name\":\"f\",\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"
	out, _ := io.ReadAll(newOpenAIToAnthropicSSE(strings.NewReader(stream), "gpt"))
	if !strings.Contains(string(out), `"id":"x_y_2"`) {
		t.Errorf("stream tool_use id not sanitized:\n%s", string(out))
	}
}

// TestConvertRequest_ParallelToolCalls: a→o disable_parallel_tool_use:true →
// parallel_tool_calls:false; o→a parallel_tool_calls:false →
// disable_parallel_tool_use:true (never on tool_choice none; synthesized
// {type:auto} when the client gave no tool_choice).
func TestConvertRequest_ParallelToolCalls(t *testing.T) {
	// anthropic → openai.
	in := []byte(`{"model":"c","max_tokens":10,"messages":[{"role":"user","content":"hi"}],"tool_choice":{"type":"auto","disable_parallel_tool_use":true}}`)
	out, err := convertAnthropicRequestToOpenAI(in)
	if err != nil {
		t.Fatal(err)
	}
	o := mustJSON(t, out)
	if o.(map[string]any)["parallel_tool_calls"] != false {
		t.Errorf("a→o parallel_tool_calls = %v want false", o.(map[string]any)["parallel_tool_calls"])
	}

	// openai → anthropic, with an existing tool_choice AND tools.
	in2 := []byte(`{"model":"g","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],"tool_choice":"required","parallel_tool_calls":false}`)
	out2, err := convertOpenAIRequestToAnthropic(in2)
	if err != nil {
		t.Fatal(err)
	}
	tc := objOf(t, mustJSON(t, out2), "tool_choice").(map[string]any)
	if tc["type"] != "any" || tc["disable_parallel_tool_use"] != true {
		t.Errorf("o→a tool_choice = %+v want {type:any, disable_parallel_tool_use:true}", tc)
	}

	// openai → anthropic, NO client tool_choice but WITH tools → synthesized
	// {type:auto} + disable_parallel_tool_use (F1 fix: tools must be present).
	in3 := []byte(`{"model":"g","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],"parallel_tool_calls":false}`)
	out3, err := convertOpenAIRequestToAnthropic(in3)
	if err != nil {
		t.Fatal(err)
	}
	tc3 := objOf(t, mustJSON(t, out3), "tool_choice").(map[string]any)
	if tc3["type"] != "auto" || tc3["disable_parallel_tool_use"] != true {
		t.Errorf("o→a synthesized tool_choice = %+v want {type:auto, disable_parallel_tool_use:true}", tc3)
	}

	// F1 fix: parallel_tool_calls:false WITHOUT tools → no tool_choice synthesized
	// (anthropic rejects a tool_choice with no tools).
	in3b := []byte(`{"model":"g","messages":[{"role":"user","content":"hi"}],"parallel_tool_calls":false}`)
	out3b, err := convertOpenAIRequestToAnthropic(in3b)
	if err != nil {
		t.Fatal(err)
	}
	if _, has := mustJSON(t, out3b).(map[string]any)["tool_choice"]; has {
		t.Errorf("F1: parallel_tool_calls:false without tools should NOT synthesize tool_choice")
	}

	// openai → anthropic, tool_choice none WITH tools must NOT carry the flag
	// (anthropic rejects the combination).
	in4 := []byte(`{"model":"g","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],"tool_choice":"none","parallel_tool_calls":false}`)
	out4, err := convertOpenAIRequestToAnthropic(in4)
	if err != nil {
		t.Fatal(err)
	}
	tc4 := objOf(t, mustJSON(t, out4), "tool_choice").(map[string]any)
	if tc4["type"] != "none" {
		t.Fatalf("o→a tool_choice = %+v want {type:none}", tc4)
	}
	if _, has := tc4["disable_parallel_tool_use"]; has {
		t.Errorf("tool_choice none must not carry disable_parallel_tool_use: %+v", tc4)
	}
}

// TestConvertResponse_CacheTokens_NonStream: o→a splits openai cached_tokens out
// of prompt_tokens (input = prompt − cached, clamped ≥0); a→o folds anthropic
// cache read/creation into prompt_tokens + prompt_tokens_details.
func TestConvertResponse_CacheTokens_NonStream(t *testing.T) {
	oaiResp := []byte(`{"id":"a","model":"g","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":4}}}`)
	ant, err := convertOpenAIResponseToAnthropic(oaiResp)
	if err != nil {
		t.Fatal(err)
	}
	u := objOf(t, mustJSON(t, ant), "usage").(map[string]any)
	if u["input_tokens"] != float64(6) || u["cache_read_input_tokens"] != float64(4) || u["output_tokens"] != float64(5) {
		t.Errorf("o→a usage = %+v want input=6 cache_read=4 output=5", u)
	}

	// Clamp: cached > prompt → input 0 (cache_read still reported verbatim).
	clampResp := []byte(`{"id":"a","model":"g","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"prompt_tokens_details":{"cached_tokens":12}}}`)
	ant2, err := convertOpenAIResponseToAnthropic(clampResp)
	if err != nil {
		t.Fatal(err)
	}
	u2 := objOf(t, mustJSON(t, ant2), "usage").(map[string]any)
	if u2["input_tokens"] != float64(0) || u2["cache_read_input_tokens"] != float64(12) {
		t.Errorf("o→a clamp usage = %+v want input=0 cache_read=12", u2)
	}

	antResp := []byte(`{"id":"msg_x","model":"c","stop_reason":"end_turn","content":[{"type":"text","text":"hi"}],"usage":{"input_tokens":3,"output_tokens":7,"cache_read_input_tokens":2,"cache_creation_input_tokens":5}}`)
	oai, err := convertAnthropicResponseToOpenAI(antResp)
	if err != nil {
		t.Fatal(err)
	}
	u3 := objOf(t, mustJSON(t, oai), "usage").(map[string]any)
	if u3["prompt_tokens"] != float64(10) || u3["completion_tokens"] != float64(7) || u3["total_tokens"] != float64(17) {
		t.Errorf("a→o usage = %+v want prompt=10 completion=7 total=17", u3)
	}
	if objOf(t, u3, "prompt_tokens_details", "cached_tokens") != float64(2) {
		t.Errorf("a→o cached_tokens = %v want 2", objOf(t, u3, "prompt_tokens_details", "cached_tokens"))
	}
}

// TestConvertResponse_CacheTokens_Stream: the streaming transformers apply the
// same cache split/fold in their terminal usage events.
func TestConvertResponse_CacheTokens_Stream(t *testing.T) {
	// o→a: trailing usage chunk with prompt_tokens_details → message_delta usage.
	fwd := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"prompt_tokens_details\":{\"cached_tokens\":3}}}\n\n" +
		"data: [DONE]\n\n"
	out, _ := io.ReadAll(newOpenAIToAnthropicSSE(strings.NewReader(fwd), "gpt"))
	s := string(out)
	if !strings.Contains(s, `"input_tokens":7`) || !strings.Contains(s, `"cache_read_input_tokens":3`) {
		t.Errorf("o→a stream usage missing cache split:\n%s", s)
	}

	// o→a stream clamp: cached > prompt → input 0.
	fwdClamp := "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":1,\"prompt_tokens_details\":{\"cached_tokens\":9}}}\n\n" +
		"data: [DONE]\n\n"
	out2, _ := io.ReadAll(newOpenAIToAnthropicSSE(strings.NewReader(fwdClamp), "gpt"))
	s2 := string(out2)
	if !strings.Contains(s2, `"input_tokens":0`) || !strings.Contains(s2, `"cache_read_input_tokens":9`) {
		t.Errorf("o→a stream clamp wrong:\n%s", s2)
	}

	// a→o: message_start usage cache fields → finish chunk prompt fold + details.
	rev := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5,\"cache_read_input_tokens\":3,\"cache_creation_input_tokens\":2}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":4}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	out3, _ := io.ReadAll(newAnthropicToOpenAISSE(strings.NewReader(rev), "c"))
	s3 := string(out3)
	for _, want := range []string{`"prompt_tokens":10`, `"completion_tokens":4`, `"total_tokens":14`, `"prompt_tokens_details":{"cached_tokens":3}`} {
		if !strings.Contains(s3, want) {
			t.Errorf("a→o stream missing %q:\n%s", want, s3)
		}
	}
}

// Developer is a first-class Chat role and maps to Anthropic's top-level
// system field (Anthropic messages themselves accept no developer role).
func TestConvertRequest_DeveloperFoldsIntoSystem(t *testing.T) {
	in := []byte(`{"model":"g","messages":[{"role":"developer","content":"x"},{"role":"user","content":"hi"}]}`)
	out, err := convertOpenAIRequestToAnthropic(in)
	if err != nil {
		t.Fatal(err)
	}
	got := unmarshalMap(t, out)
	if got["system"] != "x" {
		t.Fatalf("system = %#v, want developer content", got["system"])
	}
	for _, raw := range anySlice(got["messages"]) {
		if asMap(raw)["role"] == "developer" {
			t.Fatalf("developer role leaked into Anthropic messages: %s", out)
		}
	}
}

// Anthropic thinking/signature deltas use Chat's reasoning_content plus a
// replayable reasoning_details envelope.
func TestStreaming_ThinkingDeltaPreserved(t *testing.T) {
	stream := "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"hmm\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig\"}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	raw, err := io.ReadAll(newAnthropicToOpenAISSE(strings.NewReader(stream), "c"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"reasoning_content":"hmm"`, `"type":"anthropic_thinking"`, `"signature":"sig"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("reasoning replay missing %s:\n%s", want, raw)
		}
	}
}
