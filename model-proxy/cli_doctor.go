package main

import (
	"fmt"
	cliframework "model-proxy/internal/cli/framework"
	"os"

	clidoctor "model-proxy/internal/cli/doctor"
)

// cmdDoctor is the process-level wrapper: config loading stays here; offline
// and --live diagnostics live in internal/cli/doctor.
func cmdDoctor(args []string) {
	cfg, err := LoadConfig(cliframework.ConfigPath(args))
	if err != nil {
		fmt.Println("✗ config invalid: " + err.Error())
		os.Exit(1)
	}
	clidoctor.CmdDoctor(args, cfg, cliframework.ConfigPath(args))
}
