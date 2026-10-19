// Package mcpserver owns the `mcp-server` CLI subcommand: bundled MCP tool
// servers the binary itself serves over stdio (newline-delimited JSON-RPC on
// stdin/stdout — the gateway's transport: stdio child contract). The daemon
// spawns the deployed binary with `command: [<path>/model-proxy, mcp-server,
// <name>]` and injects credentials through env: templates, so a bundled
// server needs no external runtime (npx/node) and ships with every ordinary
// binary rebuild. Each bundled server keeps its implementation in its own
// file; this file is only the process entry and dispatch.
package mcpserver

import (
	"fmt"
	"io"
)

// RunServer is the `mcp-server <name>` process entry. Unlike ProcessCommand
// adapters it keeps the injected streams: the whole point of the subcommand
// is to speak MCP stdio on them. A normal exit (stdin EOF) returns 0; setup
// and loop failures report to stderr and return 1.
func RunServer(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 1 || args[0] == "" {
		fmt.Fprintln(stderr, "usage: mcp-server <name> — bundled stdio MCP tool servers: deepseek-search")
		return 1
	}
	switch args[0] {
	case "deepseek-search":
		// DeepSeek Web Search/Fetch API (api-docs.deepseek.com /guides/search)
		// wrapped as MCP tools; credentials arrive via DEEPSEEK_API_KEY (the
		// gateway injects ${account.api_key} from the deepseek provider pool).
		if err := ServeDeepSeek(stdin, stdout); err != nil {
			fmt.Fprintf(stderr, "mcp-server deepseek-search: %v\n", err)
			return 1
		}
		return 0
	default:
		fmt.Fprintf(stderr, "mcp-server: unknown bundled server %q (available: deepseek-search)\n", args[0])
		return 1
	}
}
