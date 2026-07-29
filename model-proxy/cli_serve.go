package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
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

	// Check for subcommand (daemon | stop | reload | status).
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
	case "status":
		cmdServeStatus(args)
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
	if err := runProxyProcess(sa); err != nil {
		log.Fatal(err)
	}
}

// runProxyProcess owns one foreground/worker process lifetime. It returns errors
// to runProxy so deferred signal and pid-file cleanup runs before log.Fatal
// terminates the process.
func runProxyProcess(sa serveArgs) error {
	cfg, err := LoadConfig(sa.config)
	if err != nil {
		return err
	}
	pidPath := "" // foreground-only pid file (login/logout SIGHUP); removed on exit
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
			pidPath = pidFilePath(lf)
			if err := writePidFile(pidPath, os.Getpid()); err != nil {
				pidPath = "" // failed to write -> don't try to remove on exit
			}
		}
	}
	if pidPath != "" {
		defer os.Remove(pidPath)
	}
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	p := NewProxy(cfg)
	// Start all process-owned optional services through the Proxy lifecycle
	// owner (stats flusher, request logger, startup catalog load).
	p.startRuntimeServices(cfg)

	mux := http.NewServeMux()
	mux.HandleFunc("/", p.handler)
	var transportTasks []transportTask
	if cfg.Web.Enabled {
		web := newWebServer(p, sa.config)
		web.logFile = resolveLogFile(sa, cfg)
		web.register(mux)
		transportTasks = append(transportTasks, func(stop <-chan struct{}) {
			if !web.start() {
				return
			}
			<-stop
			web.close()
		})
	}

	// SIGHUP reload is transport/process work: shutdown stops accepting reloads
	// before the HTTP drain and waits for an already-running reload before
	// Proxy.Close performs final lifecycle flushes.
	hupCh := make(chan os.Signal, 1)
	signal.Notify(hupCh, syscall.SIGHUP)
	defer signal.Stop(hupCh)
	transportTasks = append(transportTasks, func(stop <-chan struct{}) {
		runReloadLoop(stop, hupCh, func() {
			log.Printf("[reload] SIGHUP received, reloading config from %s", sa.config)
			if err := p.reload(sa.config); err != nil {
				var applied *reloadAppliedWarning
				if errors.As(err, &applied) {
					log.Printf("[reload] WARNING: %v", err)
				} else {
					log.Printf("[reload] FAILED: %v (keeping old config)", err)
				}
			} else {
				snapshot := p.snapshotRuntime()
				log.Printf("[reload] config reloaded successfully (providers: %s, routes: %s)",
					providerNames(snapshot.cfg), routeNames(snapshot.cfg))
			}
		})
	})

	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		p.Close()
		return err
	}
	server := &http.Server{Addr: cfg.Listen, Handler: mux}
	shutdownCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()

	log.Printf("model-proxy listening on %s (routes: %s)", cfg.Listen, routeNames(cfg))
	return serveHTTPUntilShutdown(
		server,
		listener,
		shutdownCtx.Done(),
		gracefulShutdownTimeout,
		transportTasks,
		p.Close,
	)
}
