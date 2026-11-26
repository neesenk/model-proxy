package mcpserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	mcpkg "model-proxy/internal/mcp"
)

// r6_frame_bound_test.go covers the write-side guard itself (R6 F9): fitting
// lines pass untouched, an oversized tool result keeps the longest prefix that
// fits, and a line that cannot be cut degrades to a compact error.

// TestR6BoundFrameLeavesFittingLinesAlone: a line under the budget is
// returned byte-for-byte (no re-render, no marker) — the common path.
func TestR6BoundFrameLeavesFittingLinesAlone(t *testing.T) {
	for _, resp := range [][]byte{
		mcpkg.BuildResultResponse(json.RawMessage(`5`), toolText(strings.Repeat("a", 1000))),
		mcpkg.BuildErrorResponse(json.RawMessage(`5`), -32602, "invalid params: count must be an integer 1-50"),
	} {
		if got := boundFrame(resp); !bytes.Equal(got, resp) {
			t.Fatalf("fitting line rewritten:\n got %s\nwant %s", got, resp)
		}
	}
}

// TestR6TruncateToolResultKeepsMaximalPrefix: the cut keeps the longest
// prefix that fits the limit for every text shape the tools emit.
func TestR6TruncateToolResultKeepsMaximalPrefix(t *testing.T) {
	const limit = 300
	id := json.RawMessage(`5`)
	for _, tc := range []struct {
		name    string
		text    string
		isError bool
	}{
		{"plain ascii", strings.Repeat("a", 1000), false},
		{"escape-heavy (< & \" expand 6x)", strings.Repeat(`<&"`, 200), false},
		{"control characters (\\n expands 2x)", strings.Repeat("line\n", 200), false},
		{"multi-byte runes", strings.Repeat("中文", 300), false},
		{"upstream error result keeps isError", strings.Repeat("z", 1000), true},
	} {
		line := truncateToolResult(id, []textBlock{{Type: "text", Text: tc.text}}, tc.isError, limit)
		if len(line) > limit {
			t.Fatalf("%s: truncated line is %d bytes, over the %d-byte limit", tc.name, len(line), limit)
		}
		if len(line) < limit-64 {
			t.Fatalf("%s: truncated line is %d bytes of a %d-byte budget (cut more than needed)", tc.name, len(line), limit)
		}
		var resp struct {
			ID     json.RawMessage `json:"id"`
			Result struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
				IsError bool `json:"isError"`
			} `json:"result"`
		}
		if err := json.Unmarshal(line, &resp); err != nil {
			t.Fatalf("%s: truncated line is not valid JSON: %v", tc.name, err)
		}
		if string(resp.ID) != "5" || len(resp.Result.Content) != 1 || resp.Result.IsError != tc.isError {
			t.Fatalf("%s: truncated result = id %s, %d blocks, isError %v", tc.name, resp.ID, len(resp.Result.Content), resp.Result.IsError)
		}
		text := resp.Result.Content[0].Text
		at := strings.Index(text, r6MarkerPrefix)
		if at < 0 {
			t.Fatalf("%s: no truncation marker in %q", tc.name, text)
		}
		kept := text[:at]
		var omitted int
		if _, err := fmt.Sscanf(text[at:], r6MarkerPrefix+"%d bytes omitted", &omitted); err != nil {
			t.Fatalf("%s: marker %q: %v", tc.name, text[at:], err)
		}
		if !strings.HasPrefix(tc.text, kept) || len(kept)+omitted != len(tc.text) {
			t.Fatalf("%s: kept %d + omitted %d do not reconstruct the %d-byte text", tc.name, len(kept), omitted, len(tc.text))
		}
		if !utf8.ValidString(kept) || strings.ContainsRune(kept, utf8.RuneError) {
			t.Fatalf("%s: truncation split a multi-byte rune: %q", tc.name, kept[max(0, len(kept)-12):])
		}
	}
}

// TestR6BoundFrameDropsUnshrinkableResponse: a line that cannot be cut (not a
// text tool result, or text blocks that stay over the limit when emptied)
// degrades to a compact JSON-RPC error carrying the request id — still valid
// and bounded, never the oversized line.
func TestR6BoundFrameDropsUnshrinkableResponse(t *testing.T) {
	id := json.RawMessage(`5`)
	dropped := boundFrame(mcpkg.BuildErrorResponse(id, -32602, strings.Repeat("m", mcpkg.MaxFrameBytes+1)))
	if len(dropped)+1 > deepseekMaxLineBytes {
		t.Fatalf("replacement for an oversized line is itself %d bytes", len(dropped))
	}
	var resp rpcResponse
	if err := json.Unmarshal(dropped, &resp); err != nil {
		t.Fatalf("replacement is not valid JSON: %v", err)
	}
	if string(resp.ID) != "5" || resp.Error == nil || resp.Error.Code != -32000 || !strings.Contains(resp.Error.Message, "frame limit") {
		t.Fatalf("replacement = %s, want a -32000 error for id 5 naming the frame limit", dropped)
	}

	// Multi-block result whose earlier blocks alone exceed the limit.
	line := truncateToolResult(id, []textBlock{{Type: "text", Text: strings.Repeat("a", 400)}, {Type: "text", Text: "tail"}}, false, 300)
	if !bytes.Equal(line, dropped) {
		t.Fatalf("unshrinkable multi-block result = %s, want the compact error %s", line, dropped)
	}
	// A block that is not text cannot be cut either.
	withImage := mcpkg.BuildResultResponse(id, json.RawMessage(`{"content":[{"type":"image","data":"`+strings.Repeat("A", mcpkg.MaxFrameBytes+1)+`","mimeType":"image/png"}]}`))
	if got := boundFrame(withImage); !bytes.Equal(got, dropped) {
		t.Fatalf("oversized non-text result = %q, want the compact error", got)
	}
}
