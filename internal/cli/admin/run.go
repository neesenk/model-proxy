package admin

import (
	cliframework "model-proxy/internal/cli/framework"
)

// RunPin is the process-level entry for `pin`: load config, then POST the
// route pin to the running daemon.
func RunPin(args []string) { CmdPin(args, cliframework.LoadCmdConfig(args)) }

// RunUnpin is the process-level entry for `unpin`: load config, then DELETE
// the route pin on the running daemon.
func RunUnpin(args []string) { CmdUnpin(args, cliframework.LoadCmdConfig(args)) }

// RunUnfreeze is the process-level entry for `unfreeze`: load config, then
// POST the health reset to the running daemon.
func RunUnfreeze(args []string) { CmdUnfreeze(args, cliframework.LoadCmdConfig(args)) }
