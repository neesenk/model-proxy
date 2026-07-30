package main

import (
	"log"

	clicmd "model-proxy/internal/cli"
)

func cmdPin(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		log.Fatal(err)
	}
	clicmd.CmdPin(args, cfg)
}

func cmdUnpin(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		log.Fatal(err)
	}
	clicmd.CmdUnpin(args, cfg)
}
