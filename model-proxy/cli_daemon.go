package main

import (
	cliserve "model-proxy/internal/cli/serve"
	"model-proxy/provider"
	"os"
)

// Daemon role/detach constants alias internal/cli/serve (the orchestration
// lives there; cli_serve.go reads these for role dispatch).
const (
	envRole        = cliserve.EnvRole
	roleSupervisor = cliserve.RoleSupervisor
	roleWorker     = cliserve.RoleWorker

	supervisorWorkerStopWait = cliserve.SupervisorWorkerStopWait
)

// daemonEnv wires the production process seams for the daemon paths.
func daemonEnv() cliserve.DaemonEnv {
	return cliserve.DaemonEnv{LoadConfig: LoadConfig, Executable: os.Args[0], Stdout: os.Stdout, Stderr: os.Stderr}
}

// cmdStop / cmdReload / maybeReloadDaemon delegate to internal/cli/serve with
// the production process env and CLI color helpers.
func cmdStop(args []string) {
	cliserve.CmdStop(daemonEnv(), cliserve.ParseArgs(args), provider.Yellow, provider.Gray, provider.Green)
}

func cmdReload(args []string) {
	cliserve.CmdReload(daemonEnv(), cliserve.ParseArgs(args), provider.Yellow, provider.Gray, provider.Green)
}
