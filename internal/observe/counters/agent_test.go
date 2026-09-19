package counters

import "testing"

// TestAgentFromMCPClient: the MCP initialize clientInfo.name maps onto the
// same closed label set DetectAgent produces (both surfaces agree on "codex"),
// unknown names keep a sanitized product label, and empty stays empty.
func TestAgentFromMCPClient(t *testing.T) {
	cases := []struct{ in, want string }{
		{"codex-mcp-client", "codex"},
		{"Codex", "codex"},
		{"claude-code", "claude-code"},
		{"claude-code/1.0", "claude-code"},
		{"opencode", "opencode"},
		{"pi", "pi"},
		{"pi-mcp", "pi"},
		{"my-custom-tool", "my-custom-tool"},
		{"Weird Name (v2)", "weird"},
		{"", ""},
		{"   ", ""},
	}
	for _, tc := range cases {
		if got := AgentFromMCPClient(tc.in); got != tc.want {
			t.Errorf("AgentFromMCPClient(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
