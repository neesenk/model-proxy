package main

import (
	"log"
	cliframework "model-proxy/internal/cli/framework"

	climodels "model-proxy/internal/cli/models"
)

// cmdModels is the process-level wrapper: config loading stays here; listing,
// fetch and refresh live in internal/cli/models.
func cmdModels(args []string) {
	cfg, err := LoadConfig(cliframework.ConfigPath(args))
	if err != nil {
		log.Fatal(err)
	}
	climodels.CmdModels(args, cfg, cliframework.ConfigPath(args))
}
