package protocol

import (
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
	raw := readAllChecked(t, newOpenAIToAnthropicSSE(strings.NewReader(in), "m"))
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
	raw := readAllChecked(t, newAnthropicToOpenAISSE(strings.NewReader(in), "m"))
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
	raw := readAllChecked(t, newResponsesToAnthropicSSE(strings.NewReader(in), "m"))
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
	raw := readAllChecked(t, newOpenAIToAnthropicSSE(strings.NewReader(in), "m"))
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

// TestSSE_EmptyDataLinesFoldPerSpec: an empty data: line is a real payload
// line, not "no frame yet". Per the HTML spec each data line contributes its
// value plus "\n" and the final "\n" is stripped at dispatch, so
// `data:` + `data: x` folds to "\nx" (the old pend=="" sentinel dropped the
// leading empty line) and `data: x` + `data:` folds to "x\n".
func TestSSE_EmptyDataLinesFoldPerSpec(t *testing.T) {
	pend, open := "", false
	pend = appendSSEData(pend, open, "")
	open = true
	pend = appendSSEData(pend, open, "x")
	if pend != "\nx" {
		t.Errorf("data: + data: x folded to %q, want %q", pend, "\nx")
	}

	pend, open = "", false
	pend = appendSSEData(pend, open, "x")
	open = true
	pend = appendSSEData(pend, open, "")
	if pend != "x\n" {
		t.Errorf("data: x + data: folded to %q, want %q", pend, "x\n")
	}
}

// TestSSE_LeadingEmptyDataLineOpensFrame: through the real frame assembly
// (parseWireSSE), a frame whose first data: line is empty keeps that line in
// the folded payload, a lone empty data line still dispatches a frame, and a
// trailing frame without its final blank line dispatches too.
func TestSSE_LeadingEmptyDataLineOpensFrame(t *testing.T) {
	events, err := parseWireSSE([]byte("data:\ndata: x\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].data != "\nx" {
		t.Fatalf("leading-empty-line frame = %#v, want one frame with data %q", events, "\nx")
	}

	events, err = parseWireSSE([]byte("data:\n\ndata: y\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].data != "" || events[1].data != "y" {
		t.Fatalf("lone empty data line = %#v, want an empty-payload frame then %q", events, "y")
	}

	events, err = parseWireSSE([]byte("data:\ndata: z"))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].data != "\nz" {
		t.Fatalf("trailing frame without blank line = %#v, want one frame with data %q", events, "\nz")
	}
}

// TestSSE_LeadingEmptyDataLineStillConverts: end-to-end through a transformer
// — a frame folded with a leading empty data line ("\n{...}" is still valid
// JSON) converts as one frame, exactly like the same payload on one line.
func TestSSE_LeadingEmptyDataLineStillConverts(t *testing.T) {
	in := "data:\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lead\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	raw := readAllChecked(t, newOpenAIToAnthropicSSE(strings.NewReader(in), "m"))
	out := string(raw)
	if strings.Count(out, "lead") != 1 || !strings.Contains(out, `"text_delta"`) {
		t.Errorf("frame with a leading empty data line lost or duplicated its delta:\n%s", out)
	}
	if !strings.Contains(out, `"stop_reason":"end_turn"`) {
		t.Errorf("subsequent frame lost after the empty-line frame:\n%s", out)
	}
}

// TestSSE_MergedFramesMissingBlankLineFallBackPerLine: a non-spec gateway that
// omits the blank line between frames folds DISTINCT frames into one payload.
// The merged payload does not parse, but the reader must not silently drop the
// frames — it retries each folded line individually (with a warn).
func TestSSE_MergedFramesMissingBlankLineFallBackPerLine(t *testing.T) {
	in := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"first\"},\"finish_reason\":null}]}\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"second\"},\"finish_reason\":null}]}\n" +
		"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	raw := readAllChecked(t, newOpenAIToAnthropicSSE(strings.NewReader(in), "m"))
	out := string(raw)
	if !strings.Contains(out, "first") || !strings.Contains(out, "second") {
		t.Errorf("merged frames were dropped instead of parsed per-line:\n%s", out)
	}
	if !strings.Contains(out, `"stop_reason":"end_turn"`) {
		t.Errorf("finish lost:\n%s", out)
	}
}

// TestSSE_ResponsesMergedFramesFallBackPerLine: same gateway defect against the
// responses reader — each folded JSON line dispatches its own event.
func TestSSE_ResponsesMergedFramesFallBackPerLine(t *testing.T) {
	in := "event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"m\",\"output_index\":0,\"delta\":\"one\"}\n" +
		"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"m\",\"output_index\":0,\"delta\":\"two\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
	raw := readAllChecked(t, newResponsesToAnthropicSSE(strings.NewReader(in), "m"))
	out := string(raw)
	if !strings.Contains(out, "one") || !strings.Contains(out, "two") {
		t.Errorf("merged responses frames were dropped:\n%s", out)
	}
	if !strings.Contains(out, "message_stop") {
		t.Errorf("terminal lost:\n%s", out)
	}
}

// TestSSE_MergedFramesKeepPerDataEventAssociation: when a non-spec gateway
// omits every inter-frame blank line, each recovered data payload must retain
// the event label that preceded THAT data line. Reusing the final event label
// drops message/tool history and can terminate the stream before earlier
// frames are processed.
func TestSSE_MergedFramesKeepPerDataEventAssociation(t *testing.T) {
	in := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-real\",\"model\":\"m-real\",\"usage\":{\"input_tokens\":7}}}\n" +
		"event: content_block_start\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n" +
		"event: content_block_delta\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n" +
		"event: content_block_stop\n" +
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n" +
		"event: message_stop\n" +
		"data: {\"type\":\"message_stop\"}\n\n"

	raw := readAllChecked(t, newAnthropicToResponsesSSE(strings.NewReader(in), "fallback-model"))
	events, err := parseWireSSE(raw)
	if err != nil {
		t.Fatal(err)
	}
	completed := 0
	for _, event := range events {
		if event.event == "response.completed" {
			completed++
		}
	}
	if got := completed; got != 1 {
		t.Fatalf("response.completed frames = %d, want 1:\n%s", got, raw)
	}
	out := string(raw)
	for _, want := range []string{"msg-real", "m-real", "hello", `"input_tokens":7`, `"output_tokens":2`} {
		if !strings.Contains(out, want) {
			t.Errorf("recovered event sequence lost %q:\n%s", want, out)
		}
	}
}

// TestSSE_ChatTerminalStopsRecoveredContentButKeepsUsage pins Chat's
// finish_reason as a semantic terminal inside a missing-blank folded payload.
// Later content is invalid and must not leak, while a trailing usage-only
// chunk remains observable for terminal accounting before [DONE].
func TestSSE_ChatTerminalStopsRecoveredContentButKeepsUsage(t *testing.T) {
	in := "data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"before\"},\"finish_reason\":null}]}\n" +
		"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n" +
		"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"after\"},\"finish_reason\":null}]}\n" +
		"data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\n" +
		"data: [DONE]\n\n"

	t.Run("anthropic target", func(t *testing.T) {
		raw := readAllChecked(t, newOpenAIToAnthropicSSE(strings.NewReader(in), "m"))
		out := string(raw)
		if !strings.Contains(out, "before") || strings.Contains(out, "after") {
			t.Fatalf("terminal content boundary violated:\n%s", out)
		}
		if !strings.Contains(out, `"input_tokens":3`) || !strings.Contains(out, `"output_tokens":2`) {
			t.Fatalf("trailing usage-only chunk was lost:\n%s", out)
		}
	})

	t.Run("responses target", func(t *testing.T) {
		raw := readAllChecked(t, newOpenAIToResponsesSSE(strings.NewReader(in), "m"))
		out := string(raw)
		if !strings.Contains(out, "before") || strings.Contains(out, "after") {
			t.Fatalf("terminal content boundary violated:\n%s", out)
		}
		if !strings.Contains(out, `"input_tokens":3`) || !strings.Contains(out, `"output_tokens":2`) {
			t.Fatalf("trailing usage-only chunk was lost:\n%s", out)
		}
	})
}
