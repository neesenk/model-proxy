package main

import (
	"log"
	cliframework "model-proxy/internal/cli/framework"

	clicmd "model-proxy/internal/cli"
)

// cmdUsage is the process-level wrapper: config loading stays here; usage
// rendering lives in internal/cli.
func cmdUsage(args []string) {
	cfg, err := LoadConfig(cliframework.ConfigPath(args))
	if err != nil {
		log.Fatal(err)
	}
	clicmd.CmdUsage(args, cfg)
}
