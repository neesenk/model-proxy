package main

import (
	"log"
	cliframework "model-proxy/internal/cli/framework"

	clicmd "model-proxy/internal/cli"
)

// cmdServeStatusCLI loads config then delegates to internal/cli.
func cmdServeStatusCLI(args []string) {
	cfg, err := LoadConfig(cliframework.ConfigPath(args))
	if err != nil {
		log.Fatal(err)
	}
	clicmd.CmdServeStatus(args, cfg)
}
