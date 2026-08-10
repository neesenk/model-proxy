package main

import (
	"errors"
	"io"
	"log"
	appdomain "model-proxy/internal/app"
	cliframework "model-proxy/internal/cli/framework"
	cliserve "model-proxy/internal/cli/serve"
	"net/http"
)

// application is the process composition owner. It binds the concrete command
// handlers and the serve runtime once; main only supplies process arguments,
// streams, and the final exit boundary.
type application struct {
	serve    serveAssembly
	commands map[string]cliCommand
}

func newApplication() *application {
	app := &application{}
	app.commands = map[string]cliCommand{
		"serve":    processCLICommand(app.serve.command),
		"takeover": processCLICommand(cmdTakeover),
		"restore":  processCLICommand(cmdRestore),
		"login":    processCLICommand(cmdLogin),
		"logout":   processCLICommand(cmdLogout),
		"usage":    processCLICommand(cmdUsage),
		"models":   processCLICommand(cmdModels),
		"config":   processCLICommand(cmdConfig),
		"schedule": processCLICommand(cmdSchedule),
		"pin":      processCLICommand(cmdPin),
		"unpin":    processCLICommand(cmdUnpin),
		"unfreeze": processCLICommand(cmdUnfreeze),
		"stats":    processCLICommand(cmdStats),
		"doctor":   processCLICommand(cmdDoctor),
		"test":     processCLICommand(cmdTest),
		"replay":   processCLICommand(cmdReplay),
		"shadow":   processCLICommand(cmdShadow),
		"wire":     processCLICommand(cmdWire),
	}
	return app
}

func (app *application) Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return runCLIArgsWithCommands(args, stdin, stdout, stderr, app.commands)
}

// serveAssembly owns the process-level serve/daemon routing and foreground
// server lifecycle. Its concrete runtime is assembled only after config and
// process logging have been prepared.
type serveAssembly struct{}

// applicationRuntime is the concrete application runtime assembled around one
// Proxy generation. It owns the Proxy lifecycle boundary, HTTP mux/Web wiring,
// reload projection, and transport tasks handed to the process server.
type applicationRuntime struct {
	configPath     string
	startupConfig  *Config // immutable listen/startup-log view; reload state lives in Proxy
	proxy          *Proxy
	handler        http.Handler
	transportTasks []cliserve.TransportTask
}

func newApplicationRuntime(cfg *Config, args cliserve.Args) *applicationRuntime {
	proxy := appdomain.NewProxy(cfg)
	// Start all process-owned optional services through the Proxy lifecycle
	// owner (stats flusher, request logger, startup catalog load).
	proxy.StartRuntimeServices(cfg)

	mux := http.NewServeMux()
	mux.HandleFunc("/", proxy.Handler)
	runtime := &applicationRuntime{
		configPath:    args.Config,
		startupConfig: cfg,
		proxy:         proxy,
		handler:       mux,
	}
	if cfg.Web.Enabled {
		web := appdomain.NewWebServer(proxy, args.Config)
		web.SetLogFile(cliserve.ResolveLogFile(args, cfg))
		web.Register(mux)
		runtime.transportTasks = append(runtime.transportTasks, func(stop <-chan struct{}) {
			if !web.Start() {
				return
			}
			<-stop
			web.Close()
		})
	}
	return runtime
}

func (runtime *applicationRuntime) reload() {
	log.Printf("[reload] SIGHUP received, reloading config from %s", runtime.configPath)
	if err := runtime.proxy.Reload(runtime.configPath); err != nil {
		var applied *reloadAppliedWarning
		if errors.As(err, &applied) {
			log.Printf("[reload] WARNING: %v", err)
		} else {
			log.Printf("[reload] FAILED: %v (keeping old config)", err)
		}
		return
	}
	snapshot := runtime.proxy.SnapshotRuntime()
	log.Printf("[reload] config reloaded successfully (providers: %s, routes: %s)",
		cliframework.ProviderNames(snapshot.Cfg), cliframework.RouteNames(snapshot.Cfg))
}

func (runtime *applicationRuntime) Close() {
	runtime.proxy.Close()
}
