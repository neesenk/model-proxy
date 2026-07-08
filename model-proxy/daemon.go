package main

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Daemon (supervisor/worker) model for `model-proxy serve --daemon`.
//
// Roles are selected by the MODEL_PROXY_ROLE env var so no new subcommand is needed:
//   - (unset)  foreground invocation. With --daemon it launches a detached
//               supervisor and returns; otherwise it runs the proxy inline.
//   - supervisor: detaches from the terminal, redirects stdio to the log file,
//               writes a pid file, and supervises the worker: spawn → wait →
//               restart on exit (exponential backoff, reset after sustained uptime).
//               Forwards SIGTERM/SIGINT to the worker and exits.
//   - worker:   runs the actual proxy (http.ListenAndServe). stdio is already the
//               log file (set by the supervisor), so all logs land there. Because
//               stderr is a regular file, color.go's tty check auto-disables color
//               → file logs stay escape-free.

const (
	envRole        = "MODEL_PROXY_ROLE"
	roleSupervisor = "supervisor"
	roleWorker     = "worker"
)

// serveArgs holds parsed `serve` flags.
type serveArgs struct {
	config  string
	logFile string // --log-file override
}

func parseServeArgs(args []string) serveArgs {
	sa := serveArgs{config: configPath(args)}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--log-file":
			if i+1 < len(args) {
				sa.logFile = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--log-file="):
			sa.logFile = strings.TrimPrefix(a, "--log-file=")
		}
	}
	return sa
}

// cmdServe dispatches by role and subcommand:
//   - no subcommand: foreground proxy (or supervisor/worker if env role set)
//   - "daemon": launch a detached supervisor
//   - "stop": SIGTERM a running daemon
func cmdServe(args []string) {
	// Worker/supervisor processes have envRole set — they always run their loop
	// regardless of subcommand (the subcommand was consumed by the parent).
	role := os.Getenv(envRole)
	if role == roleSupervisor {
		sa := parseServeArgs(args)
		runSupervisor(sa)
		return
	}
	if role == roleWorker {
		sa := parseServeArgs(args)
		runProxy(sa)
		return
	}

	// Check for subcommand (daemon | stop).
	sub := positional(args)
	switch sub {
	case "daemon":
		sa := parseServeArgs(args)
		if err := daemonize(sa); err != nil {
			log.Fatal(err)
		}
	case "stop":
		cmdStop(args)
	case "reload":
		cmdReload(args)
	default:
		// No subcommand — foreground serve.
		sa := parseServeArgs(args)
		runProxy(sa)
	}
}

// runProxy loads the config and runs the proxy inline (used by the worker and by
// plain foreground serve). When stdout/stderr is a log file (worker case) all
// logs land there; when a tty (foreground) logs go to the terminal.
func runProxy(sa serveArgs) {
	cfg, err := LoadConfig(sa.config)
	if err != nil {
		log.Fatal(err)
	}
	// In true foreground mode (no role env), mirror logs to the configured file.
	// The worker's stdio is already the log file (set by the supervisor), so it
	// must NOT reopen/mirror — that would double every line.
	if os.Getenv(envRole) == "" {
		if lf := resolveLogFile(sa, cfg); lf != "" {
			if f, err := openLogFile(lf); err == nil {
				log.SetOutput(io.MultiWriter(os.Stderr, f))
				// Logs now land in a file too → disable color so escape codes
				// don't pollute the file (logColorEnabled was set at init from
				// stderr being a tty, but the MultiWriter writes the same bytes
				// to both, and files must stay escape-free).
				logColorEnabled = false
			}
			// Write a pid file so `login`/`logout` can SIGHUP this foreground
			// serve to hot-reload new credentials. The daemon supervisor writes
			// its own pid file; the worker skips this block (envRole is set), so
			// there's no double-write. Remove it on SIGINT/SIGTERM so it doesn't
			// go stale (the reload path self-heals stale pids too, via Signal(0)).
			pidPath := pidFilePath(lf)
			if err := writePidFile(pidPath, os.Getpid()); err == nil {
				go func() {
					sigCh := make(chan os.Signal, 1)
					signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
					<-sigCh
					os.Remove(pidPath)
					os.Exit(0)
				}()
			}
		}
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	p := NewProxy(cfg)
	// Periodically persist the in-memory token counter so observed usage
	// survives a restart. The counter loads its baseline in NewProxy.
	go persistTokensLoop(p.tokens)
	// SIGHUP → hot reload config.
	go func() {
		hupCh := make(chan os.Signal, 1)
		signal.Notify(hupCh, syscall.SIGHUP)
		for range hupCh {
			log.Printf("[reload] SIGHUP received, reloading config from %s", sa.config)
			if err := p.reload(sa.config); err != nil {
				log.Printf("[reload] FAILED: %v (keeping old config)", err)
			} else {
				log.Printf("[reload] config reloaded successfully (providers: %s, routes: %s)",
					providerNames(p.cfg), routeNames(p.cfg))
			}
		}
	}()
	mux := http.NewServeMux()
	mux.HandleFunc("/", p.handler)
	if cfg.Web.Enabled {
		web := newWebServer(p, sa.config)
		web.logFile = resolveLogFile(sa, cfg)
		web.register(mux)
		go webGC(web.sessions)
	}
	log.Printf("model-proxy listening on %s (routes: %s)", cfg.Listen, routeNames(cfg))
	if err := http.ListenAndServe(cfg.Listen, mux); err != nil {
		log.Fatal(err)
	}
}

// persistTokensLoop periodically saves the in-memory token counter to disk so
// observed usage survives a restart. Mirrors the quota poller's lifecycle: the
// counter is created and baseline-loaded in NewProxy; this loop only writes.
// Nil-safe so degenerate tests / a nil counter are a no-op.
func persistTokensLoop(tc *tokenCounter) {
	if tc == nil {
		return
	}
	t := time.NewTicker(5 * time.Minute)
	for range t.C {
		if err := tc.save(); err != nil {
			log.Printf("[tokens] persist failed: %v", err)
		}
	}
}

// daemonize launches a detached supervisor (new session, stdio → log file) and
// returns, so the invoking shell gets its prompt back.
func daemonize(sa serveArgs) error {
	cfg, err := LoadConfig(sa.config)
	if err != nil {
		return err
	}
	logFile := resolveLogFile(sa, cfg)
	// resolveLogFile always falls back to the OS temp dir, so this is defensive.
	if logFile == "" {
		return fmt.Errorf("no log_file resolved (set log_file in config or pass --log-file)")
	}
	lf, err := openLogFile(logFile)
	if err != nil {
		return fmt.Errorf("open log file %s: %w", logFile, err)
	}

	cmd := exec.Command(os.Args[0], "serve", "--config", sa.config)
	cmd.Env = append(os.Environ(), envRole+"="+roleSupervisor)
	cmd.Stdin = nil
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.SysProcAttr = sysProcAttrDetach() // setsid: detach from controlling terminal
	if err := cmd.Start(); err != nil {
		lf.Close()
		return fmt.Errorf("start supervisor: %w", err)
	}
	// The supervisor inherits the lf fd; the parent can close its copy now.
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	lf.Close()

	fmt.Printf("model-proxy daemonized: supervisor pid=%d log=%s pidfile=%s\n",
		pid, logFile, pidFilePath(logFile))
	fmt.Printf("  stop with: kill -TERM %d  (or kill -TERM $(cat %s))\n", pid, pidFilePath(logFile))
	return nil
}

// runSupervisor supervises the worker: spawn, wait, restart on exit with backoff.
// Exits when it receives SIGTERM/SIGINT (forwarding SIGTERM to the worker first).
func runSupervisor(sa serveArgs) {
	cfg, err := LoadConfig(sa.config)
	if err != nil {
		log.Fatal(err)
	}
	logFile := resolveLogFile(sa, cfg)
	// stdio is already the log file (set by daemonize); point the log package at it.
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	pidPath := pidFilePath(logFile)
	if err := writePidFile(pidPath, os.Getpid()); err != nil {
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

		worker := spawnWorker(sa)
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
			case <-time.After(10 * time.Second):
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

// cmdStop stops a running `serve --daemon` by reading the pid file (derived from
// the same log path the supervisor used) and sending SIGTERM. The supervisor
// forwards SIGTERM to its worker, waits for it, removes the pid file, and exits.
// Waits up to 15s for the process to disappear; falls back to SIGKILL.
func cmdStop(args []string) {
	sa := parseServeArgs(args)
	cfg, err := LoadConfig(sa.config)
	if err != nil {
		log.Fatal(err)
	}
	logFile := resolveLogFile(sa, cfg)
	pidPath := pidFilePath(logFile)

	pidStr, err := os.ReadFile(pidPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println(cYellow("No daemon running.") + " (pid file not found: " + cGray(pidPath) + ")")
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
		fmt.Println(cYellow("Daemon not running.") + " (removed stale pid file " + cGray(pidPath) + ")")
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
			fmt.Println(cGreen("✓ Stopped."))
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Didn't exit gracefully — force kill.
	fmt.Fprintf(os.Stderr, "graceful stop timed out, sending SIGKILL to %d\n", pid)
	_ = proc.Kill()
	os.Remove(pidPath)
	fmt.Println(cGreen("✓ Killed."))
}

// cmdReload sends SIGHUP to a running daemon's supervisor, which forwards it
// to the worker for hot config reload.
func cmdReload(args []string) {
	sa := parseServeArgs(args)
	cfg, err := LoadConfig(sa.config)
	if err != nil {
		log.Fatal(err)
	}
	logFile := resolveLogFile(sa, cfg)
	pidPath := pidFilePath(logFile)

	pidStr, err := os.ReadFile(pidPath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println(cYellow("No daemon running.") + " (pid file not found: " + cGray(pidPath) + ")")
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
		fmt.Println(cYellow("Daemon not running.") + " (removed stale pid file)")
		return
	}
	fmt.Printf("Reloading model-proxy daemon (pid=%d)...\n", pid)
	if err := proc.Signal(syscall.SIGHUP); err != nil {
		log.Fatalf("send SIGHUP to %d: %v", pid, err)
	}
	fmt.Println(cGreen("✓ Reload signal sent.") + " Check logs for [reload] lines.")
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
	sa := parseServeArgs(args)
	cfg, err := LoadConfig(sa.config)
	if err != nil {
		return
	}
	logFile := resolveLogFile(sa, cfg)
	pidPath := pidFilePath(logFile)
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
		fmt.Println("  " + cGray(fmt.Sprintf("(signaled serve to reload: pid=%d)", pid)))
	}
}

// spawnWorker starts a worker process whose stdio is the supervisor's (the log file).
// Returns nil if the process could not be started.
func spawnWorker(sa serveArgs) *exec.Cmd {
	cmd := exec.Command(os.Args[0], "serve", "--config", sa.config)
	cmd.Env = append(os.Environ(), envRole+"="+roleWorker)
	cmd.Stdin = nil
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		log.Printf("[supervisor] failed to spawn worker: %v", err)
		return nil
	}
	log.Printf("[supervisor] spawned worker pid=%d", cmd.Process.Pid)
	return cmd
}

// resolveLogFile picks the log file path: --log-file flag > config log_file > default
// (the OS temp dir, e.g. /tmp on Linux, $TMPDIR on macOS — runtime artifacts belong
// there, not under the config dir). Returns "" only if the temp dir can't be resolved.
func resolveLogFile(sa serveArgs, cfg *Config) string {
	if sa.logFile != "" {
		return expandPath(sa.logFile)
	}
	if cfg.LogFile != "" {
		return cfg.LogFile
	}
	// Default: the OS temp dir (runtime artifacts: logs + pid), per Unix convention.
	return filepath.Join(os.TempDir(), "model-proxy.log")
}

// openLogFile opens (creating parent dirs) a log file for append.
func openLogFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

// pidFilePath derives the pid file path from the log file path.
func pidFilePath(logFile string) string {
	if strings.HasSuffix(logFile, ".log") {
		return strings.TrimSuffix(logFile, ".log") + ".pid"
	}
	return logFile + ".pid"
}

func writePidFile(path string, pid int) error {
	return os.WriteFile(path, []byte(fmt.Sprintf("%d\n", pid)), 0o644)
}
