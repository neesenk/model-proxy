package protocol

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"
)

// The executor's post-commit attribution reads the watcher's verdict through
// TerminalSeen / UpstreamErrorTerminal / UpstreamErrorEvidence (serve audit
// 2026-11-22): the request log keeps an upstream-failed stream as a plain
// status 200, so these are what put the failure into serve.log. They are
// read-only observations — the forwarded bytes must stay untouched.

func TestTerminalWatcher_VerdictAccessorsClassifyHowTheStreamEnded(t *testing.T) {
	chatError := `{"error":{"message":"upstream stream terminated unexpectedly","type":"api_error"}}`
	chatEventError := `{"message":"boom","code":500}`
	anthropicError := `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`
	responsesFailed := `{"type":"response.failed","response":{"id":"resp_1","object":"response","status":"failed","error":{"code":"server_error","message":"boom"}}}`

	cases := []struct {
		name        string
		proto       Protocol
		stream      string
		terminal    bool
		upstreamErr bool
		evidence    string
	}{
		{"chat cut before any terminal", OpenAI, watchChatPartial, false, false, ""},
		{"chat [DONE]", OpenAI, watchChatPartial + "data: [DONE]\n\n", true, false, ""},
		{"chat finish_reason", OpenAI, watchChatPartial +
			`data: {"id":"c1","model":"gpt-x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n", true, false, ""},
		{"chat error chunk", OpenAI, watchChatPartial + "data: " + chatError + "\n\n", true, true, chatError},
		{"chat event: error frame", OpenAI, watchChatPartial + "event: error\ndata: " + chatEventError + "\n\n", true, true, chatEventError},
		{"anthropic cut", Anthropic, watchAnthropicPartial, false, false, ""},
		{"anthropic message_stop", Anthropic, watchAnthropicComplete, true, false, ""},
		{"anthropic error event", Anthropic, watchAnthropicPartial + "event: error\ndata: " + anthropicError + "\n\n", true, true, anthropicError},
		{"responses cut", Responses, watchResponsesPartial, false, false, ""},
		{"responses completed", Responses, watchResponsesComplete, true, false, ""},
		{"responses response.failed", Responses, watchResponsesPartial + "event: response.failed\ndata: " + responsesFailed + "\n\n", true, true, responsesFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := &chunkSource{chunks: [][]byte{[]byte(tc.stream)}}
			w := WatchStreamTerminal(src, tc.proto)
			out, _ := drainAll(t, w)
			// Upstream bytes always lead untouched; a stream that reached a
			// terminal (either kind) gets nothing appended.
			if !bytes.HasPrefix(out, []byte(tc.stream)) || (tc.terminal && !bytes.Equal(out, []byte(tc.stream))) {
				t.Fatalf("forwarded bytes were mutated:\n%s", out)
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
			// A cut stream gets a synthesized terminal; one that ended on an
			// upstream terminal (either kind) never does.
			if got := w.Synthesized(); got != !tc.terminal {
				t.Errorf("Synthesized() = %v, want %v", got, !tc.terminal)
			}
		})
	}
}

func TestErrorExcerptIsBoundedSingleLineAndRuneSafe(t *testing.T) {
	t.Run("short payload kept verbatim", func(t *testing.T) {
		if got := errorExcerpt([]byte(`{"error":{"message":"boom"}}`)); got != `{"error":{"message":"boom"}}` {
			t.Fatalf("errorExcerpt = %q", got)
		}
	})
	t.Run("control characters cannot break the log line", func(t *testing.T) {
		got := errorExcerpt([]byte("line1\nline2\r\ttab\x00\x1f\x7fend"))
		if got != "line1 line2  tabend" {
			t.Fatalf("errorExcerpt = %q", got)
		}
	})
	t.Run("ascii boundary lands on the cap", func(t *testing.T) {
		payload := strings.Repeat("a", 300)
		if got := errorExcerpt([]byte(payload)); got != payload[:watchErrorEvidenceCap] {
			t.Fatalf("errorExcerpt length = %d, want %d", len(got), watchErrorEvidenceCap)
		}
	})
	t.Run("multibyte cut backs off to a rune boundary", func(t *testing.T) {
		// 6 ASCII bytes, then 3-byte runes: byte 200 is the third byte of a
		// rune (200-6 = 194 = 3*64+2), so the cut must back off to 198.
		payload := `{"t":"` + strings.Repeat("操作失败", 60) + `"}`
		got := errorExcerpt([]byte(payload))
		if len(got) != 198 {
			t.Fatalf("errorExcerpt length = %d, want 198 (rune boundary below the cap)", len(got))
		}
		if !utf8.ValidString(got) || !strings.HasPrefix(payload, got) {
			t.Fatalf("errorExcerpt is not a valid UTF-8 prefix of the payload: %q", got)
		}
	})
}
