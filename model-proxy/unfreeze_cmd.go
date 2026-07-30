package main

import (
	"log"

	clicmd "model-proxy/internal/cli"
)

func cmdUnfreeze(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		log.Fatal(err)
	}
	clicmd.CmdUnfreeze(args, cfg)
}
