package main

import (
	"fmt"
	"log"
	cliserve "model-proxy/internal/cli/serve"
	"model-proxy/provider"
	"os"
	"os/exec"
	"syscall"
	"time"
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

// cmdStop stops a running `serve --daemon` by reading the pid file (derived from
// the same log path the supervisor used) and sending SIGTERM. The supervisor
// forwards SIGTERM to its worker, waits for it, removes the pid file, and exits.
// Waits up to 15s for the process to disappear; falls back to SIGKILL.
func cmdStop(args []string) {
	sa := cliserve.ParseArgs(args)
	cfg, err := LoadConfig(sa.Config)
	if err != nil {
		log.Fatal(err)
	}
	logFile := cliserve.ResolveLogFile(sa, cfg)
	pidPath := cliserve.PidFilePath(logFile)

	pidStr, err := os.ReadFile(pidPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println(provider.Yellow("No daemon running.") + " (pid file not found: " + provider.Gray(pidPath) + ")")
			return
		}
		log.Fatal(err)
	}
	var pid int
	for _, c := range pidStr {
		if c < '0' || c > '9' {
			break
		}
		pid = pid*10 + int(c-'0')
	}
	if pid <= 0 {
		log.Fatalf("invalid pid in %s: %q", pidPath, string(pidStr))
	}

	// Check the process exists and is signalable.
	proc, err := os.FindProcess(pid)
	if err != nil {
		log.Fatalf("find process %d: %v", pid, err)
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		// Process is gone — clean up the stale pid file.
		os.Remove(pidPath)
		fmt.Println(provider.Yellow("Daemon not running.") + " (removed stale pid file " + provider.Gray(pidPath) + ")")
		return
	}

	fmt.Printf("Stopping model-proxy daemon (pid=%d)...\n", pid)
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		log.Fatalf("send SIGTERM to %d: %v", pid, err)
	}

	// Wait for the process to exit (it removes its own pid file on graceful exit).
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			// Gone.
			fmt.Println(provider.Green("✓ Stopped."))
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Didn't exit gracefully — force kill.
	fmt.Fprintf(os.Stderr, "graceful stop timed out, sending SIGKILL to %d\n", pid)
	_ = proc.Kill()
	os.Remove(pidPath)
	fmt.Println(provider.Green("✓ Killed."))
}

// cmdReload sends SIGHUP to a running daemon's supervisor, which forwards it
// to the worker for hot config reload.
func cmdReload(args []string) {
	sa := cliserve.ParseArgs(args)
	cfg, err := LoadConfig(sa.Config)
	if err != nil {
		log.Fatal(err)
	}
	logFile := cliserve.ResolveLogFile(sa, cfg)
	pidPath := cliserve.PidFilePath(logFile)

	pidStr, err := os.ReadFile(pidPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println(provider.Yellow("No daemon running.") + " (pid file not found: " + provider.Gray(pidPath) + ")")
			return
		}
		log.Fatal(err)
	}
	var pid int
	for _, c := range pidStr {
		if c < '0' || c > '9' {
			break
		}
		pid = pid*10 + int(c-'0')
	}
	if pid <= 0 {
		log.Fatalf("invalid pid in %s: %q", pidPath, string(pidStr))
	}
	proc, err := os.FindProcess(pid)
	if err != nil || proc.Signal(syscall.Signal(0)) != nil {
		os.Remove(pidPath)
		fmt.Println(provider.Yellow("Daemon not running.") + " (removed stale pid file)")
		return
	}
	fmt.Printf("Reloading model-proxy daemon (pid=%d)...\n", pid)
	if err := proc.Signal(syscall.SIGHUP); err != nil {
		log.Fatalf("send SIGHUP to %d: %v", pid, err)
	}
	fmt.Println(provider.Green("✓ Reload signal sent.") + " Check logs for [reload] lines.")
}

// maybeReloadDaemon sends SIGHUP to a running daemon's supervisor (which
// forwards to the worker for hot config reload) so newly added credentials are
// picked up without a restart. It is a NO-OP (no error, no fatal) when:
//   - the config can't be loaded,
//   - no pid file exists (foreground / test case),
//   - the pid file is stale (the process is gone),
//   - or the signal can't be delivered.
//
// Used by cmdLogin after a successful apikey login. Mirrors cmdReload's pid
// resolution but swallows all errors silently — callers that want errors should
// use `model-proxy reload` directly.
func maybeReloadDaemon(args []string) {
	sa := cliserve.ParseArgs(args)
	cfg, err := LoadConfig(sa.Config)
	if err != nil {
		return
	}
	logFile := cliserve.ResolveLogFile(sa, cfg)
	pidPath := cliserve.PidFilePath(logFile)
	pidStr, err := os.ReadFile(pidPath)
	if err != nil {
		return // no pid file → no daemon running
	}
	var pid int
	for _, c := range pidStr {
		if c < '0' || c > '9' {
			break
		}
		pid = pid*10 + int(c-'0')
	}
	if pid <= 0 {
		return
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		// Stale pid file — clean it up so the next start isn't confused.
		os.Remove(pidPath)
		return
	}
	if err := proc.Signal(syscall.SIGHUP); err == nil {
		fmt.Println("  " + provider.Gray(fmt.Sprintf("(signaled serve to reload: pid=%d)", pid)))
	}
}

// spawnWorker delegates to cliserve.SpawnWorker with the production env.
func spawnWorker(sa cliserve.Args) *exec.Cmd {
	return cliserve.SpawnWorker(daemonEnv(), sa)
}
