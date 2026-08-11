package protocol

// convert_sse_test.go — SSE event-sequence assertion helpers for the streaming
// converters, plus a self-test. Existing stream tests assert with ReadAll +
// substring matching; these helpers parse the converted stream into frames so
// tests can assert the exact event SEQUENCE (ported from opencodex's
// drainSse/sseItems pattern).

import (
	"io"
	"strings"
	"testing"

	sonic "github.com/bytedance/sonic"
)

// sseEvent is one parsed SSE frame: the `event:` line value ("" for data-only
// frames, OpenAI chat style) and the raw data payload ("[DONE]" for the
// OpenAI terminator).
type sseEvent struct {
	event string
	data  string
}

// drainSSE reads a converted SSE stream to EOF and parses it into frames.
func drainSSE(t *testing.T, r io.Reader) []sseEvent {
	t.Helper()
	raw, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("drainSSE read: %v", err)
	}
	return parseSSE(string(raw))
}

// parseSSE parses raw SSE text into frames. A blank line resets the pending
// event type; lines without an event:/data: prefix are ignored.
func parseSSE(s string) []sseEvent {
	var events []sseEvent
	pending := ""
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			pending = ""
			continue
		}
		if strings.HasPrefix(line, "event:") {
			pending = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			continue
		}
		if strings.HasPrefix(line, "data:") {
			events = append(events, sseEvent{event: pending, data: strings.TrimSpace(strings.TrimPrefix(line, "data:"))})
			pending = ""
		}
	}
	return events
}

// sseEventType maps a frame to a comparable type: the `event:` line value when
// present, "[DONE]" for the terminator, otherwise the payload's type/object
// field (chat.completion.chunk frames are data-only).
func sseEventType(ev sseEvent) string {
	if ev.event != "" {
		return ev.event
	}
	if ev.data == "[DONE]" {
		return "[DONE]"
	}
	var m map[string]any
	if err := sonic.UnmarshalString(ev.data, &m); err == nil {
		if ty, ok := m["type"].(string); ok && ty != "" {
			return ty
		}
		if ob, ok := m["object"].(string); ok && ob != "" {
			return ob
		}
	}
	return ev.data
}

// sseEventTypes maps frames to their type sequence.
func sseEventTypes(events []sseEvent) []string {
	types := make([]string, 0, len(events))
	for _, ev := range events {
		types = append(types, sseEventType(ev))
	}
	return types
}

// sseCount counts frames of the given type.
func sseCount(events []sseEvent, typ string) int {
	n := 0
	for _, ev := range events {
		if sseEventType(ev) == typ {
			n++
		}
	}
	return n
}

// sseFilter returns the frames of the given type, in order.
func sseFilter(events []sseEvent, typ string) []sseEvent {
	var out []sseEvent
	for _, ev := range events {
		if sseEventType(ev) == typ {
			out = append(out, ev)
		}
	}
	return out
}

// sseDataMap parses a frame's data payload (fatal on error).
func sseDataMap(t *testing.T, ev sseEvent) map[string]any {
	t.Helper()
	return unmarshalMap(t, []byte(ev.data))
}

// assertEventSequence fails unless the frame type sequence equals want.
func assertEventSequence(t *testing.T, events []sseEvent, want []string) {
	t.Helper()
	got := sseEventTypes(events)
	if len(got) != len(want) {
		t.Fatalf("event sequence length = %d, want %d\n got: %v\nwant: %v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event %d = %q, want %q\n got: %v\nwant: %v", i, got[i], want[i], got, want)
		}
	}
}

// assertResponsesItemPairing checks the synthesized Responses item lifecycle:
// every response.output_item.added is matched by exactly one
// response.output_item.done carrying the same item id AND the same item type,
// in the same order.
func assertResponsesItemPairing(t *testing.T, events []sseEvent) {
	t.Helper()
	type idType struct{ id, typ string }
	var added, done []idType
	for _, ev := range events {
		if ev.event != "response.output_item.added" && ev.event != "response.output_item.done" {
			continue
		}
		item := asMap(sseDataMap(t, ev)["item"])
		e := idType{id: strOf(item["id"]), typ: strOf(item["type"])}
		if ev.event == "response.output_item.added" {
			added = append(added, e)
		} else {
			done = append(done, e)
		}
	}
	if len(added) != len(done) {
		t.Fatalf("output_item added/done not paired: added=%v done=%v", added, done)
	}
	for i := range added {
		if added[i] != done[i] {
			t.Errorf("item %d: added=%+v done=%+v (id/type mismatch)", i, added[i], done[i])
		}
	}
}

// TestConvertSSEDrain is the helper self-test: frames with and without event:
// lines, the [DONE] marker, and junk lines parse as expected.
func TestConvertSSEDrain(t *testing.T) {
	raw := "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"r1"}}` + "\n\n" +
		"data: {\"object\":\"chat.completion.chunk\"}\n\n" +
		": a comment line to ignore\n\n" +
		"data: [DONE]\n\n"
	events := drainSSE(t, strings.NewReader(raw))
	want := []string{"response.created", "chat.completion.chunk", "[DONE]"}
	assertEventSequence(t, events, want)
	if got := sseCount(events, "[DONE]"); got != 1 {
		t.Errorf("[DONE] count = %d, want 1", got)
	}
	if id := strOf(asMap(sseDataMap(t, events[0])["response"])["id"]); id != "r1" {
		t.Errorf("first frame response.id = %q, want r1", id)
	}
	// A final frame without a trailing newline still parses.
	events2 := drainSSE(t, strings.NewReader("data: [DONE]"))
	if len(events2) != 1 || sseEventType(events2[0]) != "[DONE]" {
		t.Errorf("unterminated final frame not parsed: %+v", events2)
	}
}
