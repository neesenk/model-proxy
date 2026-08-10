package main

import (
	cliserve "model-proxy/internal/cli/serve"
	"model-proxy/provider"
	"os"
	"os/exec"
)

// Daemon role/detach constants alias internal/cli/serve (the orchestration
// lives there; cli_serve.go reads these for role dispatch).
const (
	envRole        = cliserve.EnvRole
	roleSupervisor = cliserve.RoleSupervisor
	roleWorker     = cliserve.RoleWorker

	supervisorWorkerStopWait = cliserve.SupervisorWorkerStopWait
)

// aliveDaemonPid delegates to cliserve.ReadLivePid.
func aliveDaemonPid(logFile string) int {
	return cliserve.ReadLivePid(logFile)
}

func daemonize(sa cliserve.Args) error {
	return cliserve.Daemonize(daemonEnv(), sa)
}

// daemonEnv wires the production process seams for the daemon paths.
func daemonEnv() cliserve.DaemonEnv {
	return cliserve.DaemonEnv{LoadConfig: LoadConfig, Executable: os.Args[0], Stdout: os.Stdout, Stderr: os.Stderr}
}

// runSupervisor supervises the worker: spawn, wait, restart on exit with backoff.
// Exits when it receives SIGTERM/SIGINT (forwarding SIGTERM to the worker first).
func runSupervisor(sa cliserve.Args) {
	cliserve.RunSupervisor(daemonEnv(), sa)
}

// cmdStop / cmdReload / maybeReloadDaemon delegate to internal/cli/serve with
// the production process env and CLI color helpers.
func cmdStop(args []string) {
	cliserve.CmdStop(daemonEnv(), cliserve.ParseArgs(args), provider.Yellow, provider.Gray, provider.Green)
}

func cmdReload(args []string) {
	cliserve.CmdReload(daemonEnv(), cliserve.ParseArgs(args), provider.Yellow, provider.Gray, provider.Green)
}

func maybeReloadDaemon(args []string) {
	cliserve.SignalReloadDaemon(daemonEnv(), cliserve.ParseArgs(args), provider.Gray)
}

// spawnWorker delegates to cliserve.SpawnWorker with the production env.
func spawnWorker(sa cliserve.Args) *exec.Cmd {
	return cliserve.SpawnWorker(daemonEnv(), sa)
}
