// convert_stream.go — the anthropic↔openai-chat streaming converters
// (SSE chunk/event translators). Split from convert.go: request/response
// (non-streaming) converters live there; the shared SSE frame pump lives
// in sse_pump.go. Everything here is package-internal.

package protocol

import (
	"bufio"
	"encoding/json"
	sonic "github.com/bytedance/sonic"
	"io"
	"strings"
	"time"
)

// --- streaming: openai chat chunk → anthropic message events ---

const sseScanBuf = 8 * 1024 * 1024 // 8 MiB per line; oversized lines are warned + flushed

// openaiSSEToAnthropicSSE converts an OpenAI chat.completion.chunk stream into an
// Anthropic message event stream. Text streams live (content_block_delta as it
// arrives). Tool calls are BUFFERED per openai index and emitted as complete,
// sequential tool_use blocks at the end — because Anthropic's content_block model
// is strictly sequential (one open block at a time, no resuming a stopped block),
// it CANNOT represent OpenAI's interleaved parallel-tool fragment stream. Buffering
// avoids emitting input_json_delta for an already-stopped block (an invalid
// sequence) when tools interleave. usage from a trailing chunk (prompt+completion
// tokens) is carried into the terminal message_delta.usage.
type openaiSSEToAnthropicSSE struct {
	sc          *bufio.Scanner
	out         []byte
	model       string
	id          string
	started     bool
	closed      bool
	done        bool
	errored     bool
	bomStripped bool
	nextIdx     int                   // next anthropic content_block index
	curKind     string                // "" / "text" (tools are buffered, never "current")
	curIdx      int                   // anthropic index of the open text block
	tools       map[int]*streamedTool // openai tool index → buffered call
	toolOrder   []int                 // openai tool indices in first-seen order
	outTok      int                   // completion_tokens from trailing usage
	inTok       int                   // prompt_tokens from trailing usage
	cachedTok   int                   // prompt_tokens_details.cached_tokens from trailing usage
	createTok   int                   // cache write (cache_creation_input_tokens / cache_write_tokens)
	stopRsn     string                // finish_reason mapped to stop_reason
}

// streamedTool buffers one openai tool_call until the stream ends, so its
// tool_use block can be emitted as a complete, sequential anthropic block.
type streamedTool struct {
	id, name string
	args     []byte // accumulated arguments fragments (incremental JSON)
}

func newOpenAIToAnthropicSSE(r io.Reader, model string) *openaiSSEToAnthropicSSE {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), sseScanBuf)
	return &openaiSSEToAnthropicSSE{sc: sc, model: model, id: "msg_conv", tools: map[int]*streamedTool{}}
}

func (t *openaiSSEToAnthropicSSE) emit(event string, payload map[string]any) {
	b, _ := sonic.Marshal(payload)
	t.out = append(t.out, []byte("event: "+event+"\n")...)
	t.out = append(t.out, []byte("data: ")...)
	t.out = append(t.out, b...)
	t.out = append(t.out, []byte("\n\n")...)
}

func (t *openaiSSEToAnthropicSSE) ensureStart() {
	if t.started {
		return
	}
	t.started = true
	t.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": t.id, "type": "message", "role": "assistant",
			"model": t.model, "content": []any{}, "stop_reason": nil,
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
}

// closeBlock emits content_block_stop for the open block (if any) and clears it.
func (t *openaiSSEToAnthropicSSE) closeBlock() {
	if t.curKind != "" {
		t.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": t.curIdx})
		t.curKind = ""
	}
}

// openText opens a text block (closing any other open block first).
func (t *openaiSSEToAnthropicSSE) openText() {
	if t.curKind == "text" {
		return
	}
	t.closeBlock()
	t.curKind = "text"
	t.curIdx = t.nextIdx
	t.nextIdx++
	t.emit("content_block_start", map[string]any{
		"type": "content_block_start", "index": t.curIdx,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
}

// openThinking opens a thinking block (closing any other open block first).
func (t *openaiSSEToAnthropicSSE) openThinking() {
	if t.curKind == "thinking" {
		return
	}
	t.closeBlock()
	t.curKind = "thinking"
	t.curIdx = t.nextIdx
	t.nextIdx++
	t.emit("content_block_start", map[string]any{
		"type": "content_block_start", "index": t.curIdx,
		"content_block": map[string]any{"type": "thinking", "thinking": ""},
	})
}

// bufferTool accumulates an openai tool_call fragment (id+name on first sighting,
// argument fragments appended). The tool_use block is emitted as a complete,
// sequential block in finish() — never live — so interleaved parallel tools don't
// produce an invalid resume sequence.
func (t *openaiSSEToAnthropicSSE) bufferTool(i int, id, name, args string) {
	tc, seen := t.tools[i]
	if !seen {
		tc = &streamedTool{}
		t.tools[i] = tc
		t.toolOrder = append(t.toolOrder, i)
	}
	if id != "" {
		tc.id = id
	}
	if name != "" {
		tc.name = name
	}
	if args != "" {
		tc.args = append(tc.args, args...)
	}
}

func (t *openaiSSEToAnthropicSSE) finish() {
	if t.closed {
		return
	}
	t.ensureStart()
	t.closeBlock()
	for _, i := range t.toolOrder {
		if args := t.tools[i].args; len(args) > 0 && !json.Valid(args) {
			t.emit("error", map[string]any{
				"type": "error",
				"error": map[string]any{
					"type":    "api_error",
					"message": "upstream stream ended with incomplete tool arguments",
				},
			})
			t.errored = true
			t.closed = true
			return
		}
	}
	// Emit buffered tool_use blocks sequentially (one complete block each). Empty
	// arguments → a "{}" input_json_delta so the tool_use has valid JSON input.
	for _, i := range t.toolOrder {
		tc := t.tools[i]
		idx := t.nextIdx
		t.nextIdx++
		t.emit("content_block_start", map[string]any{
			"type": "content_block_start", "index": idx,
			"content_block": map[string]any{"type": "tool_use", "id": sanitizeToolUseID(tc.id), "name": tc.name, "input": map[string]any{}},
		})
		args := string(tc.args)
		if args == "" {
			args = "{}"
		}
		t.emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": idx,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
		})
		t.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": idx})
	}
	if len(t.toolOrder) > 0 && t.stopRsn == "" {
		t.stopRsn = "tool_use"
	}
	sr := t.stopRsn
	if sr == "" {
		sr = "end_turn"
	}
	// Same cache split as the non-streaming converter: openai's prompt_tokens
	// INCLUDES cached and cache-creation tokens; anthropic's input_tokens
	// excludes both (clamp ≥0).
	inTok := t.inTok - t.cachedTok - t.createTok
	if inTok < 0 {
		inTok = 0
	}
	usage := map[string]any{"input_tokens": inTok, "output_tokens": t.outTok}
	if t.cachedTok > 0 {
		usage["cache_read_input_tokens"] = t.cachedTok
	}
	if t.createTok > 0 {
		usage["cache_creation_input_tokens"] = t.createTok
	}
	t.emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": sr, "stop_sequence": nil},
		"usage": usage,
	})
	t.emit("message_stop", map[string]any{"type": "message_stop"})
	t.closed = true
}

func (t *openaiSSEToAnthropicSSE) Read(p []byte) (int, error) {
	if pumpSSEFrames(t, t.sc, &t.bomStripped, false) {
		return 0, io.EOF
	}
	n := copy(p, t.out)
	t.out = t.out[n:]
	return n, nil
}

func (t *openaiSSEToAnthropicSSE) hasOutput() bool { return len(t.out) > 0 }
func (t *openaiSSEToAnthropicSSE) isDone() bool    { return t.done }

// drainDone finishes the message (message_stop-shaped events) and reports
// whether the buffer is empty afterwards (→ io.EOF).
func (t *openaiSSEToAnthropicSSE) drainDone() (eof bool) {
	t.finish()
	return len(t.out) == 0
}

func (t *openaiSSEToAnthropicSSE) scanError(err error) {
	convertWarn("SSE scanner error (line too long?): " + err.Error())
	t.ensureStart()
	t.closeBlock()
	t.emit("error", map[string]any{"type": "error", "error": map[string]any{
		"type": "api_error", "message": "upstream stream terminated unexpectedly",
	}})
	t.errored = true
	t.closed = true
	t.done = true
}

func (t *openaiSSEToAnthropicSSE) streamEnd() {
	if t.stopRsn != "" {
		t.done = true
		return
	}
	t.ensureStart()
	t.closeBlock()
	t.emit("error", map[string]any{"type": "error", "error": map[string]any{
		"type": "api_error", "message": "upstream stream terminated before a terminal event",
	}})
	t.errored = true
	t.closed = true
	t.done = true
}

// dispatch ============================================================

func (t *openaiSSEToAnthropicSSE) dispatch(frameEvent string, dataEvents []string, payload string) {
	if payload == "[DONE]" {
		t.done = true
		return
	}
	// OpenAI error chunk (data: {"error":{...}}) → anthropic error event.
	// Never silently swallow a mid-stream upstream error as "normal finish".
	// The string form ({"error":"rate limited"} — some gateways emit it) is
	// recognized too: the object-only decode silently skips it.
	var errMessage, errType string
	// Both recognized error forms carry an "error" key; ordinary
	// delta/usage chunks skip the probes entirely.
	if strings.Contains(payload, `"error"`) {
		var errChunk struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		if sonic.UnmarshalString(payload, &errChunk) == nil && (errChunk.Error.Message != "" || errChunk.Error.Type != "") {
			errMessage, errType = errChunk.Error.Message, errChunk.Error.Type
		} else {
			var errString struct {
				Error string `json:"error"`
			}
			if sonic.UnmarshalString(payload, &errString) == nil && errString.Error != "" {
				errMessage = errString.Error
			}
		}
	}
	if errMessage == "" && errType == "" && frameEvent == "error" {
		// Explicit `event: error` frame whose payload carries no "error"
		// key — extract message/detail like the chat→responses sibling
		// (cc-switch extract_chat_sse_error).
		var data map[string]any
		if sonic.UnmarshalString(payload, &data) == nil {
			errMessage, errType = chatSSEErrorOf(data)
		}
	}
	if errMessage != "" || errType != "" {
		t.ensureStart()
		t.closeBlock()
		et := errType
		if et == "" {
			et = "api_error"
		}
		t.emit("error", map[string]any{"type": "error", "error": map[string]any{"type": et, "message": errMessage}})
		t.errored = true
		t.closed = true
		t.done = true
		return
	}
	type chatSSEChunk struct {
		ID      string `json:"id"`
		Model   string `json:"model"`
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				Refusal          string `json:"refusal"`
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        string `json:"reasoning"` // OpenRouter spelling
				ToolCalls        []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			PromptDetails    struct {
				CachedTokens     int `json:"cached_tokens"`
				CacheWriteTokens int `json:"cache_write_tokens"`
			} `json:"prompt_tokens_details"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"` // direct spelling
		} `json:"usage"`
	}
	for _, parsed := range parseFoldedSSEFrames[chatSSEChunk](payload) {
		if parsed.done {
			// A [DONE] line folded into a multi-frame payload (missing
			// blank line) is still the explicit terminator.
			t.done = true
			break
		}
		chunk := parsed.value
		if chunk.ID != "" {
			t.id = chunk.ID // pass the upstream's real message id through
		}
		if chunk.Model != "" {
			t.model = chunk.Model
		}
		if chunk.Usage != nil {
			t.inTok = chunk.Usage.PromptTokens
			t.outTok = chunk.Usage.CompletionTokens
			t.cachedTok = chunk.Usage.PromptDetails.CachedTokens
			t.createTok = chunk.Usage.CacheCreationInputTokens
			if t.createTok == 0 {
				t.createTok = chunk.Usage.PromptDetails.CacheWriteTokens
			}
		}
		// A non-empty finish_reason is Chat's semantic terminal. Keep
		// accepting later usage-only chunks until [DONE]/EOF, but ignore
		// any content/tool frames a malformed upstream emits after it —
		// including frames recovered from the same missing-blank payload.
		if t.stopRsn != "" {
			continue
		}
		t.ensureStart()
		if len(chunk.Choices) > 0 {
			c := chunk.Choices[0]
			if rc := firstNonEmpty(c.Delta.ReasoningContent, c.Delta.Reasoning); rc != "" {
				t.openThinking()
				t.emit("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": t.curIdx,
					"delta": map[string]any{"type": "thinking_delta", "thinking": rc},
				})
			}
			if c.Delta.Content != "" {
				t.openText()
				t.emit("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": t.curIdx,
					"delta": map[string]any{"type": "text_delta", "text": c.Delta.Content},
				})
			}
			// Refusal deltas stream as plain text (anthropic has no refusal
			// block; the finish_reason already maps to stop_reason refusal).
			if c.Delta.Refusal != "" {
				t.openText()
				t.emit("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": t.curIdx,
					"delta": map[string]any{"type": "text_delta", "text": c.Delta.Refusal},
				})
			}
			for _, tc := range c.Delta.ToolCalls {
				t.bufferTool(tc.Index, tc.ID, tc.Function.Name, tc.Function.Arguments)
			}
			if c.FinishReason != "" {
				t.stopRsn = mapFinishToStopReason(c.FinishReason)
			}
		}
	}
}

// --- streaming: anthropic message events → openai chat chunks ---

// anthropicSSEToOpenAISSE converts an Anthropic message event stream into an OpenAI
// chat.completion.chunk stream. tool_use blocks map to delta.tool_calls (index
// assigned 0,1,2… per tool_use block); input_json_delta partial_json fragments map
// verbatim to the tool_call's function.arguments.
type anthropicSSEToOpenAISSE struct {
	sc           *bufio.Scanner
	out          []byte
	model        string
	id           string
	created      int64 // chat.completion.chunk created (unix seconds, constant per stream)
	roleSent     bool
	done         bool
	finished     bool
	errored      bool // stream terminated via an error path — never emit usage/[DONE]
	doneDelim    bool // an explicit data: [DONE] delimiter arrived (OpenRouter dialect): a clean terminal even without a stop_reason
	usageSent    bool
	doneSent     bool
	bomStripped  bool
	stopRsn      string       // non-empty when a message_delta with stop_reason arrived; finish emitted on message_stop/[DONE]
	curBlock     int          // anthropic block index currently open
	curType      string       // "text" / "tool_use" / ""
	toolCallIdx  map[int]int  // anthropic block index → openai tool_call index
	toolArgsSeen map[int]bool // anthropic block index → got ≥1 input_json_delta
	nextTool     int
	inputTokens  int
	outputTokens int
	cacheRead    int // cache_read_input_tokens from message_start
	cacheCreate  int // cache_creation_input_tokens from message_start
	textRunes    int // rune length of all emitted text blocks
	curTextStart int // rune offset of the current Anthropic text block
	curText      string
	curThinking  string
	curSignature string
	curRedacted  string
}

func newAnthropicToOpenAISSE(r io.Reader, model string) *anthropicSSEToOpenAISSE {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), sseScanBuf)
	return &anthropicSSEToOpenAISSE{sc: sc, model: model, id: "chatcmpl-conv",
		created:     time.Now().Unix(),
		toolCallIdx: map[int]int{}, toolArgsSeen: map[int]bool{}}
}

// usagePayload builds the terminal chunk's usage: anthropic counts cache
// reads/creation separately from input_tokens; openai folds them into
// prompt_tokens (cache reads surfaced via prompt_tokens_details).
func (t *anthropicSSEToOpenAISSE) usagePayload() map[string]any {
	prompt := t.inputTokens + t.cacheRead + t.cacheCreate
	u := map[string]any{
		"prompt_tokens": prompt, "completion_tokens": t.outputTokens,
		"total_tokens": prompt + t.outputTokens,
	}
	if t.cacheRead > 0 {
		u["prompt_tokens_details"] = map[string]any{"cached_tokens": t.cacheRead}
	}
	return u
}

func (t *anthropicSSEToOpenAISSE) emitChunk(delta map[string]any, finish any, usage map[string]any) {
	m := map[string]any{
		"id": t.id, "object": "chat.completion.chunk", "model": t.model,
		"created": t.created,
		"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
	}
	if usage != nil {
		m["usage"] = usage
	}
	b, _ := sonic.Marshal(m)
	t.out = append(t.out, []byte("data: ")...)
	t.out = append(t.out, b...)
	t.out = append(t.out, []byte("\n\n")...)
}

// emitUsageChunk emits the spec-shaped terminal usage chunk
// (ChatCompletionStreamOptions.include_usage): an additional chunk BEFORE
// data: [DONE] whose choices is an empty array and whose usage shows the
// token usage. The finish chunk itself carries only finish_reason.
func (t *anthropicSSEToOpenAISSE) emitUsageChunk() {
	m := map[string]any{
		"id": t.id, "object": "chat.completion.chunk", "model": t.model,
		"created": t.created,
		"choices": []any{}, "usage": t.usagePayload(),
	}
	b, _ := sonic.Marshal(m)
	t.out = append(t.out, []byte("data: ")...)
	t.out = append(t.out, b...)
	t.out = append(t.out, []byte("\n\n")...)
}

func (t *anthropicSSEToOpenAISSE) ensureRole() {
	if !t.roleSent {
		t.emitChunk(map[string]any{"role": "assistant"}, nil, nil)
		t.roleSent = true
	}
}

func (t *anthropicSSEToOpenAISSE) Read(p []byte) (int, error) {
	if pumpSSEFrames(t, t.sc, &t.bomStripped, false) {
		return 0, io.EOF
	}
	n := copy(p, t.out)
	t.out = t.out[n:]
	return n, nil
}

func (t *anthropicSSEToOpenAISSE) hasOutput() bool { return len(t.out) > 0 }
func (t *anthropicSSEToOpenAISSE) isDone() bool    { return t.done }

// drainDone emits the finish chunk (when message_stop hasn't), then the
// usage chunk + data: [DONE]. The only clean terminal without an explicit
// message_stop is a data: [DONE] delimiter (OpenRouter dialect), which is a
// deliberate terminator — see the parity tests. Anything else that reached
// the terminator (bare message_stop, message_delta with an empty stop_reason,
// or EOF before message_stop) was truncated upstream: synthesizing a finish
// chunk would fake a clean terminal on exactly the client bytes the
// executor's cache gate checks, so it fails closed like streamEnd — error
// chunk only, no usage, no [DONE].
func (t *anthropicSSEToOpenAISSE) drainDone() (eof bool) {
	if !t.finished && !t.errored && !t.doneDelim && t.stopRsn == "" {
		t.emitStreamErrorChunk("upstream stream ended without a terminal stop_reason")
		t.finished = true
		t.errored = true
		t.done = true
	}
	if !t.finished {
		finish := "stop"
		if t.stopRsn != "" {
			finish = mapStopReasonToFinish(t.stopRsn)
		}
		t.emitChunk(map[string]any{}, finish, nil)
		t.finished = true
	}
	if !t.errored {
		if !t.usageSent {
			t.emitUsageChunk()
			t.usageSent = true
		}
		if !t.doneSent {
			t.out = append(t.out, []byte("data: [DONE]\n\n")...)
			t.doneSent = true
		}
	}
	return len(t.out) == 0
}

// emitStreamErrorChunk appends the OpenAI-shaped terminal error chunk.
func (t *anthropicSSEToOpenAISSE) emitStreamErrorChunk(message string) {
	errObj, _ := sonic.Marshal(map[string]any{
		"message": message,
		"type":    "api_error", "param": nil, "code": nil,
	})
	t.out = append(t.out, []byte("data: {\"error\":")...)
	t.out = append(t.out, errObj...)
	t.out = append(t.out, []byte("}\n\n")...)
}

func (t *anthropicSSEToOpenAISSE) scanError(err error) {
	convertWarn("SSE scanner error (line too long?): " + err.Error())
	t.emitStreamErrorChunk("upstream stream terminated unexpectedly")
	t.finished = true
	t.errored = true
	t.done = true
}

func (t *anthropicSSEToOpenAISSE) streamEnd() {
	if !t.finished {
		t.emitStreamErrorChunk("upstream stream terminated before a terminal event")
		t.finished = true
		t.errored = true
	}
	t.done = true
}

// dispatch ============================================================

func (t *anthropicSSEToOpenAISSE) dispatch(frameEvent string, dataEvents []string, payload string) {
	if payload == "[DONE]" {
		// [DONE] terminates the stream in the chat dialect; some gateways
		// append it to anthropic-event streams (OpenRouter-style). Treat
		// it as a clean terminal exactly like the sibling directions: the
		// done-branch above emits the finish chunk (when message_delta
		// hasn't) + data: [DONE], and later frames are never scanned.
		t.doneDelim = true
		t.done = true
		return
	}
	type anthropicSSEEvent struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			Signature   string `json:"signature"`
			PartialJSON string `json:"partial_json"`
			StopReason  string `json:"stop_reason"`
			Citation    any    `json:"citation"`
		} `json:"delta"`
		ContentBlock struct {
			Type string `json:"type"`
			ID   string `json:"id"`
			Name string `json:"name"`
			Data string `json:"data"`
		} `json:"content_block"`
		Message struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Usage struct {
				InputTokens *int `json:"input_tokens"`
				CacheRead   *int `json:"cache_read_input_tokens"`
				CacheCreate *int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
		Usage struct {
			InputTokens  *int `json:"input_tokens"`
			OutputTokens *int `json:"output_tokens"`
			CacheRead    *int `json:"cache_read_input_tokens"`
			CacheCreate  *int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	for _, parsed := range parseFoldedSSEFrames[anthropicSSEEvent](payload) {
		if parsed.done {
			// A [DONE] line folded into a multi-frame payload (missing
			// blank line) is still the explicit terminator.
			t.done = true
			break
		}
		ev := parsed.value
		if ev.Message.Model != "" {
			t.model = ev.Message.Model
		}
		if t.finished || t.stopRsn != "" {
			// Post-terminal guard: once a terminal message_delta arrives,
			// ignore any content/tool frames a malformed upstream emits
			// after it (sibling directions suppress the same way).
			// message_stop/error still terminate the stream.
			switch ev.Type {
			case "content_block_start", "content_block_delta", "content_block_stop":
				continue
			}
		}
		switch ev.Type {
		case "message_start":
			updateUsageValue(ev.Message.Usage.InputTokens, &t.inputTokens)
			updateUsageValue(ev.Message.Usage.CacheRead, &t.cacheRead)
			updateUsageValue(ev.Message.Usage.CacheCreate, &t.cacheCreate)
			if ev.Message.ID != "" {
				t.id = ev.Message.ID // pass the upstream's real message id through
			}
		case "error":
			// anthropic error event → openai error chunk, fail-closed (no
			// usage chunk / [DONE] after it). Don't silently turn an
			// upstream error into a clean finish.
			et := ev.Error.Type
			if et == "" {
				et = "api_error"
			}
			errObj, _ := sonic.Marshal(map[string]any{"message": ev.Error.Message, "type": et, "param": nil, "code": nil})
			t.out = append(t.out, []byte("data: {\"error\":")...)
			t.out = append(t.out, errObj...)
			t.out = append(t.out, []byte("}\n\n")...)
			t.finished = true
			t.errored = true
			t.done = true
		case "content_block_start":
			t.curBlock = ev.Index
			t.curType = ev.ContentBlock.Type
			if ev.ContentBlock.Type == "text" {
				t.curTextStart = t.textRunes
				t.curText = ""
			} else if ev.ContentBlock.Type == "thinking" {
				t.curThinking = ""
				t.curSignature = ""
			} else if ev.ContentBlock.Type == "redacted_thinking" {
				t.curRedacted = ev.ContentBlock.Data
			}
			if ev.ContentBlock.Type == "tool_use" {
				tcIdx := t.nextTool
				t.nextTool++
				t.toolCallIdx[ev.Index] = tcIdx
				t.ensureRole()
				t.emitChunk(map[string]any{"tool_calls": []map[string]any{{
					"index": tcIdx, "id": ev.ContentBlock.ID, "type": "function",
					"function": map[string]any{"name": ev.ContentBlock.Name, "arguments": ""},
				}}}, nil, nil)
			}
		case "content_block_delta":
			switch ev.Delta.Type {
			case "text_delta":
				if ev.Delta.Text != "" {
					t.ensureRole()
					t.emitChunk(map[string]any{"content": ev.Delta.Text}, nil, nil)
					t.curText += ev.Delta.Text
					t.textRunes += len([]rune(ev.Delta.Text))
				}
			case "citations_delta":
				annotations := anthropicCitationsToChat([]any{ev.Delta.Citation}, t.curText, t.curTextStart)
				if len(annotations) > 0 {
					t.ensureRole()
					t.emitChunk(map[string]any{"annotations": annotations}, nil, nil)
				}
			case "thinking_delta":
				if thinking := firstNonEmpty(ev.Delta.Thinking, ev.Delta.Text); thinking != "" {
					t.ensureRole()
					t.emitChunk(map[string]any{"reasoning_content": thinking}, nil, nil)
					t.curThinking += thinking
				}
			case "signature_delta":
				t.curSignature += ev.Delta.Signature
			case "input_json_delta":
				if ev.Delta.PartialJSON != "" {
					if tcIdx, ok := t.toolCallIdx[t.curBlock]; ok && t.curType == "tool_use" {
						t.toolArgsSeen[t.curBlock] = true
						t.emitChunk(map[string]any{"tool_calls": []map[string]any{{
							"index": tcIdx, "function": map[string]any{"arguments": ev.Delta.PartialJSON},
						}}}, nil, nil)
					}
				}
			default:
				// thinking_delta/signature_delta/... have no openai equivalent.
				if ev.Delta.Type != "" {
					convertWarn("dropping " + ev.Delta.Type + " delta (no cross-protocol equivalent)")
				}
			}
		case "content_block_stop":
			if t.curType == "thinking" && t.curSignature != "" {
				t.emitChunk(map[string]any{"reasoning_details": []map[string]any{{
					"type": "anthropic_thinking", "thinking": t.curThinking, "signature": t.curSignature,
				}}}, nil, nil)
			} else if t.curType == "redacted_thinking" && t.curRedacted != "" {
				t.emitChunk(map[string]any{"reasoning_details": []map[string]any{{
					"type": "anthropic_redacted_thinking", "data": t.curRedacted,
				}}}, nil, nil)
			}
			// Empty-args fallback: a tool_use block with no input_json_delta still
			// gets a "{}" arguments fragment (openai requires valid JSON arguments).
			if t.curType == "tool_use" {
				if tcIdx, ok := t.toolCallIdx[t.curBlock]; ok && !t.toolArgsSeen[t.curBlock] {
					t.emitChunk(map[string]any{"tool_calls": []map[string]any{{
						"index": tcIdx, "function": map[string]any{"arguments": "{}"},
					}}}, nil, nil)
				}
			}
			t.curType = ""
		case "message_delta":
			// Vendors differ on where final usage lands: some put input/cache
			// only on message_delta, while Kimi moves input into cache_read at
			// the terminal event. Update by field presence (including explicit
			// zero), otherwise keep the message_start value.
			updateUsageValue(ev.Usage.InputTokens, &t.inputTokens)
			updateUsageValue(ev.Usage.CacheRead, &t.cacheRead)
			updateUsageValue(ev.Usage.CacheCreate, &t.cacheCreate)
			updateUsageValue(ev.Usage.OutputTokens, &t.outputTokens)
			// Record a non-empty stop_reason but do NOT emit the finish chunk
			// yet. The same-protocol cache gate requires both a
			// stop_reason-carrying message_delta AND a message_stop; emitting
			// finish_reason here and then reaching EOF without message_stop
			// would let the converted stream be judged terminal-complete and
			// poison the cache.
			if !t.finished && ev.Delta.StopReason != "" {
				t.stopRsn = ev.Delta.StopReason
			}
		case "message_stop":
			// A bare message_stop without a preceding stop_reason-carrying
			// message_delta is a truncated generation — fail-closed like EOF
			// without a terminal signal.
			if t.stopRsn == "" {
				t.emitStreamErrorChunk("upstream stream ended without a terminal stop_reason")
				t.finished = true
				t.errored = true
			}
			t.done = true
		}
		if t.done {
			break
		}
	}
}

func updateUsageValue(value *int, target *int) {
	if value != nil {
		*target = *value
	}
}

// backendPath returns the upstream request path for a backend protocol (used when
