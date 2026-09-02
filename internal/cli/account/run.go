package account

import (
	cliframework "model-proxy/internal/cli/framework"
)

// RunLogout is the process-level entry for `logout`: load config, then run.
func RunLogout(args []string) { CmdLogout(args, cliframework.LoadCmdConfig(args)) }
