package main

import (
	"context"
	"io"
	"log"
	cliframework "model-proxy/internal/cli/framework"
	cliserve "model-proxy/internal/cli/serve"
	"model-proxy/provider"
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
	role := os.Getenv(envRole)
	if role == roleSupervisor {
		sa := cliserve.ParseArgs(args)
		runSupervisor(sa)
		return
	}
	if role == roleWorker {
		sa := cliserve.ParseArgs(args)
		assembly.runProxy(sa)
		return
	}

	// Check for subcommand (daemon | stop | reload | status).
	sub := cliframework.Positional(args)
	switch sub {
	case "daemon":
		sa := cliserve.ParseArgs(args)
		if err := daemonize(sa); err != nil {
			log.Fatal(err)
		}
	case "stop":
		cmdStop(args)
	case "reload":
		cmdReload(args)
	case "status":
		cmdServeStatusCLI(args)
	default:
		// No subcommand — foreground serve.
		sa := cliserve.ParseArgs(args)
		assembly.runProxy(sa)
	}
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
	cfg, err := LoadConfig(sa.Config)
	if err != nil {
		return err
	}
	pidPath := "" // foreground-only pid file (login/logout SIGHUP); removed on exit
	// In true foreground mode (no role env), mirror logs to the configured file.
	// The worker's stdio is already the log file (set by the supervisor), so it
	// must NOT reopen/mirror — that would double every line.
	if os.Getenv(envRole) == "" {
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
	runtime.transportTasks = append(runtime.transportTasks, func(stop <-chan struct{}) {
		runReloadLoop(stop, hupCh, runtime.reload)
	})

	listener, err := net.Listen("tcp", runtime.startupConfig.Listen)
	if err != nil {
		runtime.Close()
		return err
	}
	server := &http.Server{Addr: runtime.startupConfig.Listen, Handler: runtime.handler}
	shutdownCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	log.Printf("model-proxy listening on %s (routes: %s)",
		runtime.startupConfig.Listen, routeNames(runtime.startupConfig))
	return serveHTTPUntilShutdown(
		server,
		listener,
		shutdownCtx.Done(),
		gracefulShutdownTimeout,
		runtime.transportTasks,
		runtime.Close,
	)
}
