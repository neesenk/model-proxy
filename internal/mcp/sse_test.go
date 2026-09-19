package mcp

import (
	"io"
	"strings"
	"testing"
)

func TestResolveEndpointURL(t *testing.T) {
	cases := []struct{ base, data, want string }{
		{"https://h/api/mcp/x/sse", "/api/mcp/x/mcp?sessionId=1", "https://h/api/mcp/x/mcp?sessionId=1"},
		{"https://h/api/mcp/x/sse", "https://other/mcp?sessionId=2", "https://other/mcp?sessionId=2"},
		{"https://h/api/mcp/x/sse", "mcp?sessionId=3", "https://h/api/mcp/x/mcp?sessionId=3"},
		{"https://h/api/mcp/x/sse", "  /p?q=4  ", "https://h/p?q=4"},
	}
	for _, c := range cases {
		if got := ResolveEndpointURL(c.base, c.data); got != c.want {
			t.Errorf("ResolveEndpointURL(%q,%q) = %q, want %q", c.base, c.data, got, c.want)
		}
	}
}

func TestEndpointRewriter(t *testing.T) {
	stream := "event: endpoint\ndata: /messages?sessionId=xyz\n\nevent: message\ndata: {\"a\":1}\n\n"
	rewrote := ""
	r := NewEndpointRewriter(strings.NewReader(stream), func(data string) string {
		rewrote = data
		return "/mcp/zs?mps=local-1"
	})
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if rewrote != "/messages?sessionId=xyz" {
		t.Fatalf("rewrite input = %q", rewrote)
	}
	if !strings.Contains(string(out), "data: /mcp/zs?mps=local-1\n") {
		t.Fatalf("out = %q", out)
	}
	if !strings.Contains(string(out), `data: {"a":1}`) {
		t.Fatalf("message event damaged: %q", out)
	}
	// No endpoint event → passthrough, rewrite never called.
	r2 := NewEndpointRewriter(strings.NewReader("data: {\"x\":2}\n\n"), func(string) string {
		t.Fatal("rewrite called without endpoint event")
		return ""
	})
	out2, _ := io.ReadAll(r2)
	if string(out2) != "data: {\"x\":2}\n\n" {
		t.Fatalf("passthrough = %q", out2)
	}
	// Chained events: only the first endpoint is rewritten.
	stream3 := "event: endpoint\ndata: /a?s=1\n\nevent: endpoint\ndata: /a?s=2\n\n"
	calls := 0
	r3 := NewEndpointRewriter(strings.NewReader(stream3), func(string) string { calls++; return "/x" })
	out3, _ := io.ReadAll(r3)
	if calls != 1 || strings.Count(string(out3), "/x") != 1 || !strings.Contains(string(out3), "/a?s=2") {
		t.Fatalf("multi-endpoint: calls=%d out=%q", calls, out3)
	}
}

// TestEndpointRewriter_PreservesDataWhitespace: the whitespace after
// "data:" (the SSE field separator) and the original line terminator must
// survive the rewrite instead of being erased by a whole-line TrimSpace.
func TestEndpointRewriter_PreservesDataWhitespace(t *testing.T) {
	stream := "event: endpoint\r\ndata:  /messages?sessionId=xyz\r\nevent: message\r\ndata:  {\"a\":1}\r\n"
	rewrote := ""
	r := NewEndpointRewriter(strings.NewReader(stream), func(data string) string {
		rewrote = data
		return "/mcp/zs?mps=local-1"
	})
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if rewrote != "/messages?sessionId=xyz" {
		t.Fatalf("rewrite input = %q", rewrote)
	}
	want := "data:  /mcp/zs?mps=local-1\r\n"
	if !strings.Contains(string(out), want) {
		t.Fatalf("out missing preserved whitespace: %q", out)
	}
	if strings.Contains(string(out), "data: /mcp/zs") {
		t.Fatalf("leading spaces collapsed: %q", out)
	}
	if !strings.Contains(string(out), "data:  {\"a\":1}\r\n") {
		t.Fatalf("message event damaged: %q", out)
	}
}
