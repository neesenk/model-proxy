package protocol

import (
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/bytedance/sonic"
)

// Test cases ported from Switchyard's translation suite (scenarios our own
// regression matrix lacked). Each guards a known bug class: duplicated tool
// arguments, stop-reason clobbering, buffered/streamed drift, hostile tool
// ids, data-URI variants, and tool-call adjacency.

// TestParity_DoneFrameArgumentsNotDuplicated: an output_item.done frame whose
// item repeats the FULL arguments after the deltas must not re-emit them —
// the concatenated argument stream stays exactly the original string.
func TestParity_DoneFrameArgumentsNotDuplicated(t *testing.T) {
	const args = `{"q":"x"}`
	in := `data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}` + "\n\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"search","arguments":""}}` + "\n\n" +
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"{\"q\":"}` + "\n\n" +
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":0,"delta":"\"x\"}"}` + "\n\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"search","arguments":"{\"q\":\"x\"}"}}` + "\n\n" +
		`data: {"type":"response.completed","response":{"id":"r1","status":"completed","usage":{"input_tokens":1,"output_tokens":2}}}` + "\n\n"

	// r→anthropic: concatenated input_json_delta payloads equal the original.
	rawA, _ := io.ReadAll(newResponsesToAnthropicSSE(strings.NewReader(in), "m"))
	var anthropicArgs strings.Builder
	for _, ev := range drainSSE(t, strings.NewReader(string(rawA))) {
		if ev.event != "content_block_delta" {
			continue
		}
		if d := asMap(sseDataMap(t, ev)["delta"]); d != nil && d["type"] == "input_json_delta" {
			anthropicArgs.WriteString(strOf(d["partial_json"]))
		}
	}
	if got := anthropicArgs.String(); got != args {
		t.Errorf("r→a concatenated args = %q, want exactly %q (done frame duplicated?)", got, args)
	}

	// r→chat: concatenated tool_calls argument fragments equal the original.
	rawC, _ := io.ReadAll(newResponsesToOpenAISSE(strings.NewReader(in), "m"))
	var chatArgs strings.Builder
	for _, line := range strings.Split(string(rawC), "\n") {
		if !strings.HasPrefix(line, "data: {") {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Function struct {
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if sonic.UnmarshalString(strings.TrimPrefix(line, "data: "), &chunk) == nil {
			for _, c := range chunk.Choices {
				for _, tc := range c.Delta.ToolCalls {
					chatArgs.WriteString(tc.Function.Arguments)
				}
			}
		}
	}
	if got := chatArgs.String(); got != args {
		t.Errorf("r→chat concatenated args = %q, want exactly %q (done frame duplicated?)", got, args)
	}
}

// TestParity_BareMessageStopKeepsMaxTokens: message_delta carrying
// stop_reason max_tokens followed by a reasonless message_stop must finish
// with "length" exactly once — the bare stop must not clobber or duplicate.
func TestParity_BareMessageStopKeepsMaxTokens(t *testing.T) {
	in := `event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_1","model":"claude-x","usage":{"input_tokens":3}}}` + "\n\n" +
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}` + "\n\n" +
		`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":4}}` + "\n\n" +
		`event: message_stop` + "\n" + `data: {"type":"message_stop"}` + "\n\n"
	raw, _ := io.ReadAll(newAnthropicToOpenAISSE(strings.NewReader(in), "m"))
	var lengthStops, otherStops int
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.Contains(line, `"finish_reason":"length"`) {
			continue
		}
		lengthStops++
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, `"finish_reason":"stop"`) {
			otherStops++
		}
	}
	if lengthStops != 1 {
		t.Errorf("finish_reason length appears %d times, want exactly 1:\n%s", lengthStops, raw)
	}
	if otherStops != 0 {
		t.Errorf("bare message_stop clobbered max_tokens into a clean stop:\n%s", raw)
	}
}

// TestParity_HostileToolIDConsistency: an OpenAI tool_call id outside
// Anthropic's charset must sanitize to ONE id used by both the tool_use block
// and the paired tool_result — clients echo ids back, so a mismatch breaks
// the pair at the upstream.
func TestParity_HostileToolIDConsistency(t *testing.T) {
	in := []byte(`{"model":"gpt-x","max_tokens":16,"messages":[
		{"role":"assistant","content":null,"tool_calls":[{"id":"call.bad:id 1","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call.bad:id 1","content":"result"}]}`)
	out, err := convertOpenAIRequestToAnthropic(in)
	if err != nil {
		t.Fatal(err)
	}
	root := unmarshalMap(t, out)
	var useID, resultID string
	for _, m := range asSliceAny(root["messages"]) {
		for _, b := range asSliceAny(asMap(m)["content"]) {
			bm := asMap(b)
			switch bm["type"] {
			case "tool_use":
				useID = strOf(bm["id"])
			case "tool_result":
				resultID = strOf(bm["tool_use_id"])
			}
		}
	}
	if useID == "" || resultID == "" {
		t.Fatalf("tool_use/tool_result pair not found: %s", out)
	}
	if useID != resultID {
		t.Errorf("sanitized ids diverged: tool_use=%q tool_result=%q", useID, resultID)
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9_-]+$`).MatchString(useID) {
		t.Errorf("sanitized id %q violates Anthropic charset", useID)
	}
}

// TestParity_DataURIImageVariants: a base64 data URI with extra parameters
// (charset) still decodes with the BARE MIME as media_type; a percent-encoded
// (non-base64) data URI is dropped observably (no Anthropic source form).
func TestParity_DataURIImageVariants(t *testing.T) {
	in := []byte(`{"model":"gpt-x","max_tokens":16,"messages":[{"role":"user","content":[
		{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}},
		{"type":"image_url","image_url":{"url":"data:image/png;charset=binary;base64,BBBB"}},
		{"type":"image_url","image_url":{"url":"data:text/html,%3Cb%3Efile%3C/b%3E"}},
		{"type":"text","text":"look"}]}]}`)
	var out []byte
	logs := captureConvertLog(t, func() {
		var err error
		out, err = convertOpenAIRequestToAnthropic(in)
		if err != nil {
			t.Fatal(err)
		}
	})
	root := unmarshalMap(t, out)
	var sources []map[string]any
	for _, b := range asSliceAny(asSliceAny(asMap(asSliceAny(root["messages"])[0])["content"])) {
		if bm := asMap(b); bm != nil && bm["type"] == "image" {
			sources = append(sources, asMap(bm["source"]))
		}
	}
	if len(sources) != 2 {
		t.Fatalf("image blocks = %d, want 2 (base64 + charset-param base64):\n%s", len(sources), out)
	}
	if mt := strOf(sources[0]["media_type"]); mt != "image/png" {
		t.Errorf("plain data URI media_type = %q, want image/png", mt)
	}
	if mt := strOf(sources[1]["media_type"]); mt != "image/png" {
		t.Errorf("charset-param data URI media_type = %q, want bare image/png", mt)
	}
	if !strings.Contains(logs, "non-base64 data URI") {
		t.Errorf("percent-encoded data URI dropped without warning, log = %q", logs)
	}
}

// TestParity_InterleavedAssistantKeepsToolAdjacency: an assistant message
// between a function_call and its output merges into the assistant
// tool-call message instead of splitting tool_calls from its tool result
// (strict upstreams reject non-adjacent tool messages).
func TestParity_InterleavedAssistantKeepsToolAdjacency(t *testing.T) {
	in := []byte(`{"model":"gpt-x","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"q"}]},
		{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"note"}]},
		{"type":"function_call_output","call_id":"c1","output":"res"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}]}`)
	out, err := convertResponsesRequestToOpenAI(in)
	if err != nil {
		t.Fatal(err)
	}
	root := unmarshalMap(t, out)
	msgs := asSliceAny(root["messages"])
	if len(msgs) != 4 {
		t.Fatalf("messages = %d, want 4 (user, merged assistant, tool, user):\n%s", len(msgs), out)
	}
	merged := asMap(msgs[1])
	if merged["role"] != "assistant" {
		t.Fatalf("msgs[1] role = %v, want assistant", merged["role"])
	}
	if merged["content"] != "note" || !mapHasKey(merged, "tool_calls") {
		t.Errorf("merged assistant = content %v tool_calls %v, want content note + tool_calls", merged["content"], merged["tool_calls"])
	}
	tool := asMap(msgs[2])
	if tool["role"] != "tool" || tool["tool_call_id"] != "c1" {
		t.Errorf("tool message = %v, want role tool with tool_call_id c1 (adjacent to its call)", tool)
	}
}

// TestParity_BufferedAndStreamedEquivalence: the same logical Responses
// answer must yield the same Anthropic semantics through the buffered
// converter and the streaming converter — the two implementations drift
// silently without a lock-step test.
func TestParity_BufferedAndStreamedEquivalence(t *testing.T) {
	answer := `{"id":"resp_1","model":"gpt-x","status":"completed","output":[
		{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"pondering"}],"encrypted_content":"sig123"},
		{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"hello"}]},
		{"type":"function_call","id":"fc_1","call_id":"call_1","name":"search","arguments":"{\"q\":\"x\"}"}],
		"usage":{"input_tokens":5,"output_tokens":7}}`

	buffered, err := convertResponsesToAnthropic([]byte(answer))
	if err != nil {
		t.Fatal(err)
	}
	broot := unmarshalMap(t, buffered)
	type block struct {
		kind      string
		text      string
		signature string
		id        string
		name      string
		input     string
	}
	var bblocks []block
	for _, b := range asSliceAny(broot["content"]) {
		bm := asMap(b)
		blk := block{kind: strOf(bm["type"])}
		switch blk.kind {
		case "text":
			blk.text = strOf(bm["text"])
		case "thinking":
			blk.text = strOf(bm["thinking"])
			blk.signature = strOf(bm["signature"])
		case "tool_use":
			blk.id = strOf(bm["id"])
			blk.name = strOf(bm["name"])
			input, _ := sonic.Marshal(bm["input"])
			blk.input = string(input)
		}
		bblocks = append(bblocks, blk)
	}
	bStop := strOf(broot["stop_reason"])
	bUsage := asMap(broot["usage"])

	// Streamed form of the same answer.
	stream := `data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n" +
		`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}` + "\n\n" +
		`data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","output_index":0,"delta":"pondering"}` + "\n\n" +
		`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_1","encrypted_content":"sig123"}}` + "\n\n" +
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg_1"}}` + "\n\n" +
		`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":1,"delta":"hello"}` + "\n\n" +
		`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"message","id":"msg_1"}}` + "\n\n" +
		`data: {"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"search"}}` + "\n\n" +
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":2,"delta":"{\"q\":\"x\"}"}` + "\n\n" +
		`data: {"type":"response.output_item.done","output_index":2,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"search","arguments":"{\"q\":\"x\"}"}}` + "\n\n" +
		`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":5,"output_tokens":7}}}` + "\n\n"
	rawA, _ := io.ReadAll(newResponsesToAnthropicSSE(strings.NewReader(stream), "m"))

	var sblocks []block
	cur := block{}
	stopReason, inputTok, outputTok := "", 0, 0
	for _, ev := range drainSSE(t, strings.NewReader(string(rawA))) {
		d := sseDataMap(t, ev)
		switch ev.event {
		case "content_block_start":
			cb := asMap(d["content_block"])
			cur = block{kind: strOf(cb["type"])}
			if cur.kind == "tool_use" {
				cur.id = strOf(cb["id"])
				cur.name = strOf(cb["name"])
			}
		case "content_block_delta":
			delta := asMap(d["delta"])
			switch strOf(delta["type"]) {
			case "text_delta":
				cur.text += strOf(delta["text"])
			case "thinking_delta":
				cur.text += strOf(delta["thinking"])
			case "signature_delta":
				cur.signature += strOf(delta["signature"])
			case "input_json_delta":
				cur.input += strOf(delta["partial_json"])
			}
		case "content_block_stop":
			if cur.kind != "" {
				sblocks = append(sblocks, cur)
				cur = block{}
			}
		case "message_start":
			if u := asMap(asMap(d["message"])["usage"]); u != nil {
				inputTok += intOf(u["input_tokens"])
			}
		case "message_delta":
			stopReason = strOf(asMap(d["delta"])["stop_reason"])
			if u := asMap(d["usage"]); u != nil {
				inputTok += intOf(u["input_tokens"])
				outputTok += intOf(u["output_tokens"])
			}
		}
	}

	if len(sblocks) != len(bblocks) {
		t.Fatalf("block count streamed=%d buffered=%d\nstreamed=%+v\nbuffered=%+v", len(sblocks), len(bblocks), sblocks, bblocks)
	}
	for i := range bblocks {
		if sblocks[i] != bblocks[i] {
			t.Errorf("block %d drifted: streamed=%+v buffered=%+v", i, sblocks[i], bblocks[i])
		}
	}
	if stopReason != bStop {
		t.Errorf("stop_reason streamed=%q buffered=%q", stopReason, bStop)
	}
	if inputTok != intOf(bUsage["input_tokens"]) || outputTok != intOf(bUsage["output_tokens"]) {
		t.Errorf("usage streamed=(%d,%d) buffered=(%d,%d)", inputTok, outputTok,
			intOf(bUsage["input_tokens"]), intOf(bUsage["output_tokens"]))
	}
}

// asSliceAny and mapHasKey are thin test-side helpers keeping the ported
// cases readable.
func asSliceAny(v any) []any {
	s, _ := v.([]any)
	return s
}

func mapHasKey(m map[string]any, key string) bool {
	_, ok := m[key]
	return ok
}
