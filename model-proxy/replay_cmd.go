package main

import (
	"log"

	clicmd "model-proxy/internal/cli"
)

func cmdReplay(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		log.Fatal(err)
	}
	clicmd.CmdReplay(args, cfg)
}
