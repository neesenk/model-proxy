package main

import (
	"context"
	"io"
	"log"
	clicmd "model-proxy/internal/cli"
	cliframework "model-proxy/internal/cli/framework"
	cliserve "model-proxy/internal/cli/serve"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/provider"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
)

// cmdServe dispatches by role and subcommand:
//   - no subcommand: foreground proxy (or supervisor/worker if env role set)
//   - "daemon": launch a detached supervisor
//   - "stop": SIGTERM a running daemon
func cmdServe(args []string) {
	serveAssembly{}.command(args)
}

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
		cliserve.CmdStop(daemonEnv(), cliserve.ParseArgs(args), provider.Yellow, provider.Gray, provider.Green)
	case "reload":
		cliserve.CmdReload(daemonEnv(), cliserve.ParseArgs(args), provider.Yellow, provider.Gray, provider.Green)
	case "status":
		clicmd.RunServeStatus(args)
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
				provider.LogColorEnabled = false
			}
			// Write a pid file so `login`/`logout` can SIGHUP this foreground
			// serve to hot-reload new credentials. The daemon supervisor writes
			// its own pid file; the worker skips this block (envRole is set), so
			// there's no double-write. Remove it on SIGINT/SIGTERM so it doesn't
			// go stale (the reload path self-heals stale pids too, via Signal(0)).
			pidPath = cliserve.PidFilePath(lf)
			if err := cliserve.WritePidFile(pidPath, os.Getpid()); err != nil {
				pidPath = "" // failed to write -> don't try to remove on exit
			}
		}
	}
	if pidPath != "" {
		defer os.Remove(pidPath)
	}
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
	server := &http.Server{Addr: runtime.StartupConfig.Listen, Handler: runtime.Handler}
	shutdownCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	log.Printf("model-proxy listening on %s (routes: %s)",
		runtime.StartupConfig.Listen, cliframework.RouteNames(runtime.StartupConfig))
	return cliserve.ServeHTTPUntilShutdown(
		server,
		listener,
		shutdownCtx.Done(),
		cliserve.GracefulShutdownTimeout,
		runtime.TransportTasks,
		runtime.Close,
	)
}
