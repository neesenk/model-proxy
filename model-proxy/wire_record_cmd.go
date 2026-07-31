package main

import (
	"log"

	clicmd "model-proxy/internal/cli"
)

func cmdWire(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		log.Fatal(err)
	}
	clicmd.CmdWire(args, cfg)
}

func cmdWireRecord(args []string) {
	cfg, err := LoadConfig(configPath(args))
	if err != nil {
		log.Fatal(err)
	}
	clicmd.CmdWireRecord(args, cfg)
}
