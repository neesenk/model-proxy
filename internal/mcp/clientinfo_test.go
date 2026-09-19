package mcp

import "testing"

// TestParseClientInfo: the initialize clientInfo.name is the MCP-side client
// identity — extracted only from initialize-shaped bodies that actually carry
// it, with junk and oversized names rejected.
func TestParseClientInfo(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"codex", `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"codex-mcp-client","title":"Codex","version":"0.153.4"}}}`, "codex-mcp-client"},
		{"no clientInfo", `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26"}}`, ""},
		{"not initialize", `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"search"},"clientInfo":{"name":"x"}}`, ""},
		{"empty name", `{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"clientInfo":{"name":"  "}}}`, ""},
		{"garbage", `not json`, ""},
		{"needle only", `{"clientInfo":1}`, ""},
	}
	for _, tc := range cases {
		if got := ParseClientInfo([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: ParseClientInfo = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestSessionTable_SetClient: the initialize-time client identity binds onto a
// live session, survives Get copies, and unknown/expired ids are no-ops.
func TestSessionTable_SetClient(t *testing.T) {
	tbl := NewSessionTable(4, 0)
	id := tbl.Put("zs", "zhipu", "up-1")
	if _, ok := tbl.Get(id); !ok {
		t.Fatal("session not live after Put")
	}
	s, ok := tbl.Get(id)
	if ok && s.Client != "" {
		t.Fatalf("Client = %q before binding, want empty", s.Client)
	}
	tbl.SetClient(id, "codex-mcp-client")
	s, ok = tbl.Get(id)
	if !ok || s.Client != "codex-mcp-client" {
		t.Fatalf("Client after SetClient = (%q, %v), want codex-mcp-client", s.Client, ok)
	}
	// Rebinding (re-initialize with a different client) overwrites.
	tbl.SetClient(id, "other-client")
	s, _ = tbl.Get(id)
	if s.Client != "other-client" {
		t.Fatalf("Client after rebind = %q, want other-client", s.Client)
	}
	// Unknown ids and empty labels are no-ops.
	tbl.SetClient("ghost", "x")
	tbl.SetClient(id, "")
	s, _ = tbl.Get(id)
	if s.Client != "other-client" {
		t.Fatalf("empty SetClient clobbered the label: %q", s.Client)
	}
}
