package main

import (
	"os"

	cliserve "model-proxy/internal/cli/serve"
	configdomain "model-proxy/internal/config"
)

// daemonEnv wires the production process seams for the daemon paths.
func daemonEnv() cliserve.DaemonEnv {
	return cliserve.DaemonEnv{LoadConfig: configdomain.LoadConfig, Executable: os.Args[0], Stdout: os.Stdout, Stderr: os.Stderr}
}
