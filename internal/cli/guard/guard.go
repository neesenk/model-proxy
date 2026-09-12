// Package guard owns the `guard` command: the daemon-facing viewer for the
// guard AI second-opinion channel (guard.adjudicate) — the persisted
// session-block table and its manual unblock. Counterpart surfaces: the
// WebUI Security page and GET/DELETE /api/security/blocks.
package guard

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"model-proxy/internal/appapi"
	cliframework "model-proxy/internal/cli/framework"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/daemonctl"
	"model-proxy/internal/display"
)

// RunGuard is the process-level entry for `guard`: load config, then render
// the subcommand result (exit code becomes the exit status).
func RunGuard(args []string) {
	cfg := cliframework.LoadCmdConfig(args)
	os.Exit(CmdGuard(args, cfg, os.Stdout, os.Stderr))
}

// CmdGuard implements `model-proxy guard blocks|unblock` against the running
// daemon's admin API (same surface as the WebUI Security page). Extracted
// from RunGuard for tests.
func CmdGuard(args []string, cfg *configdomain.Config, stdout, stderr io.Writer) int {
	pos := cliframework.PositionalArgs(args)
	jsonOut := hasFlag(args, "--json")
	base := "http://" + cfg.Listen
	if len(pos) == 0 {
		fmt.Fprintln(stderr, "usage: model-proxy guard <blocks|unblock> [args]")
		fmt.Fprintln(stderr, "  guard blocks                 list adjudicated-blocked sessions")
		fmt.Fprintln(stderr, "  guard unblock <session-id>   re-admit a blocked session")
		return 1
	}
	switch pos[0] {
	case "blocks":
		return renderBlocks(base, jsonOut, stdout, stderr)
	case "unblock":
		if len(pos) < 2 || pos[1] == "" {
			fmt.Fprintf(stderr, "%s usage: model-proxy guard unblock <session-id>\n", display.Red("✗"))
			return 1
		}
		return runUnblock(base, pos[1], stdout, stderr)
	default:
		fmt.Fprintf(stderr, "%s unknown guard subcommand %q — use blocks or unblock\n", display.Red("✗"), pos[0])
		return 1
	}
}

// fetchBlocks gets /api/security/blocks (daemon-owned order: newest first).
func fetchBlocks(base string) ([]appapi.SecurityBlock, error) {
	rb, status, err := daemonctl.Get(base, "/api/security/blocks")
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%s", display.Truncate(strings.TrimSpace(string(rb)), 200))
	}
	var out struct {
		Blocks []appapi.SecurityBlock `json:"blocks"`
	}
	if err := json.Unmarshal(rb, &out); err != nil {
		return nil, err
	}
	return out.Blocks, nil
}

func renderBlocks(base string, jsonOut bool, stdout, stderr io.Writer) int {
	blocks, err := fetchBlocks(base)
	if err != nil {
		fmt.Fprintf(stderr, "%s %v\n", display.Red("✗"), err)
		return 1
	}
	if jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(blocks)
		return 0
	}
	if len(blocks) == 0 {
		fmt.Fprintf(stdout, "%s no adjudicated-blocked sessions\n", display.Dim("•"))
		return 0
	}
	// The daemon returns newest first (block table snapshot order).
	fmt.Fprintf(stdout, "%s %d blocked session(s) (guard.adjudicate high verdicts)\n\n", display.Bold("guard"), len(blocks))
	for _, b := range blocks {
		ts := time.UnixMilli(b.Ts).Format("2006-01-02 15:04:05")
		fmt.Fprintf(stdout, "  %s\n", display.Bold(b.SessionID))
		fmt.Fprintf(stdout, "    rule=%s kind=%s ts=%s\n", b.Rule, b.Kind, ts)
		if b.Reason != "" {
			fmt.Fprintf(stdout, "    reason: %s\n", b.Reason)
		}
		if b.RequestID != "" {
			fmt.Fprintf(stdout, "    request=%s model=%s\n", b.RequestID, b.Model)
		}
		fmt.Fprintf(stdout, "    unblock: model-proxy guard unblock %s\n\n", b.SessionID)
	}
	return 0
}

func runUnblock(base, sessionID string, stdout, stderr io.Writer) int {
	rb, status, err := daemonctl.Do(http.MethodDelete, base, "/api/security/blocks/"+url.PathEscape(sessionID))
	if err != nil {
		fmt.Fprintf(stderr, "%s %v\n", display.Red("✗"), err)
		return 1
	}
	if status != http.StatusOK {
		fmt.Fprintf(stderr, "%s %s\n", display.Red("✗"), display.Truncate(strings.TrimSpace(string(rb)), 200))
		return 1
	}
	fmt.Fprintf(stdout, "%s unblocked session %s — requests are admitted again\n", display.Green("✓"), sessionID)
	return 0
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}
