package diag

import (
	cliframework "model-proxy/internal/cli/framework"
)

// RunWire is the process-level entry for `wire`: load config, then run the
// wire subcommand (record).
func RunWire(args []string) { CmdWire(args, cliframework.LoadCmdConfig(args)) }

// RunReplay is the process-level entry for `replay`: load config, then
// re-answer a logged request with the chosen backend.
func RunReplay(args []string) { CmdReplay(args, cliframework.LoadCmdConfig(args)) }

// RunShadow is the process-level entry for `shadow`: load config, then run
// the shadow subcommand (report).
func RunShadow(args []string) { CmdShadow(args, cliframework.LoadCmdConfig(args)) }
