package protocol

import (
	"strings"
	"testing"

	sonic "github.com/bytedance/sonic"
)

func specfixFrames(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	events, err := parseWireSSE(raw)
	if err != nil {
		t.Fatal(err)
	}
	frames := make([]map[string]any, 0, len(events))
	for _, event := range events {
		var payload map[string]any
		if err := sonic.UnmarshalString(event.data, &payload); err != nil {
			t.Fatalf("synthesized frame %q is not a JSON object: %v", event.data, err)
		}
		frames = append(frames, payload)
	}
	return frames
}

// TestSpecfixResponsesJSONToSSESequenceNumbers: every frame synthesized by the
// responses JSON→SSE bridge carries a sequence_number counting 0..N-1, like
// real upstreams and the dedicated streaming converters (strict SDKs require
// it), and both the response.created and response.completed snapshots carry
// created_at even when the source JSON omits it.
func TestSpecfixResponsesJSONToSSESequenceNumbers(t *testing.T) {
	body := `{"id":"r1","object":"response","status":"completed","model":"m",` +
		`"output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed",` +
		`"content":[{"type":"output_text","text":"hi"}]}],` +
		`"usage":{"input_tokens":1,"output_tokens":1}}`
	stream, err := responseToSSE([]byte(body), "responses")
	if err != nil {
		t.Fatal(err)
	}
	frames := specfixFrames(t, stream)
	if len(frames) == 0 {
		t.Fatal("no frames synthesized")
	}
	for i, frame := range frames {
		seq, ok := frame["sequence_number"].(float64)
		if !ok || int(seq) != i {
			t.Fatalf("frame %d (type %v) sequence_number = %v, want %d", i, frame["type"], frame["sequence_number"], i)
		}
	}
	var createdAt any
	for _, frame := range frames {
		switch frame["type"] {
		case "response.created", "response.completed":
			resp := asMap(frame["response"])
			value, ok := resp["created_at"].(float64)
			if !ok || value <= 0 {
				t.Fatalf("%s snapshot created_at = %v, want a positive Unix-seconds number", frame["type"], resp["created_at"])
			}
			if createdAt == nil {
				createdAt = resp["created_at"]
			} else if createdAt != resp["created_at"] {
				t.Fatalf("response.created created_at %v != response.completed created_at %v", createdAt, resp["created_at"])
			}
		}
	}
	if createdAt == nil {
		t.Fatal("no response.created/response.completed frames found")
	}
}

// TestSpecfixResponsesJSONToSSEItemIDs: delta/part frames carry the item_id of
// their enclosing output item, and reasoning summaries follow the spec frame
// shape: reasoning_summary_part.added (part {type:summary_text, text:""}) →
// reasoning_summary_text.delta → reasoning_summary_text.done →
// reasoning_summary_part.done (part carries the full text).
func TestSpecfixResponsesJSONToSSEItemIDs(t *testing.T) {
	body := `{"id":"r1","object":"response","status":"completed","model":"m","created_at":123,"output":[` +
		`{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"deep thought"}]},` +
		`{"type":"message","id":"msg_1","role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}`
	stream, err := responseToSSE([]byte(body), "responses")
	if err != nil {
		t.Fatal(err)
	}
	frames := specfixFrames(t, stream)
	var types []string
	for _, frame := range frames {
		types = append(types, strOf(frame["type"]))
	}
	want := []string{
		"response.created",
		"response.output_item.added",
		"response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done",
		"response.reasoning_summary_part.done",
		"response.output_item.done",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("frame sequence = %v, want %v", types, want)
	}
	for _, frame := range frames {
		switch strOf(frame["type"]) {
		case "response.output_text.delta", "response.output_text.done",
			"response.content_part.added", "response.content_part.done":
			if id := strOf(frame["item_id"]); id != "msg_1" {
				t.Fatalf("%s item_id = %q, want %q", frame["type"], id, "msg_1")
			}
		case "response.reasoning_summary_part.added", "response.reasoning_summary_part.done",
			"response.reasoning_summary_text.delta", "response.reasoning_summary_text.done":
			if id := strOf(frame["item_id"]); id != "rs_1" {
				t.Fatalf("%s item_id = %q, want %q", frame["type"], id, "rs_1")
			}
			if index := intOf(frame["summary_index"]); index != 0 {
				t.Fatalf("%s summary_index = %d, want 0", frame["type"], index)
			}
		}
		if strOf(frame["type"]) == "response.reasoning_summary_part.added" {
			part := asMap(frame["part"])
			if strOf(part["type"]) != "summary_text" || strOf(part["text"]) != "" {
				t.Fatalf("reasoning_summary_part.added part = %v, want {type:summary_text, text:\"\"}", part)
			}
		}
		if strOf(frame["type"]) == "response.reasoning_summary_part.done" {
			part := asMap(frame["part"])
			if strOf(part["type"]) != "summary_text" || strOf(part["text"]) != "deep thought" {
				t.Fatalf("reasoning_summary_part.done part = %v, want the full summary text", part)
			}
		}
	}
}

// TestSpecfixAggregateResponsesSSEFailedTerminal: a response.completed event
// whose response object carries status failed/cancelled or a non-null error is
// a failure, not a clean terminal — aggregation must error (fail-closed, same
// conditions as the streaming converters), while a clean completed succeeds.
func TestSpecfixAggregateResponsesSSEFailedTerminal(t *testing.T) {
	delta := "event: response.output_text.delta\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n"
	cases := []struct {
		name     string
		terminal string
	}{
		{"failed status", `{"type":"response.completed","response":{"id":"r1","status":"failed","error":{"type":"server_error","message":"boom"}}}`},
		{"cancelled status", `{"type":"response.completed","response":{"id":"r1","status":"cancelled"}}`},
		{"completed with non-null error", `{"type":"response.completed","response":{"id":"r1","status":"completed","error":{"type":"server_error","message":"boom"}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := delta + "event: response.completed\ndata: " + tc.terminal + "\n\n"
			if _, err := aggregateSSEToResponse([]byte(raw), "responses"); err == nil {
				t.Fatalf("terminal %s aggregated as a clean response, want an error", tc.name)
			}
		})
	}
	t.Run("clean completed still succeeds", func(t *testing.T) {
		raw := delta + "event: response.completed\n" +
			`data: {"type":"response.completed","response":{"id":"r1","status":"completed",` +
			`"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}}` + "\n\n"
		aggregated, err := aggregateSSEToResponse([]byte(raw), "responses")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(aggregated), `"text":"hi"`) {
			t.Fatalf("clean completed lost its content: %s", aggregated)
		}
	})
}

// TestSpecfixAggregateMergedFramesRecoveredPerLine: a non-spec gateway that
// omits blank lines between frames folds several JSON payloads into one frame.
// Aggregation recovers each data line as its own event (with the event: label
// current when that line was read) instead of silently dropping them.
func TestSpecfixAggregateMergedFramesRecoveredPerLine(t *testing.T) {
	t.Run("chat", func(t *testing.T) {
		raw := "data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"first\"},\"finish_reason\":null}]}\n" +
			"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"second\"},\"finish_reason\":null}]}\n" +
			"data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
			"data: [DONE]\n\n"
		aggregated, err := aggregateSSEToResponse([]byte(raw), "openai")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(aggregated), `"content":"firstsecond"`) {
			t.Fatalf("merged chat frames not recovered: %s", aggregated)
		}
		if !strings.Contains(string(aggregated), `"finish_reason":"stop"`) {
			t.Fatalf("merged chat frames lost the finish: %s", aggregated)
		}
	})

	t.Run("responses keeps per-line event association", func(t *testing.T) {
		// Payloads deliberately lack "type" so dispatch depends on the event:
		// label that preceded THAT data line, not the frame's final label.
		raw := "event: response.output_text.delta\n" +
			"data: {\"delta\":\"one\"}\n" +
			"event: response.output_text.delta\n" +
			"data: {\"delta\":\"two\"}\n" +
			"event: response.completed\n" +
			"data: {\"response\":{\"id\":\"r1\",\"status\":\"completed\"}}\n\n"
		aggregated, err := aggregateSSEToResponse([]byte(raw), "responses")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(aggregated), "onetwo") {
			t.Fatalf("recovered responses frames lost their per-line events or deltas: %s", aggregated)
		}
	})
}

// TestSpecfixParseWireSSEFoldedDoneTerminates: a literal [DONE] recovered from
// a gateway-merged folded frame is an explicit terminator — later data lines
// of the same folded frame are not processed.
func TestSpecfixParseWireSSEFoldedDoneTerminates(t *testing.T) {
	raw := "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n" +
		"data: [DONE]\n" +
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"leak\"},\"finish_reason\":\"stop\"}]}\n\n"
	events, err := parseWireSSE([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].data != "[DONE]" {
		t.Fatalf("recovered events = %#v, want the first chunk then [DONE] and nothing after", events)
	}
	aggregated, err := aggregateSSEToResponse([]byte(raw), "openai")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(aggregated), "hi") || strings.Contains(string(aggregated), "leak") {
		t.Fatalf("content after a folded [DONE] leaked into the aggregate: %s", aggregated)
	}
}

// TestSpecfixAnthropicJSONToSSEOmitsNullUsage: when the source message JSON
// has no usage, the synthesized message_delta omits the usage key entirely
// instead of emitting "usage":null; present usage passes through.
func TestSpecfixAnthropicJSONToSSEOmitsNullUsage(t *testing.T) {
	body := `{"id":"m1","type":"message","role":"assistant","model":"m",` +
		`"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn"}`
	stream, err := responseToSSE([]byte(body), "anthropic")
	if err != nil {
		t.Fatal(err)
	}
	delta := specfixLastFrameOfType(t, stream, "message_delta")
	if _, present := delta["usage"]; present {
		t.Fatalf("message_delta emitted usage %v for a source without usage, want the key omitted", delta["usage"])
	}

	withUsage := `{"id":"m1","type":"message","role":"assistant","model":"m",` +
		`"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",` +
		`"usage":{"input_tokens":3,"output_tokens":2}}`
	stream, err = responseToSSE([]byte(withUsage), "anthropic")
	if err != nil {
		t.Fatal(err)
	}
	delta = specfixLastFrameOfType(t, stream, "message_delta")
	usage := asMap(delta["usage"])
	if intOf(usage["output_tokens"]) != 2 || intOf(usage["input_tokens"]) != 3 {
		t.Fatalf("message_delta usage = %v, want the source usage passed through", delta["usage"])
	}
}

func specfixLastFrameOfType(t *testing.T, raw []byte, typ string) map[string]any {
	t.Helper()
	var found map[string]any
	for _, frame := range specfixFrames(t, raw) {
		if strOf(frame["type"]) == typ {
			found = frame
		}
	}
	if found == nil {
		t.Fatalf("no %s frame in:\n%s", typ, raw)
	}
	return found
}
