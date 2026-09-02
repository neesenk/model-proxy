package main

import (
	"context"
	"io"
	"log"
	cliframework "model-proxy/internal/cli/framework"
	cliserve "model-proxy/internal/cli/serve"
	clistatus "model-proxy/internal/cli/status"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/display"
	"model-proxy/internal/observe/logx"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// cmdServe dispatches by role and subcommand:
//   - no subcommand: foreground proxy (or supervisor/worker if env role set)
//   - "daemon": launch a detached supervisor
//   - "stop": SIGTERM a running daemon
//
// The serve driver is injected into the CLI command registry via
// serveAssembly{}.command; see newApplication in app_assembly.go.
func (assembly serveAssembly) command(args []string) {
	// Worker/supervisor processes have envRole set — they always run their loop
	// regardless of subcommand (the subcommand was consumed by the parent).
	role := os.Getenv(cliserve.EnvRole)
	if role == cliserve.RoleSupervisor {
		sa := cliserve.ParseArgs(args)
		cliserve.RunSupervisor(daemonEnv(), sa)
		return
	}
	if role == cliserve.RoleWorker {
		sa := cliserve.ParseArgs(args)
		assembly.runProxy(sa)
		return
	}

	// Check for subcommand (daemon | stop | reload | status).
	sub := cliframework.Positional(args)
	switch sub {
	case "daemon":
		sa := cliserve.ParseArgs(args)
		if err := cliserve.Daemonize(daemonEnv(), sa); err != nil {
			log.Fatal(err)
		}
	case "stop":
		cliserve.CmdStop(daemonEnv(), cliserve.ParseArgs(args), display.Yellow, display.Gray, display.Green)
	case "reload":
		cliserve.CmdReload(daemonEnv(), cliserve.ParseArgs(args), display.Yellow, display.Gray, display.Green)
	case "status":
		clistatus.RunServeStatus(args)
	default:
		// No subcommand — foreground serve.
		sa := cliserve.ParseArgs(args)
		assembly.runProxy(sa)
	}
}

// daemonEnv wires the production process seams for the daemon paths.
func daemonEnv() cliserve.DaemonEnv {
	return cliserve.DaemonEnv{LoadConfig: configdomain.LoadConfig, Executable: os.Args[0], Stdout: os.Stdout, Stderr: os.Stderr}
}

// runProxy loads the config and runs the proxy inline (used by the worker and
// plain foreground serve). When stdout/stderr is a log file (worker case) all
// logs land there; when a tty (foreground) logs go to the terminal.
func (assembly serveAssembly) runProxy(sa cliserve.Args) {
	if err := assembly.runProxyProcess(sa); err != nil {
		log.Fatal(err)
	}
}

// runProxyProcess owns one foreground/worker process lifetime. It returns errors
// to runProxy so deferred signal and pid-file cleanup runs before log.Fatal.
func (serveAssembly) runProxyProcess(sa cliserve.Args) error {
	cfg, err := configdomain.LoadConfig(sa.Config)
	if err != nil {
		return err
	}
	// log_level is startup-only (reload does not re-read it): apply it once
	// here, before any runtime log line is emitted.
	logx.SetLevel(cfg.LogLevel)
	pidPath := "" // foreground-only pid file (login/logout SIGHUP); removed on exit
	// In true foreground mode (no role env), mirror logs to the configured file.
	// The worker's stdio is already the log file (set by the supervisor), so it
	// must NOT reopen/mirror — that would double every line.
	if os.Getenv(cliserve.EnvRole) == "" {
		if lf := cliserve.ResolveLogFile(sa, cfg); lf != "" {
			if f, err := cliserve.OpenLogFile(lf); err == nil {
				log.SetOutput(io.MultiWriter(os.Stderr, f))
				// Logs now land in a file too → disable color so escape codes
				// don't pollute the file (logColorEnabled was set at init from
				// stderr being a tty, but the MultiWriter writes the same bytes
				// to both, and files must stay escape-free).
				display.LogColorEnabled = false
			} else {
				// Not fatal, but never silent: the operator asked for a log
				// file and would otherwise discover the loss only when the
				// file is empty or missing after a crash.
				logx.Warnf("model-proxy: could not open log file %s: %v (logging to stderr only)", lf, err)
			}
			// Write a pid file so `login`/`logout` can SIGHUP this foreground
			// serve to hot-reload new credentials. The daemon supervisor writes
			// its own pid file; the worker skips this block (envRole is set), so
			// there's no double-write. When a live daemon (or another serve)
			// already owns the file, DON'T take it over — overwriting would
			// point stop/reload at us, and our exit cleanup would delete the
			// daemon's pid file and orphan it.
			pidPath = cliserve.ClaimForegroundPidFile(lf)
		}
	}
	defer cliserve.ReleasePidFileIfOwned(pidPath)
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	runtime := newApplicationRuntime(cfg, sa)

	// SIGHUP reload is transport/process work: shutdown stops accepting reloads
	// before the HTTP drain and waits for an already-running reload before
	// Proxy.Close performs final lifecycle flushes.
	hupCh := make(chan os.Signal, 1)
	signal.Notify(hupCh, syscall.SIGHUP)
	defer signal.Stop(hupCh)
	runtime.TransportTasks = append(runtime.TransportTasks, func(stop <-chan struct{}) {
		cliserve.RunReloadLoop(stop, hupCh, runtime.Reload)
	})

	listener, err := net.Listen("tcp", runtime.StartupConfig.Listen)
	if err != nil {
		runtime.Close()
		return err
	}
	// ReadHeaderTimeout bounds how long a connection may sit before its
	// headers arrive: without it a slow/idle peer pins a handler goroutine
	// forever (the body timeout comes from upstream_timeout per request).
	server := &http.Server{
		Addr:              runtime.StartupConfig.Listen,
		Handler:           runtime.Handler,
		ReadHeaderTimeout: 30 * time.Second,
	}
	shutdownCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	logx.Infof("model-proxy listening on %s (routes: %s)",
		runtime.StartupConfig.Listen, runtime.StartupConfig.RouteNames())
	return cliserve.ServeHTTPUntilShutdown(
		server,
		listener,
		shutdownCtx.Done(),
		cliserve.GracefulShutdownTimeout,
		runtime.TransportTasks,
		runtime.Close,
	)
}
