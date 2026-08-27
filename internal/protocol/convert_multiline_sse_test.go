package protocol

import (
	"io"
	"strings"
	"testing"
)

// Multi-line SSE data frames (spec: consecutive data: lines join with "\n" at
// the dispatch blank line). LLM vendors emit single-line frames today, but the
// readers now fold them per spec instead of mis-splitting each line into its
// own frame. One test per source-protocol family.

// TestSSE_MultiLineDataChatSource: a chat chunk split over two data: lines
// still produces ONE anthropic text_delta with the full text.
func TestSSE_MultiLineDataChatSource(t *testing.T) {
	in := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\n" +
		"data: \"delta\":{\"content\":\"hello world\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	raw, _ := io.ReadAll(newOpenAIToAnthropicSSE(strings.NewReader(in), "m"))
	out := string(raw)
	if !strings.Contains(out, `"text_delta"`) || !strings.Contains(out, "hello world") {
		t.Errorf("folded chat frame lost its delta:\n%s", out)
	}
	if strings.Count(out, "hello world") != 1 {
		t.Errorf("folded frame dispatched more than once:\n%s", out)
	}
	if !strings.Contains(out, `"stop_reason":"end_turn"`) {
		t.Errorf("finish lost:\n%s", out)
	}
}

// TestSSE_MultiLineDataAnthropicSource: an anthropic event split over two
// data: lines still converts to one chat content chunk.
func TestSSE_MultiLineDataAnthropicSource(t *testing.T) {
	in := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\n" +
		"data: \"usage\":{\"input_tokens\":3}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\n" +
		"data: \"delta\":{\"type\":\"text_delta\",\"text\":\"hi there\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	raw, _ := io.ReadAll(newAnthropicToOpenAISSE(strings.NewReader(in), "m"))
	out := string(raw)
	if !strings.Contains(out, `"content":"hi there"`) {
		t.Errorf("folded anthropic frame lost its delta:\n%s", out)
	}
	if strings.Count(out, "hi there") != 1 {
		t.Errorf("folded frame dispatched more than once:\n%s", out)
	}
}

// TestSSE_MultiLineDataResponsesSource: a responses event split over two
// data: lines still converts (event type from the event: line survives).
func TestSSE_MultiLineDataResponsesSource(t *testing.T) {
	in := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":\n" +
		"data: {\"id\":\"r1\",\"status\":\"in_progress\"}}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"m\",\"output_index\":0,\n" +
		"data: \"delta\":\"folded text\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
	raw, _ := io.ReadAll(newResponsesToAnthropicSSE(strings.NewReader(in), "m"))
	out := string(raw)
	if !strings.Contains(out, `"text_delta"`) || !strings.Contains(out, "folded text") {
		t.Errorf("folded responses frame lost its delta:\n%s", out)
	}
	if strings.Count(out, "folded text") != 1 {
		t.Errorf("folded frame dispatched more than once:\n%s", out)
	}
	if !strings.Contains(out, "message_stop") {
		t.Errorf("terminal lost:\n%s", out)
	}
}

// TestSSE_NonDataLinesDoNotSplitFrames: per spec only a blank line dispatches
// a frame. A comment or event: line interleaved with (or trailing) the data
// lines must not dispatch the frame early — an early dispatch would cut the
// JSON payload mid-object, lose the delta, and leak the event forward.
func TestSSE_NonDataLinesDoNotSplitFrames(t *testing.T) {
	in := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\n" +
		": heartbeat comment mid-frame\n" +
		"data: \"delta\":{\"content\":\"kept whole\"},\"finish_reason\":null}]}\n" +
		"event: trailing-label-after-data\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	raw, _ := io.ReadAll(newOpenAIToAnthropicSSE(strings.NewReader(in), "m"))
	out := string(raw)
	if !strings.Contains(out, "kept whole") {
		t.Errorf("frame split by comment/event lines — delta lost:\n%s", out)
	}
	if strings.Count(out, "kept whole") != 1 {
		t.Errorf("frame dispatched more than once:\n%s", out)
	}
	if !strings.Contains(out, `"stop_reason":"end_turn"`) {
		t.Errorf("subsequent frame lost after non-data lines:\n%s", out)
	}
}
