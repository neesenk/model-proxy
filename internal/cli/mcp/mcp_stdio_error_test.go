package climcp

import (
	"os"
	"strings"
	"testing"

	"model-proxy/internal/cli/clitest"
)

// stdioChildConfig builds a provider-backed stdio server config whose command
// is this test binary in fake-child mode (mode selected via the MCP_STDIO_CHILD
// env indirection; env values are indirection-only, literals are rejected).
func stdioChildConfig(provider string) string {
	return `
listen: 127.0.0.1:1
providers:
  ` + provider + `: {provider_id: ` + provider + `, openai_base_url: https://example.com/api/v1}
mcp:
  vision:
    transport: stdio
    provider: ` + provider + `
    command: ["` + os.Args[0] + `", "-test.run=TestHelperProcess"]
    env:
      MCP_STDIO_CHILD: env:MCP_STDIO_CHILD
`
}

// TestMCPTestStdioInitializeRejected: a stdio child that answers initialize
// with a response frame lacking protocolVersion must exit 1 with the
// initialize error naming the server — not panic, not hang (CLI.md §13b).
func TestMCPTestStdioInitializeRejected(t *testing.T) {
	home := t.TempDir()
	// The key indirection (${account.api_key} in sibling tests) is resolved
	// from the pool, so a pool file must exist before the child is spawned.
	clitest.SetPoolHome(t, home)
	clitest.WritePoolFile(t, "zhipu", "zhipu", "pool-key-1")
	t.Setenv("MCP_STDIO_CHILD", "no-proto")
	cfgPath := clitest.WriteTempConfig(t, stdioChildConfig("zhipu"))

	_, stderr, code := clitest.RunCLIWithHome(t, home, "mcp", cfgPath, "test", "vision")
	if code != 1 {
		t.Fatalf("exit = %d, want 1\n--- stderr ---\n%s", code, stderr)
	}
	if !strings.Contains(stderr, "initialize: no protocolVersion in response") {
		t.Errorf("stderr = %q, want the no-protocolVersion initialize error", stderr)
	}
}

// TestMCPTestStdioOAuthProviderCannotSupplyKey: a provider-backed stdio server
// must inject the account key into the child env — OAuth/SSO providers (aqp,
// codex) have no raw API key to hand out, so the probe must exit 1 with the
// apikey-only guidance BEFORE spawning any child.
func TestMCPTestStdioOAuthProviderCannotSupplyKey(t *testing.T) {
	t.Setenv("MCP_STDIO_CHILD", "1")
	cfgPath := clitest.WriteTempConfig(t, `
listen: 127.0.0.1:1
providers:
  aqp:
    openai_base_url: https://example.invalid/compass-api/v1
    provider_id: aqp
    aqp_mint_url: https://example.invalid/api/v1/cqp/ccswitch/api_key/get_or_generate
mcp:
  vision:
    transport: stdio
    provider: aqp
    command: ["true"]
    env:
      MCP_STDIO_CHILD: env:MCP_STDIO_CHILD
`)

	_, stderr, code := clitest.RunCLI(t, "mcp", cfgPath, "test", "vision")
	if code != 1 {
		t.Fatalf("exit = %d, want 1\n--- stderr ---\n%s", code, stderr)
	}
	if !strings.Contains(stderr, `provider "aqp" cannot supply a raw API key (apikey providers only)`) {
		t.Errorf("stderr = %q, want the apikey-only guidance", stderr)
	}
}
