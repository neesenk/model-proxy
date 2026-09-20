// stream_terminal_watch.go — same-protocol passthrough terminal watching.
//
// The streaming converters already fail closed on truncated upstream streams:
// each of the six conversion directions synthesizes a protocol-native
// terminal error when the upstream ends before its terminal sequence (see
// convert_stream.go, convert_responses_stream.go, convert_responses_stream_to.go).
// The same-protocol passthrough path used to be a raw byte copy, so an
// upstream that abandoned a generation mid-stream (observed live: zhipu
// dropping an anthropic SSE connection during the thinking phase) reached the
// client as a bare EOF — state-tracking clients (Anthropic SDKs, pi) hard-fail
// on the missing terminal, lenient ones (most OpenAI SDKs) silently accept a
// truncated answer.
//
// TerminalWatcher forwards upstream bytes UNTOUCHED while shadow-scanning SSE
// frame boundaries for the client protocol's terminal sequence:
//
//   - anthropic: message_stop (an upstream error event is also terminal)
//   - openai:    a non-empty finish_reason or a data: [DONE] delimiter
//   - responses: response.completed / response.incomplete / response.failed
//
// When the upstream body ends (EOF or a read error) after the stream has
// begun (at least one data: line) without any terminal, the watcher appends a
// synthesized protocol-native terminal error event BEFORE reporting the end:
// anthropic event: error, an openai error chunk (never a fake finish_reason
// and never [DONE] — the same fail-closed rule as the converters), responses
// response.failed. The messages deliberately use wording that agent clients
// classify as retryable provider errors (pi matches "stream ended before
// message_stop", "ended without", "stream ended before a terminal response
// event").
//
// A bare anthropic message_stop without a stop_reason (observed from a kimi
// upstream) is NOT complete for cache purposes (StreamTerminalComplete is the
// authority there) but IS client-acceptable: the bytes are forwarded
// untouched and nothing is synthesized.
package protocol

import (
	"bytes"
	"io"
	"strings"

	sonic "github.com/bytedance/sonic"
)

// Synthesized terminal messages. The wording is a client-facing contract:
// agent clients (pi) retry provider errors by matching error-message
// patterns, so these must keep containing the substrings those patterns list.
const (
	watchAnthropicTruncated = "upstream stream ended before message_stop"
	watchOpenAITruncated    = "upstream stream ended without a terminal finish_reason"
	watchResponsesTruncated = "upstream stream ended before a terminal response event"
)

// Synthesized-event payload types. Structs (not maps): sonic does not sort
// map keys, so map-built events would serialize in a different key order on
// every call — these bytes are a client-facing contract and must be stable.
type watchAnthropicErrorEvent struct {
	Type  string `json:"type"`
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type watchOpenAIErrorChunk struct {
	Error struct {
		Code    *string `json:"code"`
		Message string  `json:"message"`
		Param   *string `json:"param"`
		Type    string  `json:"type"`
	} `json:"error"`
}

type watchResponsesFailedEvent struct {
	Type     string `json:"type"`
	Response struct {
		ID     string `json:"id"`
		Object string `json:"object"`
		Status string `json:"status"`
		Error  struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	} `json:"response"`
}

// WatchStreamTerminal wraps a same-protocol streaming SSE body so a truncated
// upstream stream still reaches the client with a protocol-native terminal
// error instead of a bare EOF. The wrapped body must be the raw upstream
// stream (below any effects/recording wrappers): every byte the upstream sent
// is forwarded unchanged, and the synthesized terminal — appended only when
// the upstream ends without one — becomes part of the client-visible stream.
func WatchStreamTerminal(body io.ReadCloser, proto Protocol) *TerminalWatcher {
	return &TerminalWatcher{body: body, proto: proto}
}

// TerminalWatcher is the passthrough terminal scanner. It is NOT a converter:
// it never rewrites, reorders or drops upstream bytes.
type TerminalWatcher struct {
	body  io.ReadCloser
	proto Protocol

	// Shadow-scanner state (operates on copies; never blocks forwarding).
	pend       []byte // trailing partial line of the bytes scanned so far
	dataOpen   bool   // a data: line opened the frame in progress
	dataLines  []string
	frameEvent string // latest event: line since the frame opened
	sawData    bool   // any data: line reached the scanner (stream began)
	terminal   bool   // client-acceptable terminal sequence seen
	sawError   bool   // protocol-native error terminal seen upstream
	respID     string // responses: response id tracked from the first event

	// Delivery state.
	synth   []byte // pending synthesized terminal, served before the end
	synthed bool   // sticky: a terminal was synthesized (survives draining)
	end     error  // underlying end error, served after the synth
	done    bool   // underlying end consumed
}

// Synthesized reports whether a terminal error event was appended because the
// upstream stream ended without its terminal sequence. Sticky: it stays true
// after the synthesized bytes have been read out.
func (w *TerminalWatcher) Synthesized() bool { return w.synthed }

// Read forwards the upstream bytes untouched. On the upstream end it first
// serves the synthesized terminal (when one was warranted), then the original
// end error.
func (w *TerminalWatcher) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		if len(w.synth) > 0 {
			n := copy(p, w.synth)
			w.synth = w.synth[n:]
			return n, nil
		}
		if w.done {
			return 0, w.end
		}
		n, err := w.body.Read(p)
		if n > 0 {
			w.scan(p[:n])
		}
		if err != nil {
			w.finish(err)
			if n > 0 {
				// Payload first; the synthesized terminal (if any) and the
				// saved end surface on the following Read calls.
				return n, nil
			}
			continue
		}
		if n > 0 {
			return n, nil
		}
		// n == 0, err == nil: permitted but uninformative — read again.
	}
}

// Close propagates to the upstream body (the watcher adds no resources).
func (w *TerminalWatcher) Close() error { return w.body.Close() }

// finish consumes the upstream end. Synthesis is warranted only when the
// stream began (an empty 200 must keep the executor's zero-byte failure path)
// and no client-acceptable terminal and no upstream error terminal arrived.
func (w *TerminalWatcher) finish(err error) {
	w.done = true
	w.end = err
	if !w.terminal && !w.sawError && w.sawData {
		w.synth = w.synthesize()
		w.synthed = true
	}
}

// scan feeds forwarded bytes through the shadow scanner. Chunk boundaries are
// irrelevant: only complete lines (up to \n) are classified, and a partial
// trailing line stays buffered until the next chunk.
func (w *TerminalWatcher) scan(chunk []byte) {
	w.pend = append(w.pend, chunk...)
	for {
		idx := bytes.IndexByte(w.pend, '\n')
		if idx < 0 {
			return
		}
		line := strings.TrimRight(string(w.pend[:idx]), "\r")
		w.pend = w.pend[idx+1:]
		w.line(line)
	}
}

// line classifies one complete SSE line. It mirrors the frame semantics of
// the shared pump (sse_pump.go): only a blank line dispatches a frame, data:
// lines fold, and an event: line names the frame in progress. Classification
// drives terminal detection only — the forwarded bytes are never touched.
func (w *TerminalWatcher) line(line string) {
	switch {
	case strings.HasPrefix(line, "data:"):
		w.dataOpen = true
		w.dataLines = append(w.dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		w.sawData = true
	case line == "":
		if w.dataOpen {
			w.dispatch(strings.Join(w.dataLines, "\n"))
			w.dataOpen = false
			w.dataLines = nil
		}
		w.frameEvent = ""
	case strings.HasPrefix(line, "event:"):
		w.frameEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
	}
}

// dispatch inspects one complete SSE frame's folded data payload. Detection
// keys off the payload's own "type" (some upstreams omit event: lines
// entirely); parse failures are ignored — garbage frames are forwarded
// untouched and merely escape terminal detection.
func (w *TerminalWatcher) dispatch(payload string) {
	switch w.proto {
	case Anthropic:
		var frame struct {
			Type string `json:"type"`
		}
		if sonic.UnmarshalString(payload, &frame) != nil {
			return
		}
		switch frame.Type {
		case "message_stop":
			w.terminal = true
		case "error":
			w.sawError = true
		}
	case OpenAI:
		if payload == "[DONE]" {
			w.terminal = true
			return
		}
		var frame struct {
			Error   any `json:"error"`
			Choices []struct {
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if sonic.UnmarshalString(payload, &frame) != nil {
			return
		}
		if frame.Error != nil {
			w.sawError = true
			return
		}
		for _, choice := range frame.Choices {
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				w.terminal = true
			}
		}
	case Responses:
		var frame struct {
			Type     string `json:"type"`
			Response struct {
				ID string `json:"id"`
			} `json:"response"`
		}
		if sonic.UnmarshalString(payload, &frame) != nil {
			return
		}
		if frame.Response.ID != "" && w.respID == "" {
			w.respID = frame.Response.ID
		}
		switch frame.Type {
		case "response.completed", "response.incomplete":
			w.terminal = true
		case "response.failed":
			w.sawError = true
		}
	}
}

// sseEmitJSON appends one SSE event frame (event: line + data: payload) to
// dst. It is sseEmit's any-typed variant for struct payloads, whose field
// order sonic keeps stable (map keys it does not).
func sseEmitJSON(dst *[]byte, event string, payload any) {
	b, _ := sonic.Marshal(payload)
	*dst = append(*dst, []byte("event: "+event+"\n")...)
	*dst = append(*dst, []byte("data: ")...)
	*dst = append(*dst, b...)
	*dst = append(*dst, []byte("\n\n")...)
}

// mustSonicMarshal marshals a struct payload; struct field order (unlike map
// key order) is stable under sonic, and a struct cannot fail to marshal.
func mustSonicMarshal(v any) []byte {
	b, err := sonic.Marshal(v)
	if err != nil {
		panic("protocol: struct marshal failed: " + err.Error())
	}
	return b
}

// synthesize builds the protocol-native terminal error event. The payload
// types are structs so the wire bytes are deterministic (see their comment);
// the frame shape matches the converters' premature-end emissions.
func (w *TerminalWatcher) synthesize() []byte {
	var out []byte
	switch w.proto {
	case Anthropic:
		var event watchAnthropicErrorEvent
		event.Type = "error"
		event.Error.Type = "api_error"
		event.Error.Message = watchAnthropicTruncated
		sseEmitJSON(&out, "error", event)
	case OpenAI:
		var chunk watchOpenAIErrorChunk
		chunk.Error.Message = watchOpenAITruncated
		chunk.Error.Type = "api_error"
		out = append(out, []byte("data: ")...)
		out = append(out, mustSonicMarshal(chunk)...)
		out = append(out, []byte("\n\n")...)
	case Responses:
		var event watchResponsesFailedEvent
		event.Type = "response.failed"
		event.Response.ID = w.respID
		event.Response.Object = "response"
		event.Response.Status = "failed"
		event.Response.Error.Code = "api_error"
		event.Response.Error.Message = watchResponsesTruncated
		sseEmitJSON(&out, "response.failed", event)
	}
	return out
}
