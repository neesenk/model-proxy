package main

import (
	"log"

	clicmd "model-proxy/internal/cli"
)

func cmdConfig(args []string) {
	// config init writes the template without loading config; print/check need
	// a loaded config.
	if len(args) > 0 && args[0] == "init" {
		clicmd.CmdConfig(args, nil)
		return
	}
	cfg, err := LoadConfig(configPath(args[1:]))
	if err != nil {
		log.Fatal(err)
	}
	clicmd.CmdConfig(args, cfg)
}
