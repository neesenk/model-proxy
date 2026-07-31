package main

import (
	"log"
	cliframework "model-proxy/internal/cli/framework"

	clicmd "model-proxy/internal/cli"
)

func cmdShadow(args []string) {
	cfg, err := LoadConfig(cliframework.ConfigPath(args))
	if err != nil {
		log.Fatal(err)
	}
	clicmd.CmdShadow(args, cfg)
}

func cmdShadowReport(args []string) {
	cfg, err := LoadConfig(cliframework.ConfigPath(args))
	if err != nil {
		log.Fatal(err)
	}
	clicmd.CmdShadowReport(args, cfg)
}
