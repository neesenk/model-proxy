package main

import (
	"log"

	clicmd "model-proxy/internal/cli"
)

func cmdShadow(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		log.Fatal(err)
	}
	clicmd.CmdShadow(args, cfg)
}

func cmdShadowReport(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		log.Fatal(err)
	}
	clicmd.CmdShadowReport(args, cfg)
}
