package main

import (
	"log"
	cliframework "model-proxy/internal/cli/framework"
	"os"

	clicmd "model-proxy/internal/cli"
)

// cmdStats is the process-level wrapper: config loading and --config parsing
// stay here; rendering and daemon queries live in internal/cli.
func cmdStats(args []string) {
	cfg, err := LoadConfig(cliframework.ConfigPath(args))
	if err != nil {
		log.Fatal(err)
	}
	os.Exit(clicmd.CmdStats(args, cfg.Listen, os.Stdout, os.Stderr))
}
