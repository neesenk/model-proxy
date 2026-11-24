package protocol

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

// ObserveStreamTerminal (serve audit 2026-11-24) rides above a streaming
// converter, which already terminates its stream fail-closed with a terminal
// in the client protocol. The observer reports how those bytes ended so the
// executor can attribute the abort; it must never add a terminal of its own.

func TestObserveStreamTerminal_ReportsTheTerminalAndAppendsNothing(t *testing.T) {
	// The converters' own fail-closed terminals, in each client protocol.
	chatError := `{"error":{"param":null,"code":null,"message":"upstream stream terminated unexpectedly","type":"api_error"}}`
	anthropicError := `{"type":"error","error":{"type":"api_error","message":"upstream stream ended without a terminal stop_reason"}}`
	responsesFailed := `{"type":"response.failed","response":{"id":"resp_1","object":"response","status":"failed","error":{"code":"api_error","message":"upstream stream terminated before a terminal response event"}}}`

	cases := []struct {
		name        string
		proto       Protocol
		stream      string
		terminal    bool
		upstreamErr bool
		evidence    string
	}{
		{"chat converter error terminal", OpenAI, watchChatPartial + "data: " + chatError + "\n\n", true, true, chatError},
		{"chat complete", OpenAI, watchChatComplete, true, false, ""},
		{"chat: no terminal at all", OpenAI, watchChatPartial, false, false, ""},
		{"anthropic converter error event", Anthropic, watchAnthropicPartial + "event: error\ndata: " + anthropicError + "\n\n", true, true, anthropicError},
		{"anthropic complete", Anthropic, watchAnthropicComplete, true, false, ""},
		{"anthropic: no terminal at all", Anthropic, watchAnthropicPartial, false, false, ""},
		{"responses converter failed event", Responses, watchResponsesPartial + "event: response.failed\ndata: " + responsesFailed + "\n\n", true, true, responsesFailed},
		{"responses complete", Responses, watchResponsesComplete, true, false, ""},
		{"responses: no terminal at all", Responses, watchResponsesPartial, false, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Frames split across Read boundaries: classification must not
			// depend on how the converter hands them over.
			src := &chunkSource{chunks: [][]byte{[]byte(tc.stream[:len(tc.stream)/2]), []byte(tc.stream[len(tc.stream)/2:])}}
			w := ObserveStreamTerminal(src, tc.proto)
			out, _ := drainAll(t, w)
			if !bytes.Equal(out, []byte(tc.stream)) {
				t.Fatalf("the observer changed the bytes (a watching watcher would have appended a terminal):\n%s", out)
			}
			if w.Synthesized() {
				t.Error("Synthesized() = true; an observing watcher never appends a terminal")
			}
			if w.TerminalSeen() != tc.terminal {
				t.Errorf("TerminalSeen() = %v, want %v", w.TerminalSeen(), tc.terminal)
			}
			if w.UpstreamErrorTerminal() != tc.upstreamErr {
				t.Errorf("UpstreamErrorTerminal() = %v, want %v", w.UpstreamErrorTerminal(), tc.upstreamErr)
			}
			if got := w.UpstreamErrorEvidence(); got != tc.evidence {
				t.Errorf("UpstreamErrorEvidence() = %q, want %q", got, tc.evidence)
			}
		})
	}
}

// The underlying end is not the observer's to soften: a read error before
// any terminal surfaces unchanged, with nothing appended in front of it.
func TestObserveStreamTerminal_ReadErrorSurfacesUnchanged(t *testing.T) {
	readErr := errors.New("unexpected EOF")
	for proto, partial := range map[Protocol]string{
		OpenAI: watchChatPartial, Anthropic: watchAnthropicPartial, Responses: watchResponsesPartial,
	} {
		src := &chunkSource{chunks: [][]byte{[]byte(partial)}, end: readErr}
		w := ObserveStreamTerminal(src, proto)
		out, err := drainAll(t, w)
		if err != readErr || !bytes.Equal(out, []byte(partial)) {
			t.Errorf("%s: drained (%q, %v), want the source bytes then the source error untouched", proto, out, err)
		}
		if w.Synthesized() || w.TerminalSeen() {
			t.Errorf("%s: synthesized=%v terminal=%v; a cut stream must stay unreported as ended", proto, w.Synthesized(), w.TerminalSeen())
		}
	}
}

func TestObserveStreamTerminal_ClosePropagates(t *testing.T) {
	src := &chunkSource{}
	w := ObserveStreamTerminal(src, OpenAI)
	if err := w.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	if !src.closed {
		t.Error("Close did not reach the wrapped body")
	}
	if _, err := w.Read(make([]byte, 8)); err != io.EOF {
		t.Errorf("Read after Close = %v, want io.EOF", err)
	}
}
