package protocol

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
)

// --- benchmark fixtures ---

// A realistic 3-turn tool conversation (anthropic format, ~4KB with tools).
var benchAnthropicReq = []byte(`{
  "model":"claude-sonnet-4-20250514",
  "max_tokens":4096,
  "system":"You are a helpful assistant. Always use tools when available. Be concise but thorough in your explanations. Follow the user's instructions carefully and ask for clarification when needed.",
  "tools":[
    {"name":"bash","description":"Execute a bash command","input_schema":{"type":"object","properties":{"command":{"type":"string","description":"The bash command to run"},"timeout":{"type":"integer","default":30}},"required":["command"]}},
    {"name":"edit","description":"Edit a file","input_schema":{"type":"object","properties":{"path":{"type":"string"},"old":{"type":"string"},"new":{"type":"string"}},"required":["path","old","new"]}},
    {"name":"read","description":"Read a file","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}
  ],
  "tool_choice":{"type":"auto","disable_parallel_tool_use":true},
  "messages":[
    {"role":"user","content":"Fix the failing test in proxy_test.go. The test expects status 200 but gets 502 because the upstream is down. Add a retry."},
    {"role":"assistant","content":[{"type":"text","text":"I'll start by reading the test file."},{"type":"tool_use","id":"toolu_01abc","name":"read","input":{"path":"proxy_test.go"}}]},
    {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01abc","content":"func TestProxy(t *testing.T) {\n  resp := doRequest(t, \"/v1/messages\", body)\n  if resp.StatusCode != 200 {\n    t.Fatalf(\"status=%d\", resp.StatusCode)\n  }\n}"}]},
    {"role":"assistant","content":[{"type":"text","text":"The test doesn't handle the upstream being down. I'll add a mock server."},{"type":"tool_use","id":"toolu_02def","name":"edit","input":{"path":"proxy_test.go","old":"func TestProxy","new":"func TestProxyWithMock"}}]},
    {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_02def","content":"edited"}]},
    {"role":"user","content":"Now run the test to make sure it passes."}
  ]
}`)

// Same conversation in openai format.
var benchOpenAIReq = mustParseJSON(`{
  "model":"gpt-4o",
  "max_tokens":4096,
  "tools":[
    {"type":"function","function":{"name":"bash","description":"Execute a bash command","parameters":{"type":"object","properties":{"command":{"type":"string"},"timeout":{"type":"integer","default":30}},"required":["command"]}}},
    {"type":"function","function":{"name":"edit","description":"Edit a file","parameters":{"type":"object","properties":{"path":{"type":"string"},"old":{"type":"string"},"new":{"type":"string"}},"required":["path","old","new"]}}},
    {"type":"function","function":{"name":"read","description":"Read a file","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}}
  ],
  "parallel_tool_calls":false,
  "messages":[
    {"role":"system","content":"You are a helpful assistant. Always use tools when available. Be concise but thorough."},
    {"role":"user","content":"Fix the failing test in proxy_test.go."},
    {"role":"assistant","content":null,"tool_calls":[{"id":"call_01abc","type":"function","function":{"name":"read","arguments":"{\"path\":\"proxy_test.go\"}"}}]},
    {"role":"tool","tool_call_id":"call_01abc","content":"func TestProxy..."},
    {"role":"assistant","content":"I'll add a mock server.","tool_calls":[{"id":"call_02def","type":"function","function":{"name":"edit","arguments":"{\"path\":\"proxy_test.go\"}"}}]},
    {"role":"tool","tool_call_id":"call_02def","content":"edited"},
    {"role":"user","content":"Now run the test."}
  ]
}`)

func mustParseJSON(s string) []byte {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		panic("bench fixture JSON: " + err.Error())
	}
	out, _ := json.Marshal(v)
	return out
}

// A large non-streaming openai response (~2KB with tool_calls + usage).
var benchOpenAIResp = []byte(`{"id":"chatcmpl-abc","model":"gpt-4o","choices":[{"message":{"role":"assistant","content":"I'll read the file first, then make the edit.","tool_calls":[{"id":"call_xyz1","type":"function","function":{"name":"bash","arguments":"{\"command\":\"go test ./...\"}"}},{"id":"call_xyz2","type":"function","function":{"name":"read","arguments":"{\"path\":\"main.go\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":3200,"completion_tokens":180,"prompt_tokens_details":{"cached_tokens":1500}}}`)

// A 20-chunk openai SSE stream simulating text + tool_calls + finish + usage.
var benchOpenAISSE = func() string {
	var b strings.Builder
	for i := 0; i < 15; i++ {
		b.WriteString("data: {\"id\":\"chatcmpl-x\",\"choices\":[{\"delta\":{\"content\":\"This is chunk number " + string(rune('0'+i)) + " of the streaming response. \"}}]}\n\n")
	}
	b.WriteString("data: {\"id\":\"chatcmpl-x\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_t1\",\"function\":{\"name\":\"bash\",\"arguments\":\"{\\\"command\\\":\\\"ls\"}}]}}]}\n\n")
	b.WriteString("data: {\"id\":\"chatcmpl-x\",\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\" -la\"}}]}}]}\n\n")
	b.WriteString("data: {\"id\":\"chatcmpl-x\",\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":3000,\"completion_tokens\":50,\"prompt_tokens_details\":{\"cached_tokens\":1000}}}\n\n")
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}()

// --- benchmarks ---

func BenchmarkConvertRequest_AnthropicToOpenAI(b *testing.B) {
	b.SetBytes(int64(len(benchAnthropicReq)))
	for i := 0; i < b.N; i++ {
		if _, err := convertAnthropicRequestToOpenAI(benchAnthropicReq); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkConvertRequest_OpenAIToAnthropic(b *testing.B) {
	b.SetBytes(int64(len(benchOpenAIReq)))
	for i := 0; i < b.N; i++ {
		if _, err := convertOpenAIRequestToAnthropic(benchOpenAIReq, nil); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkConvertResponse_OpenAIToAnthropic(b *testing.B) {
	b.SetBytes(int64(len(benchOpenAIResp)))
	for i := 0; i < b.N; i++ {
		if _, err := convertOpenAIResponseToAnthropic(benchOpenAIResp); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkConvertSSE_OpenAIToAnthropic(b *testing.B) {
	r := strings.NewReader(benchOpenAISSE)
	b.SetBytes(int64(len(benchOpenAISSE)))
	for i := 0; i < b.N; i++ {
		conv := newOpenAIToAnthropicSSE(strings.NewReader(benchOpenAISSE), "gpt-4o")
		io.Copy(io.Discard, conv)
		_ = r
	}
}
