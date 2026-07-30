package main

import (
	"log"

	clicmd "model-proxy/internal/cli"
)

// cmdLogout is the process-level wrapper: config loading stays here; logout
// flow lives in internal/cli.
func cmdLogout(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		log.Fatal(err)
	}
	clicmd.CmdLogout(args, cfg)
}
