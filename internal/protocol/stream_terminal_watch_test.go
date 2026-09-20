package protocol

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// chunkSource serves fixed chunks then a final error (io.EOF by default), so
// tests can split frames across Read boundaries at will.
type chunkSource struct {
	chunks [][]byte
	end    error
	closed bool
}

func (c *chunkSource) Read(p []byte) (int, error) {
	if len(c.chunks) == 0 {
		if c.end == nil {
			return 0, io.EOF
		}
		return 0, c.end
	}
	n := copy(p, c.chunks[0])
	c.chunks = c.chunks[1:]
	return n, nil
}

func (c *chunkSource) Close() error {
	c.closed = true
	return nil
}

// drainAll reads the watcher to its end, tolerating the non-EOF end error the
// source injected (the synthesized terminal must still have been served).
func drainAll(t *testing.T, w *TerminalWatcher) ([]byte, error) {
	t.Helper()
	var out bytes.Buffer
	buf := make([]byte, 4096)
	for {
		n, err := w.Read(buf)
		if n > 0 {
			out.Write(buf[:n])
		}
		if err != nil {
			return out.Bytes(), err
		}
	}
}

const watchAnthropicPartial = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_1","role":"assistant","content":[],"model":"glm-5.3"}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"par"}}` + "\n\n"

const watchAnthropicComplete = watchAnthropicPartial +
	"event: content_block_stop\n" +
	`data: {"type":"content_block_stop","index":0}` + "\n\n" +
	"event: message_delta\n" +
	`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":3}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

const watchChatPartial = `data: {"id":"c1","model":"gpt-x","choices":[{"index":0,"delta":{"role":"assistant","content":"he"}}]}` + "\n\n" +
	`data: {"id":"c1","model":"gpt-x","choices":[{"index":0,"delta":{"content":"llo"}}]}` + "\n\n"

const watchChatComplete = watchChatPartial +
	`data: {"id":"c1","model":"gpt-x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
	"data: [DONE]\n\n"

const watchResponsesPartial = "event: response.created\n" +
	`data: {"type":"response.created","response":{"id":"resp_1","object":"response","status":"in_progress"}}` + "\n\n" +
	"event: response.output_text.delta\n" +
	`data: {"type":"response.output_text.delta","delta":"par"}` + "\n\n"

const watchResponsesComplete = watchResponsesPartial +
	"event: response.completed\n" +
	`data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed"}}` + "\n\n"

func TestTerminalWatcher_AnthropicTruncatedSynthesizesErrorEvent(t *testing.T) {
	src := &chunkSource{chunks: [][]byte{[]byte(watchAnthropicPartial)}}
	w := WatchStreamTerminal(src, Anthropic)
	out, err := drainAll(t, w)
	if err != io.EOF {
		t.Fatalf("end error = %v, want io.EOF", err)
	}
	if !bytes.HasPrefix(out, []byte(watchAnthropicPartial)) {
		t.Fatalf("forwarded bytes were mutated:\n%s", out)
	}
	want := "event: error\n" +
		`data: {"type":"error","error":{"type":"api_error","message":"upstream stream ended before message_stop"}}` + "\n\n"
	if !bytes.HasSuffix(out, []byte(want)) {
		t.Fatalf("stream does not end with the synthesized error event:\ngot  tail: %q\nwant tail: %q", out[len(out)-len(want)-40:], want)
	}
	if bytes.Contains(out, []byte("event: message_stop")) ||
		bytes.Contains(out, []byte(`"type":"message_stop"`)) {
		t.Fatalf("a fake success terminal leaked into the synthesized stream")
	}
	if !w.Synthesized() {
		t.Fatal("Synthesized() = false, want true")
	}
	if src.closed {
		t.Fatal("Close must not have been called by reading")
	}
}

func TestTerminalWatcher_AnthropicCompleteIsByteIdentical(t *testing.T) {
	src := &chunkSource{chunks: [][]byte{[]byte(watchAnthropicComplete)}}
	w := WatchStreamTerminal(src, Anthropic)
	out, err := drainAll(t, w)
	if err != io.EOF {
		t.Fatalf("end error = %v, want io.EOF", err)
	}
	if !bytes.Equal(out, []byte(watchAnthropicComplete)) {
		t.Fatalf("complete stream was modified:\n%s", out)
	}
	if w.Synthesized() {
		t.Fatal("Synthesized() = true on a complete stream")
	}
}

// A bare message_stop without stop_reason (kimi upstream behavior) is not
// cache-complete (StreamTerminalComplete) but IS client-acceptable: the SDK
// contract is satisfied, so nothing may be appended after the client's own
// terminal.
func TestTerminalWatcher_AnthropicBareMessageStopUntouched(t *testing.T) {
	bare := watchAnthropicPartial +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	src := &chunkSource{chunks: [][]byte{[]byte(bare)}}
	w := WatchStreamTerminal(src, Anthropic)
	out, err := drainAll(t, w)
	if err != io.EOF {
		t.Fatalf("end error = %v, want io.EOF", err)
	}
	if !bytes.Equal(out, []byte(bare)) {
		t.Fatalf("bare-message_stop stream was modified:\n%s", out)
	}
	if w.Synthesized() {
		t.Fatal("Synthesized() = true on a bare message_stop stream")
	}
}

// An upstream error event is already a protocol-native terminal: never
// synthesize a second one behind it.
func TestTerminalWatcher_AnthropicUpstreamErrorEventUntouched(t *testing.T) {
	failed := strings.Replace(watchAnthropicComplete,
		"event: message_stop", "event: error", 1)
	failed = strings.Replace(failed,
		`data: {"type":"message_stop"}`, `data: {"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`, 1)
	src := &chunkSource{chunks: [][]byte{[]byte(failed)}}
	w := WatchStreamTerminal(src, Anthropic)
	out, err := drainAll(t, w)
	if err != io.EOF {
		t.Fatalf("end error = %v, want io.EOF", err)
	}
	if !bytes.Equal(out, []byte(failed)) {
		t.Fatalf("upstream-error stream was modified:\n%s", out)
	}
	if w.Synthesized() {
		t.Fatal("Synthesized() = true behind an upstream error event")
	}
}

func TestTerminalWatcher_OpenAITruncatedSynthesizesErrorChunk(t *testing.T) {
	src := &chunkSource{chunks: [][]byte{[]byte(watchChatPartial)}}
	w := WatchStreamTerminal(src, OpenAI)
	out, err := drainAll(t, w)
	if err != io.EOF {
		t.Fatalf("end error = %v, want io.EOF", err)
	}
	if !bytes.HasPrefix(out, []byte(watchChatPartial)) {
		t.Fatalf("forwarded bytes were mutated:\n%s", out)
	}
	want := `data: {"error":{"code":null,"message":"upstream stream ended without a terminal finish_reason","param":null,"type":"api_error"}}` + "\n\n"
	if !bytes.HasSuffix(out, []byte(want)) {
		t.Fatalf("stream does not end with the synthesized error chunk:\ngot  tail: %q\nwant tail: %q", out[len(out)-len(want)-40:], want)
	}
	if bytes.Contains(out, []byte("[DONE]")) {
		t.Fatal("a fake [DONE] delimiter leaked into the synthesized stream")
	}
	if !w.Synthesized() {
		t.Fatal("Synthesized() = false, want true")
	}
}

// finish_reason without a trailing [DONE] is client-acceptable for the openai
// dialect (several upstreams omit the delimiter): untouched, unsynthesized.
func TestTerminalWatcher_OpenAIFinishReasonWithoutDONEUntouched(t *testing.T) {
	stream := watchChatPartial +
		`data: {"id":"c1","model":"gpt-x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	src := &chunkSource{chunks: [][]byte{[]byte(stream)}}
	w := WatchStreamTerminal(src, OpenAI)
	out, err := drainAll(t, w)
	if err != io.EOF {
		t.Fatalf("end error = %v, want io.EOF", err)
	}
	if !bytes.Equal(out, []byte(stream)) {
		t.Fatalf("finish_reason-only stream was modified:\n%s", out)
	}
	if w.Synthesized() {
		t.Fatal("Synthesized() = true on a finish_reason-only stream")
	}
}

func TestTerminalWatcher_OpenAIDoneDelimiterOnlyUntouched(t *testing.T) {
	stream := watchChatPartial + "data: [DONE]\n\n"
	src := &chunkSource{chunks: [][]byte{[]byte(stream)}}
	w := WatchStreamTerminal(src, OpenAI)
	out, err := drainAll(t, w)
	if err != io.EOF {
		t.Fatalf("end error = %v, want io.EOF", err)
	}
	if !bytes.Equal(out, []byte(stream)) {
		t.Fatalf("[DONE]-terminated stream was modified:\n%s", out)
	}
	if w.Synthesized() {
		t.Fatal("Synthesized() = true on a [DONE]-terminated stream")
	}
}

func TestTerminalWatcher_ResponsesTruncatedSynthesizesFailed(t *testing.T) {
	src := &chunkSource{chunks: [][]byte{[]byte(watchResponsesPartial)}}
	w := WatchStreamTerminal(src, Responses)
	out, err := drainAll(t, w)
	if err != io.EOF {
		t.Fatalf("end error = %v, want io.EOF", err)
	}
	if !bytes.HasPrefix(out, []byte(watchResponsesPartial)) {
		t.Fatalf("forwarded bytes were mutated:\n%s", out)
	}
	want := "event: response.failed\n" +
		`data: {"type":"response.failed","response":{"id":"resp_1","object":"response","status":"failed","error":{"code":"api_error","message":"upstream stream ended before a terminal response event"}}}` + "\n\n"
	if !bytes.HasSuffix(out, []byte(want)) {
		t.Fatalf("stream does not end with the synthesized response.failed:\ngot  tail: %q\nwant tail: %q", out[len(out)-len(want)-40:], want)
	}
	if !w.Synthesized() {
		t.Fatal("Synthesized() = false, want true")
	}
}

func TestTerminalWatcher_ResponsesCompletedUntouched(t *testing.T) {
	src := &chunkSource{chunks: [][]byte{[]byte(watchResponsesComplete)}}
	w := WatchStreamTerminal(src, Responses)
	out, err := drainAll(t, w)
	if err != io.EOF {
		t.Fatalf("end error = %v, want io.EOF", err)
	}
	if !bytes.Equal(out, []byte(watchResponsesComplete)) {
		t.Fatalf("completed stream was modified:\n%s", out)
	}
	if w.Synthesized() {
		t.Fatal("Synthesized() = true on a completed stream")
	}
}

// response.failed is itself a terminal: an upstream that reported its own
// failure must not get a second synthesized one.
func TestTerminalWatcher_ResponsesUpstreamFailedUntouched(t *testing.T) {
	failed := strings.Replace(watchResponsesComplete,
		"event: response.completed", "event: response.failed", 1)
	failed = strings.Replace(failed,
		`"status":"completed"`, `"status":"failed"`, 1)
	src := &chunkSource{chunks: [][]byte{[]byte(failed)}}
	w := WatchStreamTerminal(src, Responses)
	out, err := drainAll(t, w)
	if err != io.EOF {
		t.Fatalf("end error = %v, want io.EOF", err)
	}
	if !bytes.Equal(out, []byte(failed)) {
		t.Fatalf("upstream-failed stream was modified:\n%s", out)
	}
	if w.Synthesized() {
		t.Fatal("Synthesized() = true behind an upstream response.failed")
	}
}

// A read error mid-stream is still a truncation from the client's point of
// view: the synthesized terminal must be served BEFORE the error surfaces.
func TestTerminalWatcher_ReadErrorServesSynthesisBeforeError(t *testing.T) {
	sentinel := errors.New("upstream connection reset")
	src := &chunkSource{chunks: [][]byte{[]byte(watchAnthropicPartial)}, end: sentinel}
	w := WatchStreamTerminal(src, Anthropic)
	out, err := drainAll(t, w)
	if !errors.Is(err, sentinel) {
		t.Fatalf("end error = %v, want the upstream sentinel", err)
	}
	if !bytes.HasSuffix(out, []byte("event: error\n")) &&
		!strings.Contains(string(out), watchAnthropicTruncated) {
		t.Fatalf("synthesized terminal not served before the error:\n%s", out)
	}
	if !strings.Contains(string(out), watchAnthropicTruncated) {
		t.Fatalf("synthesized message missing from the served bytes:\n%s", out)
	}
}

// The stream-began gate: an empty body must stay empty so the executor's
// zero-byte 200 failure path (model lock) keeps working.
func TestTerminalWatcher_EmptyBodyStaysEmpty(t *testing.T) {
	src := &chunkSource{}
	w := WatchStreamTerminal(src, Anthropic)
	out, err := drainAll(t, w)
	if err != io.EOF {
		t.Fatalf("end error = %v, want io.EOF", err)
	}
	if len(out) != 0 {
		t.Fatalf("empty stream synthesized %d bytes", len(out))
	}
	if w.Synthesized() {
		t.Fatal("Synthesized() = true on an empty stream")
	}
}

// Read chunk boundaries are arbitrary: frames split mid-JSON, mid-line and in
// single bytes must classify exactly like whole-buffer reads.
func TestTerminalWatcher_ChunkBoundariesDoNotAffectClassification(t *testing.T) {
	split := func(s string) [][]byte {
		var chunks [][]byte
		for i := 0; i < len(s); i++ {
			chunks = append(chunks, []byte(s[i:i+1]))
		}
		return chunks
	}
	for _, tc := range []struct {
		name   string
		proto  Protocol
		stream string
		synth  bool
	}{
		{"anthropic truncated", Anthropic, watchAnthropicPartial, true},
		{"anthropic complete", Anthropic, watchAnthropicComplete, false},
		{"openai truncated", OpenAI, watchChatPartial, true},
		{"openai complete", OpenAI, watchChatComplete, false},
		{"responses truncated", Responses, watchResponsesPartial, true},
		{"responses complete", Responses, watchResponsesComplete, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &chunkSource{chunks: split(tc.stream)}
			w := WatchStreamTerminal(src, tc.proto)
			out, err := drainAll(t, w)
			if err != io.EOF {
				t.Fatalf("end error = %v, want io.EOF", err)
			}
			if !bytes.HasPrefix(out, []byte(tc.stream)) {
				t.Fatalf("forwarded bytes were mutated")
			}
			if got := w.Synthesized(); got != tc.synth {
				t.Fatalf("Synthesized() = %v, want %v", got, tc.synth)
			}
			if tc.synth && len(out) <= len(tc.stream) {
				t.Fatal("synthesis expected but no bytes were appended")
			}
			if !tc.synth && !bytes.Equal(out, []byte(tc.stream)) {
				t.Fatal("complete stream was modified")
			}
		})
	}
}

// Folded multi-data-line frames follow the SSE spec (joined with \n) and the
// shared pump's semantics: one frame, one classification.
func TestTerminalWatcher_FoldedDataLinesClassifyAsOneFrame(t *testing.T) {
	stream := "data: {\"type\":\"message_start\",\n" +
		`data: "message":{"id":"m"}}` + "\n\n" +
		"event: message_stop\n" +
		"data: {\"type\":\n" +
		`data: "message_stop"}` + "\n\n"
	src := &chunkSource{chunks: [][]byte{[]byte(stream)}}
	w := WatchStreamTerminal(src, Anthropic)
	out, err := drainAll(t, w)
	if err != io.EOF {
		t.Fatalf("end error = %v, want io.EOF", err)
	}
	if !bytes.Equal(out, []byte(stream)) {
		t.Fatalf("folded-frame stream was modified:\n%s", out)
	}
	if w.Synthesized() {
		t.Fatal("folded message_stop frame was not recognized as a terminal")
	}
}

func TestTerminalWatcher_ClosePropagates(t *testing.T) {
	src := &chunkSource{chunks: [][]byte{[]byte(watchChatComplete)}}
	w := WatchStreamTerminal(src, OpenAI)
	if err := w.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	if !src.closed {
		t.Fatal("Close did not reach the upstream body")
	}
}
