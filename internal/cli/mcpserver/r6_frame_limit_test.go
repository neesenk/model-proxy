package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	mcpkg "model-proxy/internal/mcp"
	"model-proxy/internal/testsyn"
)

// r6_frame_limit_test.go pins the child side of the stdio frame contract
// (R6 F9): every line the bundled server writes stays below the gateway's
// frame bound. An oversized tool result degrades to truncated content with a
// recognizable marker instead of a line the gateway's read loop kills the
// whole session on.

// r6MarkerPrefix is the recognizable start of the truncation notice.
const r6MarkerPrefix = "\n\n[truncated: "

const (
	r6InitLine   = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
	r6FetchLine  = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"url_fetch","arguments":{"url":"https://example.com/big"}}}`
	r6SearchLine = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"big","need_content":true}}}`
	r6PingLine   = `{"jsonrpc":"2.0","id":3,"method":"ping"}`
)

// r6FrameAPI is a fake DeepSeek API whose fetch page AND search result carry
// the given content (the credential contract is asserted per request).
func r6FrameAPI(t *testing.T, content string) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q, want Bearer test-key", got)
		}
		var payload any
		switch r.URL.Path {
		case "/v1/tools/fetch":
			payload = map[string]any{"title": "Big page", "content": content}
		case "/v1/tools/search":
			payload = map[string]any{"results": []map[string]any{{"title": "Big hit", "url": "https://example.com/big", "content": content}}}
		default:
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// r6ServeInProcess drives ServeDeepSeek over the stdin lines and returns the
// response lines (newline excluded), paired with the requests in order.
func r6ServeInProcess(t *testing.T, api *httptest.Server, lines ...string) [][]byte {
	t.Helper()
	t.Setenv(deepseekAPIKeyEnv, "test-key")
	t.Setenv(deepseekBaseURLEnv, api.URL)
	var out bytes.Buffer
	if err := ServeDeepSeek(strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatalf("ServeDeepSeek: %v", err)
	}
	return bytes.Split(bytes.TrimSuffix(out.Bytes(), []byte("\n")), []byte("\n"))
}

// r6RequireTruncated asserts that line is the bounded, valid, truncated form
// of a tools/call result whose complete text would have been full: the kept
// text is a prefix of full and the marker names exactly the rest.
func r6RequireTruncated(t *testing.T, line []byte, full string) {
	t.Helper()
	if len(line)+1 > mcpkg.MaxFrameBytes {
		t.Fatalf("response line is %d bytes (+newline): the gateway drops lines at %d and kills the session", len(line), mcpkg.MaxFrameBytes)
	}
	if len(line)+1 > deepseekMaxLineBytes {
		t.Fatalf("response line is %d bytes (+newline), over the child's own %d-byte line budget", len(line), deepseekMaxLineBytes)
	}
	if len(line) < deepseekMaxLineBytes-1<<10 {
		t.Fatalf("truncated line uses only %d of its %d-byte budget (cut far more than needed)", len(line), deepseekMaxLineBytes)
	}
	var resp rpcResponse
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("truncated line is not valid JSON: %v", err)
	}
	if string(resp.ID) != "2" || resp.Error != nil {
		t.Fatalf("truncated answer = id %s error %+v, want the result for id 2", resp.ID, resp.Error)
	}
	text := callText(t, resp)
	at := strings.Index(text, r6MarkerPrefix)
	if at < 0 {
		t.Fatalf("truncated result carries no marker; text ends %q", text[max(0, len(text)-80):])
	}
	kept, marker := text[:at], text[at:]
	var omitted int
	if _, err := fmt.Sscanf(marker, r6MarkerPrefix+"%d bytes omitted", &omitted); err != nil {
		t.Fatalf("marker %q does not name the omitted byte count: %v", marker, err)
	}
	if !strings.HasPrefix(full, kept) {
		t.Fatal("kept text is not a prefix of the complete result")
	}
	if len(kept)+omitted != len(full) || omitted == 0 {
		t.Fatalf("marker reports %d omitted bytes with %d kept, want them to add up to the %d-byte result", omitted, len(kept), len(full))
	}
	if !utf8.ValidString(kept) || strings.ContainsRune(kept, utf8.RuneError) {
		t.Fatal("truncation split a multi-byte rune")
	}
}

// TestR6OversizedFetchIsTruncatedToFrame: a url_fetch page larger than the
// gateway frame bound (and under the API read bound) is answered with its
// maximal fitting prefix plus the truncation marker — valid JSON, below the
// bound — through the real render/write path.
func TestR6OversizedFetchIsTruncatedToFrame(t *testing.T) {
	content := strings.Repeat("x", mcpkg.MaxFrameBytes+1<<20)
	lines := r6ServeInProcess(t, r6FrameAPI(t, content), r6InitLine, r6FetchLine)
	if len(lines) != 2 {
		t.Fatalf("%d response lines, want one per request", len(lines))
	}
	r6RequireTruncated(t, lines[1], "# Big page\n\n"+content)
}

// TestR6EscapeHeavyFetchIsTruncatedToFrame: the cut follows the ENCODED size.
// This page is only 3 MiB raw — well under the frame bound — but every byte
// is `<`, which JSON expands sixfold (`\u003c`), so the line is 18 MiB: a
// clamp on raw length would let it through and kill the session.
func TestR6EscapeHeavyFetchIsTruncatedToFrame(t *testing.T) {
	content := strings.Repeat("<", 3<<20)
	if encoded, _ := json.Marshal(content); len(encoded) <= mcpkg.MaxFrameBytes {
		t.Fatalf("fixture encodes to %d bytes: not over the frame bound", len(encoded))
	}
	if len(content) >= mcpkg.MaxFrameBytes {
		t.Fatalf("fixture is %d bytes raw: a raw-length clamp would truncate it anyway", len(content))
	}
	lines := r6ServeInProcess(t, r6FrameAPI(t, content), r6InitLine, r6FetchLine)
	r6RequireTruncated(t, lines[1], "# Big page\n\n"+content)
}

// TestR6OversizedSearchIsTruncatedToFrame: the guard sits at the write
// convergence point, so web_search results (need_content) take the same cut.
func TestR6OversizedSearchIsTruncatedToFrame(t *testing.T) {
	content := strings.Repeat("y", mcpkg.MaxFrameBytes+1<<20)
	lines := r6ServeInProcess(t, r6FrameAPI(t, content), r6InitLine, r6SearchLine)
	full := renderSearchResults("big", []searchResult{{Title: "Big hit", URL: "https://example.com/big", Content: content}})
	r6RequireTruncated(t, lines[1], full)
}

// TestR6DeepSeekChildOversizedFrameKeepsSessionAlive is the finding end to
// end: the bundled server as a real stdio child of the gateway's StdioConn
// answers an oversized url_fetch with a bounded line, so the parent neither
// hits ErrTooLong nor kills the child — the session keeps serving.
func TestR6DeepSeekChildOversizedFrameKeepsSessionAlive(t *testing.T) {
	content := strings.Repeat("x", mcpkg.MaxFrameBytes+1<<20)
	// The conn owns a read-loop goroutine and the API its server goroutines:
	// both must be gone once their Close ran (cleanups fire LIFO — the conn's
	// Close, then the API server's, then this check).
	baseline := testsyn.Goroutines()
	t.Cleanup(func() { testsyn.LeakCheck(t, baseline, "oversized-frame child conn") })
	api := r6FrameAPI(t, content)
	conn, err := mcpkg.StartStdio(
		[]string{os.Args[0], "-test.run=^TestDeepSeekChildProc$"},
		[]string{
			"MP_DEEPSEEK_CHILD=1",
			deepseekAPIKeyEnv + "=test-key",
			deepseekBaseURLEnv + "=" + api.URL,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if init, err := conn.Call(ctx, []byte(initializeLine("1", "2025-03-26"))); err != nil || !strings.Contains(string(init), "model-proxy-deepseek-search") {
		t.Fatalf("initialize through the gateway conn = %q, %v", init, err)
	}
	resp, err := conn.Call(ctx, []byte(r6FetchLine))
	if err != nil {
		t.Fatalf("url_fetch of an oversized page = %v (the gateway killed the child on an oversized frame)", err)
	}
	r6RequireTruncated(t, resp, "# Big page\n\n"+content)
	if ping, err := conn.Call(ctx, []byte(r6PingLine)); err != nil || !strings.Contains(string(ping), `"id":3`) {
		t.Fatalf("session did not survive the oversized page: ping = %q, %v", ping, err)
	}
}
