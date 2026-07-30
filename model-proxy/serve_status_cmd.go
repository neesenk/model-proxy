package main

import (
	"log"

	clicmd "model-proxy/internal/cli"
)

// cmdServeStatusCLI loads config then delegates to internal/cli.
func cmdServeStatusCLI(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		log.Fatal(err)
	}
	clicmd.CmdServeStatus(args, cfg)
}
