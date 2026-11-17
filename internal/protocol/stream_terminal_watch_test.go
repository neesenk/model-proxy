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

// An `event: error` frame whose payload carries no "error" key (OpenRouter
// chat dialect: message/detail fields) is an upstream error terminal — no
// second synthesized error may follow it.
func TestTerminalWatcher_ChatEventLineErrorFrameNoSynthesis(t *testing.T) {
	stream := watchChatPartial +
		"event: error\n" +
		`data: {"message":"provider overloaded","detail":"retry later"}` + "\n\n"
	src := &chunkSource{chunks: [][]byte{[]byte(stream)}}
	w := WatchStreamTerminal(src, OpenAI)
	out, err := drainAll(t, w)
	if err != io.EOF {
		t.Fatalf("end error = %v, want io.EOF", err)
	}
	if !bytes.Equal(out, []byte(stream)) {
		t.Fatalf("event-line error stream was modified:\n%s", out)
	}
	if w.Synthesized() {
		t.Fatal("Synthesized() = true behind an event:-only upstream error frame")
	}
}

// Anthropic dialect naming the frame only on the event: line (payload without
// a type key): both message_stop and error must classify from the event name.
func TestTerminalWatcher_AnthropicEventLineOnlyClassification(t *testing.T) {
	stop := watchAnthropicPartial +
		"event: message_stop\n" +
		`data: {"stop":true}` + "\n\n"
	src := &chunkSource{chunks: [][]byte{[]byte(stop)}}
	w := WatchStreamTerminal(src, Anthropic)
	out, err := drainAll(t, w)
	if err != io.EOF {
		t.Fatalf("end error = %v, want io.EOF", err)
	}
	if !bytes.Equal(out, []byte(stop)) || w.Synthesized() {
		t.Fatal("event:-only message_stop was not recognized as a terminal")
	}

	failed := watchAnthropicPartial +
		"event: error\n" +
		`data: {"message":"overloaded"}` + "\n\n"
	src = &chunkSource{chunks: [][]byte{[]byte(failed)}}
	w = WatchStreamTerminal(src, Anthropic)
	out, err = drainAll(t, w)
	if err != io.EOF {
		t.Fatalf("end error = %v, want io.EOF", err)
	}
	if !bytes.Equal(out, []byte(failed)) || w.Synthesized() {
		t.Fatal("event:-only error frame was not recognized as an upstream error terminal")
	}
}

// Responses dialect naming the frame only on the event: line: a
// response.failed whose payload lacks the type key must not be followed by a
// synthesized response.failed.
func TestTerminalWatcher_ResponsesEventLineFailedNoSynthesis(t *testing.T) {
	stream := watchResponsesPartial +
		"event: response.failed\n" +
		`data: {"response":{"id":"resp_1","object":"response","status":"failed"}}` + "\n\n"
	src := &chunkSource{chunks: [][]byte{[]byte(stream)}}
	w := WatchStreamTerminal(src, Responses)
	out, err := drainAll(t, w)
	if err != io.EOF {
		t.Fatalf("end error = %v, want io.EOF", err)
	}
	if !bytes.Equal(out, []byte(stream)) {
		t.Fatalf("event:-only response.failed stream was modified:\n%s", out)
	}
	if w.Synthesized() {
		t.Fatal("Synthesized() = true behind an event:-only response.failed frame")
	}
}

// A single SSE line exceeding the shared 8 MiB cap disables terminal
// detection for the rest of the stream: the bytes forward untouched and no
// terminal is synthesized on the later end (the scanner can no longer prove
// one is missing). Memory-bounded by the same cap the converters enforce.
func TestTerminalWatcher_OversizedLineDisablesDetectionWithoutRewrite(t *testing.T) {
	stream := watchChatPartial +
		"data: " + strings.Repeat("x", sseScanBuf+64) + "\n\n"
	// chunkSource drops the unread tail of a chunk, so serve the 8 MiB+
	// stream through a plain reader instead.
	w := WatchStreamTerminal(io.NopCloser(bytes.NewReader([]byte(stream))), OpenAI)
	out, err := drainAll(t, w)
	if err != io.EOF {
		t.Fatalf("end error = %v, want io.EOF", err)
	}
	if !bytes.Equal(out, []byte(stream)) {
		t.Fatal("oversized-line stream was modified")
	}
	if w.Synthesized() {
		t.Fatal("Synthesized() = true after the scanner gave up on an oversized line")
	}
}

// TestTerminalWatcher_KeySpellingsDecodedLikeTheFrameParse pins what the
// frame decode accepts as the terminal/error fields — ASCII case variants of
// the keys and \u escapes in keys and values all count — so a pre-filter that
// skips decoding frames which cannot carry them must keep these outcomes.
func TestTerminalWatcher_KeySpellingsDecodedLikeTheFrameParse(t *testing.T) {
	cases := []struct {
		name            string
		proto           Protocol
		body            string
		terminal, error bool
	}{
		{"openai title-case finish_reason", OpenAI, "data: {\"choices\":[{\"Finish_Reason\":\"stop\"}]}\n\n", true, false},
		{"openai escaped finish_reason key", OpenAI, "data: {\"choices\":[{\"finish\\u005freason\":\"stop\"}]}\n\n", true, false},
		{"openai spaced finish_reason", OpenAI, "data: {\"choices\":[{\"finish_reason\" : \"length\"}]}\n\n", true, false},
		{"openai null finish_reason", OpenAI, "data: {\"choices\":[{\"finish_reason\":null}]}\n\n", false, false},
		{"openai upper-case error", OpenAI, "data: {\"ERROR\":{\"message\":\"x\"}}\n\n", false, true},
		{"openai null error", OpenAI, "data: {\"error\":null,\"choices\":[]}\n\n", false, false},
		{"anthropic title-case type", Anthropic, "event: x\ndata: {\"Type\":\"message_stop\"}\n\n", true, false},
		{"anthropic escaped type value", Anthropic, "event: x\ndata: {\"type\":\"message\\u005fstop\"}\n\n", true, false},
		{"anthropic event fallback", Anthropic, "event: message_stop\ndata: {\"no_type\":1}\n\n", true, false},
		{"anthropic delta mentioning error", Anthropic, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"error\"}}\n\n", false, false},
	}
	for _, tc := range cases {
		watcher := WatchStreamTerminal(io.NopCloser(strings.NewReader(tc.body)), tc.proto)
		if _, err := io.ReadAll(watcher); err != nil {
			t.Fatalf("%s: read: %v", tc.name, err)
		}
		if watcher.terminal != tc.terminal || watcher.sawError != tc.error {
			t.Errorf("%s: terminal=%v sawError=%v, want terminal=%v sawError=%v", tc.name, watcher.terminal, watcher.sawError, tc.terminal, tc.error)
		}
	}
}

// TestTerminalWatcher_AllocationsDoNotScaleWithFrames: the watcher shadows
// every chunk of every same-protocol stream. Line splitting must not copy,
// and an openai delta chunk whose finish_reason is null (and carries no
// error key) cannot change the verdict, so it is not decoded: doubling a
// stream past its buffer warm-up (1500 → 3000 chunks) adds no per-chunk allocation.
func TestTerminalWatcher_AllocationsDoNotScaleWithFrames(t *testing.T) {
	stream := func(chunks int) string {
		var b strings.Builder
		for i := 0; i < chunks; i++ {
			b.WriteString("data: {\"id\":\"c1\",\"object\":\"chat.completion.chunk\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"tok\"},\"finish_reason\":null}]}\n\n")
		}
		b.WriteString("data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		return b.String()
	}
	// A caller-owned copy buffer, and wrappers hiding ReaderFrom/WriterTo:
	// io.Discard's ReadFrom draws pooled buffers, and the race detector
	// randomly drops sync.Pool items — an allocation outside the watcher.
	buf := make([]byte, 8<<10)
	allocs := func(raw string) float64 {
		return testing.AllocsPerRun(10, func() {
			watcher := WatchStreamTerminal(io.NopCloser(strings.NewReader(raw)), OpenAI)
			if _, err := io.CopyBuffer(struct{ io.Writer }{io.Discard}, struct{ io.Reader }{watcher}, buf); err != nil {
				t.Fatal(err)
			}
			if watcher.Synthesized() {
				t.Fatal("complete stream synthesized a terminal")
			}
		})
	}
	// The rate, not exact equality: the race detector's runtime allocates
	// sporadically (±1 per run); a per-chunk allocation shows as >= 1500.
	if half, full := allocs(stream(1500)), allocs(stream(3000)); (full-half)/1500 >= 0.01 {
		t.Errorf("allocations: 3000 chunks = %v, 1500 chunks = %v; want no per-chunk allocation", full, half)
	}
}
