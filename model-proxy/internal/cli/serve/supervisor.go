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
