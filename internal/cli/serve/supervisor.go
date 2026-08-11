// supervisor.go owns the daemon supervisor/worker process orchestration:
// daemonize (detached launch), the restart-with-backoff supervisor loop, and
// worker spawning. Config loading is injected so the composition root keeps
// ownership of the config contract.
package serve

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	configdomain "model-proxy/internal/config"
)

// Daemon (supervisor/worker) roles are selected by MODEL_PROXY_ROLE so no new
// subcommand is needed.
const (
	EnvRole        = "MODEL_PROXY_ROLE"
	RoleSupervisor = "supervisor"
	RoleWorker     = "worker"

	// SupervisorWorkerStopWait bounds the graceful SIGTERM window before the
	// supervisor SIGKILLs a stuck worker.
	SupervisorWorkerStopWait = 10 * time.Second
)

// DaemonEnv carries the process seams the daemon paths need from the
// composition root.
type DaemonEnv struct {
	LoadConfig func(path string) (*configdomain.Config, error)
	Executable string // os.Args[0]
	Stdout     *os.File
	Stderr     *os.File
}

// Daemonize launches a detached supervisor (new session, stdio → log file) and
// returns, so the invoking shell gets its prompt back.
func Daemonize(env DaemonEnv, sa Args) error {
	cfg, err := env.LoadConfig(sa.Config)
	if err != nil {
		return err
	}
	logFile := ResolveLogFile(sa, cfg)
	// ResolveLogFile always falls back to the OS temp dir, so this is defensive.
	if logFile == "" {
		return fmt.Errorf("no log_file resolved (set log_file in config or pass --log-file)")
	}
	// Pre-start guard: refuse to launch a second supervisor over a running one.
	// Without this a second `serve daemon` overwrites the pid file, its worker
	// fails to bind (port in use) and enters a restart loop, and `serve stop`
	// then stops the wrong supervisor - leaving the original orphaned with no pid
	// file. If a daemon is already running, point the user at it instead.
	if pid := ReadLivePid(logFile); pid > 0 {
		return fmt.Errorf("model-proxy is already running (supervisor pid=%d); use `serve stop` first, or `serve status` to inspect", pid)
	}
	lf, err := OpenLogFile(logFile)
	if err != nil {
		return fmt.Errorf("open log file %s: %w", logFile, err)
	}

	cmd := exec.Command(env.Executable, "serve", "--config", sa.Config)
	cmd.Env = append(os.Environ(), EnvRole+"="+RoleSupervisor)
	cmd.Stdin = nil
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.SysProcAttr = SysProcAttrDetach() // setsid: detach from controlling terminal
	if err := cmd.Start(); err != nil {
		lf.Close()
		return fmt.Errorf("start supervisor: %w", err)
	}
	// The supervisor inherits the lf fd; the parent can close its copy now.
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	lf.Close()

	fmt.Printf("model-proxy daemonized: supervisor pid=%d log=%s pidfile=%s\n",
		pid, logFile, PidFilePath(logFile))
	fmt.Printf("  stop with: kill -TERM %d  (or kill -TERM $(cat %s))\n", pid, PidFilePath(logFile))
	return nil
}

// SpawnWorker starts a worker process whose stdio is the supervisor's (the log
// file). Returns nil if the process could not be started.
func SpawnWorker(env DaemonEnv, sa Args) *exec.Cmd {
	cmd := exec.Command(env.Executable, "serve", "--config", sa.Config)
	cmd.Env = append(os.Environ(), EnvRole+"="+RoleWorker)
	cmd.Stdin = nil
	cmd.Stdout = env.Stdout
	cmd.Stderr = env.Stderr
	if err := cmd.Start(); err != nil {
		log.Printf("[supervisor] failed to spawn worker: %v", err)
		return nil
	}
	log.Printf("[supervisor] spawned worker pid=%d", cmd.Process.Pid)
	return cmd
}

// RunSupervisor supervises the worker: spawn, wait, restart on exit with
// backoff. Exits when it receives SIGTERM/SIGINT (forwarding SIGTERM to the
// worker first).
func RunSupervisor(env DaemonEnv, sa Args) {
	cfg, err := env.LoadConfig(sa.Config)
	if err != nil {
		log.Fatal(err)
	}
	logFile := ResolveLogFile(sa, cfg)
	// stdio is already the log file (set by daemonize); point the log package at it.
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	pidPath := PidFilePath(logFile)
	if err := WritePidFile(pidPath, os.Getpid()); err != nil {
		log.Printf("[supervisor] warn: write pid file %s: %v", pidPath, err)
	}
	defer os.Remove(pidPath)

	log.Printf("[supervisor] started pid=%d log=%s", os.Getpid(), logFile)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)

	backoff := time.Second
	const maxBackoff = 30 * time.Second
	const uptimeReset = 30 * time.Second // reset backoff after the worker lived this long

	for {
		// Exit if a signal arrived before we spawn the next worker.
		select {
		case sig := <-sigCh:
			log.Printf("[supervisor] received %v, exiting", sig)
			return
		default:
		}

		worker := SpawnWorker(env, sa)
		if worker == nil {
			// Spawn failed — treat as immediate exit, backoff and retry.
			log.Printf("[supervisor] worker spawn failed, retrying in %s", backoff)
			select {
			case sig := <-sigCh:
				if sig == syscall.SIGHUP {
					continue // ignore reload during spawn-failure backoff
				}
				log.Printf("[supervisor] received %v, exiting", sig)
				return
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}
		started := time.Now()
		exitCh := make(chan error, 1)
		go func() { exitCh <- worker.Wait() }()

		select {
		case sig := <-sigCh:
			if sig == syscall.SIGHUP {
				// Forward reload to the worker.
				log.Printf("[supervisor] received SIGHUP, forwarding to worker pid=%d", worker.Process.Pid)
				_ = worker.Process.Signal(syscall.SIGHUP)
				continue
			}
			// Graceful shutdown: forward SIGTERM, wait up to 10s, then SIGKILL.
			log.Printf("[supervisor] received %v, stopping worker pid=%d", sig, worker.Process.Pid)
			_ = worker.Process.Signal(syscall.SIGTERM)
			select {
			case <-exitCh:
			case <-time.After(SupervisorWorkerStopWait):
				log.Printf("[supervisor] worker pid=%d did not exit, killing", worker.Process.Pid)
				_ = worker.Process.Kill()
				<-exitCh
			}
			log.Printf("[supervisor] exiting")
			return
		case err := <-exitCh:
			lived := time.Since(started)
			if lived >= uptimeReset {
				backoff = time.Second // sustained uptime → reset backoff
			}
			log.Printf("[supervisor] worker pid=%d exited after %s: %v — restarting in %s",
				worker.Process.Pid, lived, err, backoff)
		}

		// Backoff wait, interruptible by a stop signal.
		select {
		case sig := <-sigCh:
			log.Printf("[supervisor] received %v during backoff, exiting", sig)
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// CmdStop stops a running `serve daemon` by reading the pid file (derived from
// the same log path the supervisor used) and sending SIGTERM. The supervisor
// forwards SIGTERM to its worker, waits for it, removes the pid file, and
// exits. Waits up to 15s for the process to disappear; falls back to SIGKILL.
func CmdStop(env DaemonEnv, sa Args, yellow, gray, green func(string) string) {
	cfg, err := env.LoadConfig(sa.Config)
	if err != nil {
		log.Fatal(err)
	}
	logFile := ResolveLogFile(sa, cfg)
	pidPath := PidFilePath(logFile)

	pidStr, err := os.ReadFile(pidPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println(yellow("No daemon running.") + " (pid file not found: " + gray(pidPath) + ")")
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
		fmt.Println(yellow("Daemon not running.") + " (removed stale pid file " + gray(pidPath) + ")")
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
			fmt.Println(green("✓ Stopped."))
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Didn't exit gracefully — force kill.
	fmt.Fprintf(os.Stderr, "graceful stop timed out, sending SIGKILL to %d\n", pid)
	_ = proc.Kill()
	os.Remove(pidPath)
	fmt.Println(green("✓ Killed."))
}

// CmdReload sends SIGHUP to a running daemon's supervisor, which forwards it
// to the worker for hot config reload.
func CmdReload(env DaemonEnv, sa Args, yellow, gray, green func(string) string) {
	cfg, err := env.LoadConfig(sa.Config)
	if err != nil {
		log.Fatal(err)
	}
	logFile := ResolveLogFile(sa, cfg)
	pidPath := PidFilePath(logFile)

	pidStr, err := os.ReadFile(pidPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println(yellow("No daemon running.") + " (pid file not found: " + gray(pidPath) + ")")
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
		fmt.Println(yellow("Daemon not running.") + " (removed stale pid file)")
		return
	}
	fmt.Printf("Reloading model-proxy daemon (pid=%d)...\n", pid)
	if err := proc.Signal(syscall.SIGHUP); err != nil {
		log.Fatalf("send SIGHUP to %d: %v", pid, err)
	}
	fmt.Println(green("✓ Reload signal sent.") + " Check logs for [reload] lines.")
}

// SignalReloadDaemon sends SIGHUP to a running daemon's supervisor (which
// forwards to the worker for hot config reload) so newly added credentials are
// picked up without a restart. It is a NO-OP (no error, no fatal) when the
// config can't be loaded, no pid file exists, the pid file is stale, or the
// signal can't be delivered. Used after a successful login; callers that want
// errors should use `serve reload` directly.
func SignalReloadDaemon(env DaemonEnv, sa Args, gray func(string) string) {
	cfg, err := env.LoadConfig(sa.Config)
	if err != nil {
		return
	}
	logFile := ResolveLogFile(sa, cfg)
	pidPath := PidFilePath(logFile)
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
		fmt.Println("  " + gray(fmt.Sprintf("(signaled serve to reload: pid=%d)", pid)))
	}
}
