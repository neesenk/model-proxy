package stats

import (
	"os"

	cliframework "model-proxy/internal/cli/framework"
)

// RunStats is the process-level entry for `stats`: load config, then render
// the daemon statistics report (exit code becomes the exit status).
func RunStats(args []string) {
	cfg := cliframework.LoadCmdConfig(args)
	os.Exit(CmdStats(args, cfg.Listen, os.Stdout, os.Stderr))
}

// RunUsage is the process-level entry for `usage`: load config, then print
// per-provider quota/credit usage.
func RunUsage(args []string) { CmdUsage(args, cliframework.LoadCmdConfig(args)) }
