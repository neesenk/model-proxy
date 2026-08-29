package app

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"model-proxy/internal/protocol"
)

func unmarshalMap(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("unmarshal %q: %v", body, err)
	}
	return value
}

func asMap(value any) map[string]any {
	result, _ := value.(map[string]any)
	return result
}

func strOpt(value any) string {
	result, _ := value.(string)
	return result
}

func strOf(value any) string {
	if result, ok := value.(string); ok {
		return result
	}
	body, _ := json.Marshal(value)
	return strings.Trim(string(body), `"`)
}

func intOf(value any) int {
	switch number := value.(type) {
	case float64:
		return int(number)
	case int:
		return number
	default:
		return 0
	}
}

func mustJSONStr(t *testing.T, value string) string {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

type sseEvent struct {
	event string
	data  string
}

func drainSSE(t *testing.T, reader io.Reader) []sseEvent {
	t.Helper()
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("drainSSE read: %v", err)
	}
	return parseSSE(string(raw))
}

func parseSSE(raw string) []sseEvent {
	var events []sseEvent
	pending := ""
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case line == "":
			pending = ""
		case strings.HasPrefix(line, "event:"):
			pending = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			events = append(events, sseEvent{
				event: pending,
				data:  strings.TrimSpace(strings.TrimPrefix(line, "data:")),
			})
			pending = ""
		}
	}
	return events
}

func sseEventType(event sseEvent) string {
	if event.event != "" {
		return event.event
	}
	if event.data == "[DONE]" {
		return "[DONE]"
	}
	var body map[string]any
	if json.Unmarshal([]byte(event.data), &body) == nil {
		if typ := strOpt(body["type"]); typ != "" {
			return typ
		}
		if object := strOpt(body["object"]); object != "" {
			return object
		}
	}
	return event.data
}

func sseEventTypes(events []sseEvent) []string {
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, sseEventType(event))
	}
	return types
}

func sseCount(events []sseEvent, typ string) int {
	count := 0
	for _, event := range events {
		if sseEventType(event) == typ {
			count++
		}
	}
	return count
}

func sseFilter(events []sseEvent, typ string) []sseEvent {
	var filtered []sseEvent
	for _, event := range events {
		if sseEventType(event) == typ {
			filtered = append(filtered, event)
		}
	}
	return filtered
}

func assertNoSSEError(t *testing.T, events []sseEvent) {
	t.Helper()
	for _, event := range events {
		if event.event == "error" {
			t.Fatalf("unexpected SSE error event: %s", event.data)
		}
		var body map[string]any
		if json.Unmarshal([]byte(event.data), &body) == nil && body["error"] != nil {
			t.Fatalf("unexpected SSE error payload: %s", event.data)
		}
	}
}

func sseDataMap(t *testing.T, event sseEvent) map[string]any {
	t.Helper()
	return unmarshalMap(t, []byte(event.data))
}

func extractCandidateText(body []byte, backendProto string) string {
	wire, ok := protocol.Parse(backendProto)
	if !ok {
		return ""
	}
	return protocol.ExtractResponseText(body, wire)
}

const responsesTextSSE = "event: response.created\n" +
	`data: {"type":"response.created","response":{"id":"resp_1","status":"in_progress","model":"gpt-x"}}` + "\n\n" +
	"event: response.output_item.added\n" +
	`data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","id":"msg_0","role":"assistant","content":[]}}` + "\n\n" +
	"event: response.output_text.delta\n" +
	`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"hel"}` + "\n\n" +
	"event: response.output_text.delta\n" +
	`data: {"type":"response.output_text.delta","output_index":0,"content_index":0,"delta":"lo"}` + "\n\n" +
	"event: response.output_item.done\n" +
	`data: {"type":"response.output_item.done","output_index":0,"item":{"type":"message","id":"msg_0","status":"completed"}}` + "\n\n" +
	"event: response.completed\n" +
	`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","model":"gpt-x","usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}` + "\n\n"
