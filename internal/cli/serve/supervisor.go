// supervisor.go owns the daemon supervisor/worker process orchestration:
// daemonize (detached launch), the restart-with-backoff supervisor loop, and
// worker spawning. Config loading is injected so the composition root keeps
// ownership of the config contract.
package serve

import (
	"fmt"
	"log"
	"model-proxy/internal/observe/logx"
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

// supervisorWorker is the narrow process contract needed by the supervisor
// state machine. Keeping it package-private lets tests drive lifecycle edges
// without exposing process-control hooks to callers or signalling real user
// processes.
type supervisorWorker interface {
	PID() int
	Wait() error
	Signal(os.Signal) error
	Kill() error
}

type execSupervisorWorker struct{ cmd *exec.Cmd }

func (w execSupervisorWorker) PID() int                   { return w.cmd.Process.Pid }
func (w execSupervisorWorker) Wait() error                { return w.cmd.Wait() }
func (w execSupervisorWorker) Signal(sig os.Signal) error { return w.cmd.Process.Signal(sig) }
func (w execSupervisorWorker) Kill() error                { return w.cmd.Process.Kill() }

type supervisorDeps struct {
	spawn   func(DaemonEnv, Args) supervisorWorker
	signals <-chan os.Signal
	after   func(time.Duration) <-chan time.Time
	now     func() time.Time
	pid     func() int
}

func productionSupervisorDeps(signals <-chan os.Signal) supervisorDeps {
	return supervisorDeps{
		spawn: func(env DaemonEnv, sa Args) supervisorWorker {
			cmd := SpawnWorker(env, sa)
			if cmd == nil {
				return nil
			}
			return execSupervisorWorker{cmd: cmd}
		},
		signals: signals,
		after:   time.After,
		now:     time.Now,
		pid:     os.Getpid,
	}
}

// supervisorArgs builds the argv the supervisor is spawned with. --log-file is
// forwarded so the supervisor resolves the SAME log (and pid) file the parent
// claimed; without it the parent and supervisor would disagree whenever the
// flag is set.
func supervisorArgs(sa Args) []string {
	args := []string{"serve", "--config", sa.Config}
	if sa.LogFile != "" {
		args = append(args, "--log-file", sa.LogFile)
	}
	return args
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
	// Close the precheck→write double-start window: claim the pid file with an
	// atomic O_EXCL create the moment the precheck passes. Without the claim,
	// two concurrent `serve daemon` starts both pass the precheck and the second
	// supervisor's pid write clobbers the first. The placeholder names THIS
	// process — live while it runs, so a racing starter's re-read refuses; the
	// spawned supervisor overwrites the same file with its own pid (runSupervisor
	// writes to the identical path), and any later failure here removes it again.
	pidPath := PidFilePath(logFile)
	placeholder, err := os.OpenFile(pidPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		// Lost the race, or the path is unusable. Re-read: a live owner means a
		// daemon (or another starter) won — refuse; anything else is an error.
		if pid := ReadLivePid(logFile); pid > 0 {
			return fmt.Errorf("model-proxy is already running (supervisor pid=%d); use `serve stop` first, or `serve status` to inspect", pid)
		}
		return fmt.Errorf("claim pid file %s: %w", pidPath, err)
	}
	if _, err := fmt.Fprintf(placeholder, "%d\n", os.Getpid()); err != nil {
		placeholder.Close()
		os.Remove(pidPath)
		return fmt.Errorf("write pid file placeholder %s: %w", pidPath, err)
	}
	placeholder.Close()

	lf, err := OpenLogFile(logFile)
	if err != nil {
		os.Remove(pidPath)
		return fmt.Errorf("open log file %s: %w", logFile, err)
	}

	cmd := exec.Command(env.Executable, supervisorArgs(sa)...)
	cmd.Env = append(os.Environ(), EnvRole+"="+RoleSupervisor)
	cmd.Stdin = nil
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.SysProcAttr = SysProcAttrDetach() // setsid: detach from controlling terminal
	if err := cmd.Start(); err != nil {
		lf.Close()
		os.Remove(pidPath)
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
		logx.Warnf("[supervisor] failed to spawn worker: %v", err)
		return nil
	}
	logx.Infof("[supervisor] spawned worker pid=%d", cmd.Process.Pid)
	return cmd
}

// RunSupervisor supervises the worker: spawn, wait, restart on exit with
// backoff. Exits when it receives SIGTERM/SIGINT (forwarding SIGTERM to the
// worker first).
func RunSupervisor(env DaemonEnv, sa Args) {
	err := func() error {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
		defer signal.Stop(sigCh)
		return runSupervisor(env, sa, productionSupervisorDeps(sigCh))
	}()
	if err != nil {
		log.Fatal(err)
	}
}

// runSupervisor contains the daemon's restart/shutdown state machine. OS
// signal registration and real process construction stay in RunSupervisor;
// this core is deterministic under tests and never needs to signal a process
// outside the supplied worker contract.
func runSupervisor(env DaemonEnv, sa Args, deps supervisorDeps) error {
	cfg, err := env.LoadConfig(sa.Config)
	if err != nil {
		return err
	}
	logFile := ResolveLogFile(sa, cfg)
	// stdio is already the log file (set by daemonize); point the log package at it.
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	pidPath := PidFilePath(logFile)
	pidWritten := false
	if err := WritePidFile(pidPath, deps.pid()); err != nil {
		logx.Warnf("[supervisor] warn: write pid file %s: %v", pidPath, err)
	} else {
		pidWritten = true
	}
	if pidWritten {
		defer os.Remove(pidPath)
	}

	logx.Infof("[supervisor] started pid=%d log=%s", deps.pid(), logFile)

	backoff := time.Second
	const maxBackoff = 30 * time.Second
	const uptimeReset = 30 * time.Second // reset backoff after the worker lived this long

	for {
		// Exit if a signal arrived before we spawn the next worker.
		select {
		case sig := <-deps.signals:
			if sig == syscall.SIGHUP {
				continue
			}
			logx.Infof("[supervisor] received %v, exiting", sig)
			return nil
		default:
		}

		worker := deps.spawn(env, sa)
		if worker == nil {
			// Spawn failed — treat as immediate exit, backoff and retry.
			logx.Warnf("[supervisor] worker spawn failed, retrying in %s", backoff)
			if !waitSupervisorBackoff(deps.signals, deps.after(backoff)) {
				return nil
			}
			backoff = nextSupervisorBackoff(backoff, maxBackoff)
			continue
		}
		started := deps.now()
		exitCh := make(chan error, 1)
		go func() { exitCh <- worker.Wait() }()

		workerExited := false
		for !workerExited {
			select {
			case sig := <-deps.signals:
				if sig == syscall.SIGHUP {
					// Remain in this worker's wait loop after forwarding. Starting
					// another worker here would race the still-running listener.
					logx.Infof("[supervisor] received SIGHUP, forwarding to worker pid=%d", worker.PID())
					_ = worker.Signal(syscall.SIGHUP)
					continue
				}
				// Graceful shutdown: forward SIGTERM, wait up to 10s, then SIGKILL.
				logx.Infof("[supervisor] received %v, stopping worker pid=%d", sig, worker.PID())
				_ = worker.Signal(syscall.SIGTERM)
				select {
				case <-exitCh:
				case <-deps.after(SupervisorWorkerStopWait):
					logx.Warnf("[supervisor] worker pid=%d did not exit, killing", worker.PID())
					_ = worker.Kill()
					<-exitCh
				}
				logx.Infof("[supervisor] exiting")
				return nil
			case err := <-exitCh:
				lived := deps.now().Sub(started)
				if lived >= uptimeReset {
					backoff = time.Second // sustained uptime → reset backoff
				}
				logx.Warnf("[supervisor] worker pid=%d exited after %s: %v — restarting in %s",
					worker.PID(), lived, err, backoff)
				workerExited = true
			}
		}

		// Backoff wait, interruptible by a stop signal.
		if !waitSupervisorBackoff(deps.signals, deps.after(backoff)) {
			return nil
		}
		backoff = nextSupervisorBackoff(backoff, maxBackoff)
	}
}

func waitSupervisorBackoff(signals <-chan os.Signal, elapsed <-chan time.Time) bool {
	for {
		select {
		case sig := <-signals:
			if sig == syscall.SIGHUP {
				continue
			}
			logx.Infof("[supervisor] received %v during backoff, exiting", sig)
			return false
		case <-elapsed:
			return true
		}
	}
}

func nextSupervisorBackoff(current, maximum time.Duration) time.Duration {
	next := current * 2
	if next > maximum {
		return maximum
	}
	return next
}

// Wait budgets for `serve stop`: the graceful SIGTERM window, and the window
// verifying a SIGKILLed daemon is actually gone before reporting success.
const (
	stopGracefulWait = 15 * time.Second
	stopKillExitWait = 5 * time.Second
)

// resolveDaemonPid resolves the live daemon supervisor's process from the
// pid file derived from the configured log path — the shared prelude of
// CmdStop and CmdReload. ok == false means a user-facing message was already
// printed (no daemon running, or a stale pid file removed) and the caller
// must return. Terminal errors (bad config, unreadable/invalid pid file,
// unfindable process) stay log.Fatal, matching the historical contract.
func resolveDaemonPid(env DaemonEnv, sa Args, yellow, gray func(string) string) (proc *os.Process, pid int, pidPath string, ok bool) {
	cfg, err := env.LoadConfig(sa.Config)
	if err != nil {
		log.Fatal(err)
	}
	logFile := ResolveLogFile(sa, cfg)
	pidPath = PidFilePath(logFile)

	pidStr, err := os.ReadFile(pidPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println(yellow("No daemon running.") + " (pid file not found: " + gray(pidPath) + ")")
			return nil, 0, pidPath, false
		}
		log.Fatal(err)
	}
	pid = parsePidFileContents(pidStr)
	if pid <= 0 {
		log.Fatalf("invalid pid in %s: %q", pidPath, string(pidStr))
	}

	// Check the process exists and is signalable.
	proc, err = os.FindProcess(pid)
	if err != nil {
		log.Fatalf("find process %d: %v", pid, err)
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		// Process is gone — clean up the stale pid file.
		os.Remove(pidPath)
		fmt.Println(yellow("Daemon not running.") + " (removed stale pid file " + gray(pidPath) + ")")
		return nil, 0, pidPath, false
	}
	return proc, pid, pidPath, true
}

// CmdStop stops a running `serve daemon` by reading the pid file (derived from
// the same log path the supervisor used) and sending SIGTERM. The supervisor
// forwards SIGTERM to its worker, waits for it, removes the pid file, and
// exits. Waits up to 15s for the process to disappear; falls back to SIGKILL
// and then verifies the exit before claiming success.
func CmdStop(env DaemonEnv, sa Args, yellow, gray, green func(string) string) {
	proc, pid, pidPath, ok := resolveDaemonPid(env, sa, yellow, gray)
	if !ok {
		return
	}

	fmt.Printf("Stopping model-proxy daemon (pid=%d)...\n", pid)
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		log.Fatalf("send SIGTERM to %d: %v", pid, err)
	}
	stopDaemonProcess(proc, pid, pidPath, stopGracefulWait, stopKillExitWait, yellow, green)
}

// stopDaemonProcess waits (bounded) for the SIGTERMed daemon to exit and
// escalates to SIGKILL, verifying the process really disappeared before
// reporting success — "✓ Killed." used to print immediately after the signal,
// while the daemon could still hold the listen port and break
// `serve stop && serve daemon` restart scripts. Split from CmdStop so tests
// can drive the escalate/verify edges with millisecond budgets.
func stopDaemonProcess(proc *os.Process, pid int, pidPath string, graceful, killExit time.Duration, yellow, green func(string) string) {
	if waitForProcessExit(proc, graceful) {
		fmt.Println(green("✓ Stopped."))
		return
	}
	fmt.Fprintf(os.Stderr, "graceful stop timed out, sending SIGKILL to %d\n", pid)
	_ = proc.Kill()
	os.Remove(pidPath)
	if waitForProcessExit(proc, killExit) {
		fmt.Println(green("✓ Killed."))
		return
	}
	fmt.Println(yellow(fmt.Sprintf("⚠ SIGKILL sent to %d but it has not exited yet; verify the process manually.", pid)))
}

// waitForProcessExit polls process liveness (Signal 0) until the process is
// gone or the deadline passes; it reports whether the process disappeared.
func waitForProcessExit(proc *os.Process, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// CmdReload sends SIGHUP to a running daemon's supervisor, which forwards it
// to the worker for hot config reload.
func CmdReload(env DaemonEnv, sa Args, yellow, gray, green func(string) string) {
	proc, pid, _, ok := resolveDaemonPid(env, sa, yellow, gray)
	if !ok {
		return
	}
	fmt.Printf("Reloading model-proxy daemon (pid=%d)...\n", pid)
	if err := proc.Signal(syscall.SIGHUP); err != nil {
		log.Fatalf("send SIGHUP to %d: %v", pid, err)
	}
	fmt.Println(green("✓ Reload signal sent.") + " Check logs for [reload] lines.")
}
