package models

import (
	cliframework "model-proxy/internal/cli/framework"
)

// RunModels is the process-level entry for `models`: load config, then run
// the models command (list/refresh/pull).
func RunModels(args []string) {
	cfg := cliframework.LoadCmdConfig(args)
	CmdModels(args, cfg, cliframework.ConfigPath(args))
}
