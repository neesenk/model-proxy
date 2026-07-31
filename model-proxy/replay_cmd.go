package main

import (
	"log"
	cliframework "model-proxy/internal/cli/framework"

	clicmd "model-proxy/internal/cli"
)

func cmdReplay(args []string) {
	cfg, err := LoadConfig(cliframework.ConfigPath(args))
	if err != nil {
		log.Fatal(err)
	}
	clicmd.CmdReplay(args, cfg)
}
