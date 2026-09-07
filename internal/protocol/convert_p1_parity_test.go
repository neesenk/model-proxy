package protocol

import (
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/bytedance/sonic"
)

// Second wave of Switchyard-ported scenarios: usage arithmetic pinning,
// dialect terminators, legacy/unknown roles, json_schema edges, empty-output
// synthesis, id-collision disambiguation, SSE parser robustness, completed-
// snapshot completeness, and a kitchen-sink no-leakage pass.

// TestParity_TotalTokensRecomputed: the a→chat finish chunk derives
// total_tokens = prompt + completion even though anthropic never carries it.
func TestParity_TotalTokensRecomputed(t *testing.T) {
	in := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"usage\":{\"input_tokens\":5}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":3}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	raw := readAllChecked(t, newAnthropicToOpenAISSE(strings.NewReader(in), "m"))
	if !strings.Contains(string(raw), `"total_tokens":8`) {
		t.Errorf("total_tokens not recomputed as input+output:\n%s", raw)
	}
}

// TestParity_AnthropicSourceDoneTerminator: a gateway speaking anthropic
// events but terminating with [DONE] (OpenRouter-style dialect) must yield a
// clean finish, not a fail-closed error stream.
func TestParity_AnthropicSourceDoneTerminator(t *testing.T) {
	in := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"usage\":{\"input_tokens\":5}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"!\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n" +
		"data: [DONE]\n\n" +
		"data: {\"garbage\":\"after done\"}\n\n"
	raw := readAllChecked(t, newAnthropicToOpenAISSE(strings.NewReader(in), "m"))
	out := string(raw)
	if !strings.Contains(out, `"finish_reason":"stop"`) {
		t.Errorf("[DONE]-terminated anthropic stream produced no clean finish:\n%s", out)
	}
	if !strings.Contains(out, `"content":"hi"`) {
		t.Errorf("text deltas lost:\n%s", out)
	}
	if strings.Contains(out, "garbage") {
		t.Errorf("frames after [DONE] leaked into the client stream:\n%s", out)
	}
}

// TestParity_SSEParserRobustness: one-byte-at-a-time chunk feeding (split
// UTF-8, split framing) and CRLF delimiters reassemble identically to the
// LF-delimited feed.
func TestParity_SSEParserRobustness(t *testing.T) {
	in := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"usage\":{\"input_tokens\":1}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"héllo ☃\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	want := readAllChecked(t, newAnthropicToOpenAISSE(strings.NewReader(in), "m"))

	oneByte := &oneByteReader{r: strings.NewReader(in)}
	got := readAllChecked(t, newAnthropicToOpenAISSE(oneByte, "m"))
	assertSemanticallyEqualSSE(t, "1-byte chunked feed", string(want), string(got))

	crlf := strings.ReplaceAll(in, "\n", "\r\n")
	gotCRLF := readAllChecked(t, newAnthropicToOpenAISSE(strings.NewReader(crlf), "m"))
	assertSemanticallyEqualSSE(t, "CRLF feed", string(want), string(gotCRLF))
}

// assertSemanticallyEqualSSE compares two SSE payloads frame by frame as
// parsed JSON — map key order in the re-marshaled frames is not a contract.
func assertSemanticallyEqualSSE(t *testing.T, tag, want, got string) {
	t.Helper()
	wantFrames := dataPayloads(want)
	gotFrames := dataPayloads(got)
	if len(wantFrames) != len(gotFrames) {
		t.Fatalf("%s: frame count %d ≠ %d\nwant: %s\ngot:  %s", tag, len(wantFrames), len(gotFrames), want, got)
	}
	for i := range wantFrames {
		// The [DONE] terminator is a literal, not JSON.
		if wantFrames[i] == "[DONE]" || gotFrames[i] == "[DONE]" {
			if wantFrames[i] != gotFrames[i] {
				t.Errorf("%s: frame %d terminator mismatch: %q vs %q", tag, i, wantFrames[i], gotFrames[i])
			}
			continue
		}
		var wv, gv any
		if sonic.Unmarshal([]byte(wantFrames[i]), &wv) != nil || sonic.Unmarshal([]byte(gotFrames[i]), &gv) != nil {
			t.Fatalf("%s: frame %d not JSON\nwant: %s\ngot:  %s", tag, i, wantFrames[i], gotFrames[i])
		}
		if fmt.Sprintf("%v", wv) != fmt.Sprintf("%v", gv) {
			t.Errorf("%s: frame %d differs\nwant: %v\ngot:  %v", tag, i, wv, gv)
		}
	}
}

func dataPayloads(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(line, "data: ") {
			out = append(out, strings.TrimSpace(strings.TrimPrefix(line, "data: ")))
		}
	}
	return out
}

type oneByteReader struct{ r io.Reader }

func (c *oneByteReader) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return c.r.Read(p)
}

// TestParity_LegacyAndUnknownRolesWarn: role:"function" (legacy OpenAI) and
// unknown roles are dropped with a convertWarn — never silently swallowed.
func TestParity_LegacyAndUnknownRolesWarn(t *testing.T) {
	for name, body := range map[string]string{
		"legacy function": `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"},{"role":"function","name":"f","content":"res"}]}`,
		"unknown role":    `{"model":"m","max_tokens":16,"messages":[{"role":"api","content":"x"},{"role":"user","content":"hi"}]}`,
	} {
		var out []byte
		logs := captureConvertLog(t, func() {
			var err error
			out, err = convertOpenAIRequestToAnthropic([]byte(body), nil)
			if err != nil {
				t.Fatal(err)
			}
		})
		if !strings.Contains(logs, "unknown role") {
			t.Errorf("%s: no drop warning, log = %q", name, logs)
		}
		if strings.Contains(string(out), `"res"`) || strings.Contains(string(out), `"x"`) {
			t.Errorf("%s: dropped message content leaked: %s", name, out)
		}
		if !strings.Contains(string(out), `"text":"hi"`) {
			t.Errorf("%s: user message lost: %s", name, out)
		}
	}
}

// TestParity_EmptyJsonSchemaWrapperDropped: a json_schema wrapper without a
// schema is dropped with a warning (a bare {"type":"json_schema"} is invalid
// upstream); the nested type never overrides the discriminator.
func TestParity_EmptyJsonSchemaWrapperDropped(t *testing.T) {
	var out []byte
	logs := captureConvertLog(t, func() {
		var err error
		out, err = convertOpenAIRequestToResponses([]byte(
			`{"model":"m","messages":[{"role":"user","content":"x"}],"response_format":{"type":"json_schema","json_schema":{}}}`), nil)
		if err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(logs, "empty json_schema") {
		t.Errorf("missing drop warning, log = %q", logs)
	}
	if strings.Contains(string(out), "json_schema") {
		t.Errorf("empty wrapper not dropped: %s", out)
	}

	// Nested type override: still a json_schema request.
	out, err := convertOpenAIRequestToResponses([]byte(
		`{"model":"m","messages":[{"role":"user","content":"x"}],"response_format":{"type":"json_schema","json_schema":{"type":"text","name":"out","schema":{"type":"object"}}}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"type":"json_schema"`) || strings.Contains(string(out), `"format":{"type":"text"`) {
		t.Errorf("nested type overrode the json_schema discriminator: %s", out)
	}
}

// TestParity_EmptyChatResponseSynthesizesTextBlock: a chat answer with no
// content, refusal, or tool calls still yields ONE (empty) text block — an
// empty content array is not a valid anthropic message.
func TestParity_EmptyChatResponseSynthesizesTextBlock(t *testing.T) {
	out, err := convertOpenAIResponseToAnthropic([]byte(
		`{"id":"c1","model":"gpt","choices":[{"message":{"role":"assistant","content":null},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	root := unmarshalMap(t, out)
	content := asSliceAny(root["content"])
	if len(content) != 1 {
		t.Fatalf("content blocks = %d, want 1 synthesized text block: %s", len(content), out)
	}
	if blk := asMap(content[0]); blk["type"] != "text" {
		t.Errorf("synthesized block = %v, want text", blk)
	}
}

// TestParity_ToolIDCollisionDisambiguated: two different hostile ids that
// sanitize to the same string must not collide in the converted request; the
// same id echoed twice must stay stable.
func TestParity_ToolIDCollisionDisambiguated(t *testing.T) {
	in := []byte(`{"model":"m","max_tokens":16,"messages":[
		{"role":"assistant","content":null,"tool_calls":[
			{"id":"call.a","type":"function","function":{"name":"f","arguments":"{}"}},
			{"id":"call_a","type":"function","function":{"name":"g","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call.a","content":"r1"},
		{"role":"tool","tool_call_id":"call_a","content":"r2"}]}`)
	out, err := convertOpenAIRequestToAnthropic(in, nil)
	if err != nil {
		t.Fatal(err)
	}
	var useIDs []string
	var resultIDs []string
	for _, m := range asSliceAny(unmarshalMap(t, out)["messages"]) {
		for _, b := range asSliceAny(asMap(m)["content"]) {
			bm := asMap(b)
			switch bm["type"] {
			case "tool_use":
				useIDs = append(useIDs, strOf(bm["id"]))
			case "tool_result":
				resultIDs = append(resultIDs, strOf(bm["tool_use_id"]))
			}
		}
	}
	if len(useIDs) != 2 || useIDs[0] == useIDs[1] {
		t.Fatalf("colliding tool_use ids not disambiguated: %v", useIDs)
	}
	// Pairing survives the suffix: each result matches its own call.
	for i, id := range useIDs {
		if resultIDs[i] != id {
			t.Errorf("pair %d broken: tool_use=%q tool_result=%q", i, id, resultIDs[i])
		}
	}
}

// TestParity_CompletedSnapshotCompleteness: a synthesized Responses stream's
// response.completed carries the fields strict client SDKs require.
func TestParity_CompletedSnapshotCompleteness(t *testing.T) {
	in := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"claude-x\",\"usage\":{\"input_tokens\":4}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	raw := readAllChecked(t, newAnthropicToResponsesSSE(strings.NewReader(in), "m"))
	var completed map[string]any
	for _, ev := range drainSSE(t, strings.NewReader(string(raw))) {
		if ev.event == "response.completed" {
			completed = sseDataMap(t, ev)
		}
	}
	if completed == nil {
		t.Fatalf("no response.completed frame:\n%s", raw)
	}
	resp := asMap(completed["response"])
	if resp == nil {
		t.Fatalf("completed frame has no response object:\n%s", raw)
	}
	for _, field := range []string{"id", "object", "created_at", "status", "model", "output", "usage"} {
		if _, ok := resp[field]; !ok {
			t.Errorf("response.completed missing required field %q:\n%v", field, resp)
		}
	}
	// created_at is a Unix-seconds NUMBER on the Responses wire (int64 in
	// strongly-typed SDKs); an RFC3339 string would fail the whole frame.
	if ts, ok := resp["created_at"].(float64); !ok || ts <= 0 || ts != float64(int64(ts)) {
		t.Errorf("created_at must be a Unix-seconds integer, got %T (%v)", resp["created_at"], resp["created_at"])
	}
	if usage := asMap(resp["usage"]); usage == nil {
		t.Errorf("completed usage missing: %v", resp["usage"])
	}
}

// TestParity_KitchenSinkNoLeakage: one fixture carrying vendor/unknown shapes
// converts through every direction that accepts its source protocol without
// the unknown type names leaking into known block/part positions.
func TestParity_KitchenSinkNoLeakage(t *testing.T) {
	chatIn := []byte(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":[
		{"type":"text","text":"hi"},
		{"type":"future_openai_part","payload":{"vendor":true}}]}]}`)
	var out []byte
	_ = captureConvertLog(t, func() {
		var err error
		out, err = convertOpenAIRequestToAnthropic(chatIn, nil)
		if err != nil {
			t.Fatal(err)
		}
	})
	if strings.Contains(string(out), "future_openai_part") {
		t.Errorf("unknown part type leaked into anthropic request: %s", out)
	}
	if !strings.Contains(string(out), `"text":"hi"`) {
		t.Errorf("known part lost: %s", out)
	}
	// No-leak + known-parts-survive is the observable contract here; per-shape
	// warn coverage lives in the fault suite (J).

	anthropicIn := []byte(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":[
		{"type":"text","text":"hi"},
		{"type":"future_vendor_block","vendor":"x"}]}]}`)
	outA, err := convertAnthropicRequestToOpenAI(anthropicIn)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(outA), "future_vendor_block") {
		t.Errorf("unknown block type leaked into chat request: %s", outA)
	}
	if !strings.Contains(string(outA), `"content":"hi"`) && !strings.Contains(string(outA), `"text":"hi"`) {
		t.Errorf("known block lost: %s", outA)
	}

	responsesIn := []byte(`{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"audio_clip","data":"x"}]}`)
	outR, err := convertResponsesRequestToOpenAI(responsesIn)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(outR), "audio_clip") {
		t.Errorf("unknown item type leaked into chat request: %s", outR)
	}
	if !strings.Contains(string(outR), `"content":"hi"`) && !strings.Contains(string(outR), `"text":"hi"`) {
		t.Errorf("known item lost: %s", outR)
	}
}

// TestParity_AnthropicDoneTerminatorIgnoresLaterFrames: [DONE] is an explicit
// terminator — a RECOGNIZED event frame after it must not be processed. The
// finish chunk is emitted exactly once, by the [DONE] itself.
func TestParity_AnthropicDoneTerminatorIgnoresLaterFrames(t *testing.T) {
	in := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"usage\":{\"input_tokens\":5}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"data: [DONE]\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"after\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":9}}\n\n"
	raw := readAllChecked(t, newAnthropicToOpenAISSE(strings.NewReader(in), "m"))
	out := string(raw)
	if strings.Contains(out, "after") {
		t.Errorf("content delta after [DONE] leaked into the client stream:\n%s", out)
	}
	// Exactly role + content + finish + usage chunks and one [DONE]: the
	// post-[DONE] message_delta must not emit a second finish chunk (nor its
	// usage).
	if n := strings.Count(out, "chat.completion.chunk"); n != 4 {
		t.Errorf("chunk count = %d, want 4 (role, content, finish, usage):\n%s", n, out)
	}
	if n := strings.Count(out, `"finish_reason":"stop"`); n != 1 {
		t.Errorf("finish chunk count = %d, want 1:\n%s", n, out)
	}
	if strings.Contains(out, `"output_tokens":9`) || strings.Contains(out, `"completion_tokens":9`) {
		t.Errorf("usage from a post-[DONE] message_delta leaked:\n%s", out)
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Errorf("client stream missing the [DONE] marker:\n%s", out)
	}
}

// holdOpenReader yields data once, then blocks until release — mimics a
// gateway that sends [DONE] and then holds the connection open.
type holdOpenReader struct {
	data    []byte
	off     int
	release <-chan struct{}
}

func (h *holdOpenReader) Read(p []byte) (int, error) {
	if h.off < len(h.data) {
		n := copy(p, h.data[h.off:])
		h.off += n
		return n, nil
	}
	<-h.release
	return 0, io.EOF
}

// TestParity_AnthropicDoneTerminatorNoEOFWait: the finish chunk + data: [DONE]
// must reach the client as soon as the upstream [DONE] arrives, without
// waiting for the upstream to close the connection.
func TestParity_AnthropicDoneTerminatorNoEOFWait(t *testing.T) {
	release := make(chan struct{})
	defer close(release) // unblocks the reader goroutine if the assertion times out
	in := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"usage\":{\"input_tokens\":5}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"data: [DONE]\n\n"
	tr := newAnthropicToOpenAISSE(&holdOpenReader{data: []byte(in), release: release}, "m")
	done := make(chan string, 1)
	go func() {
		var out strings.Builder
		buf := make([]byte, 4096)
		for !strings.Contains(out.String(), "data: [DONE]") {
			n, err := tr.Read(buf)
			out.Write(buf[:n])
			if err != nil {
				break
			}
		}
		done <- out.String()
	}()
	select {
	case out := <-done:
		if !strings.Contains(out, `"content":"hi"`) || !strings.Contains(out, `"finish_reason":"stop"`) {
			t.Errorf("stream before upstream EOF = %q, want content + finish chunk", out)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("transformer blocked waiting for upstream EOF after [DONE]")
	}
}

// TestParity_AnthropicDoneTerminatorNoTerminalEvent: a [DONE]-terminated
// stream without message_delta is a CLEAN dialect termination — the finish
// chunk is synthesized from what arrived, and no "terminated before a
// terminal event" error chunk follows the already-delivered content.
func TestParity_AnthropicDoneTerminatorNoTerminalEvent(t *testing.T) {
	in := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"usage\":{\"input_tokens\":5}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"data: [DONE]\n\n"
	raw := readAllChecked(t, newAnthropicToOpenAISSE(strings.NewReader(in), "m"))
	out := string(raw)
	if !strings.Contains(out, `"content":"hi"`) || !strings.Contains(out, `"finish_reason":"stop"`) || !strings.Contains(out, "data: [DONE]") {
		t.Errorf("clean [DONE] termination incomplete (content/finish/[DONE]):\n%s", out)
	}
	if strings.Contains(out, "terminated before a terminal event") || strings.Contains(out, `"error"`) {
		t.Errorf("spurious error chunk after a [DONE]-terminated stream:\n%s", out)
	}
}
