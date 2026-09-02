package audit

import (
	"os"

	cliframework "model-proxy/internal/cli/framework"
)

// RunAudit is the process-level entry for `audit`: load config, then render
// the security audit log view (exit code becomes the exit status).
func RunAudit(args []string) {
	cfg := cliframework.LoadCmdConfig(args)
	os.Exit(CmdAudit(args, cfg, os.Stdout, os.Stderr))
}
