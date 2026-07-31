package main

import (
	"log"
	cliframework "model-proxy/internal/cli/framework"

	clicmd "model-proxy/internal/cli"
)

func cmdPin(args []string) {
	cfg, err := LoadConfig(cliframework.ConfigPath(args))
	if err != nil {
		log.Fatal(err)
	}
	clicmd.CmdPin(args, cfg)
}

func cmdUnpin(args []string) {
	cfg, err := LoadConfig(cliframework.ConfigPath(args))
	if err != nil {
		log.Fatal(err)
	}
	clicmd.CmdUnpin(args, cfg)
}
