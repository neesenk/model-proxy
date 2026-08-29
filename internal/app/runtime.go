package app

import (
	"errors"
	"model-proxy/internal/observe/logx"
	"net/http"

	cliframework "model-proxy/internal/cli/framework"
	cliserve "model-proxy/internal/cli/serve"
)

// Runtime is the concrete application runtime assembled around one Proxy
// generation. It owns the Proxy lifecycle boundary, HTTP mux/Web wiring,
// reload projection, and transport tasks handed to the process server.
type Runtime struct {
	ConfigPath     string
	StartupConfig  *Config // immutable listen/startup-log view; reload state lives in Proxy
	Proxy          *Proxy
	Handler        http.Handler
	TransportTasks []cliserve.TransportTask
}

// NewRuntime assembles the runtime: one Proxy, its process-owned services, and
// the HTTP mux with the Web admin when enabled.
func NewRuntime(cfg *Config, args cliserve.Args) *Runtime {
	proxy := NewProxy(cfg)
	// Start all process-owned optional services through the Proxy lifecycle
	// owner (stats flusher, request logger, startup catalog load).
	proxy.StartRuntimeServices(cfg)

	mux := http.NewServeMux()
	mux.HandleFunc("/", proxy.Handler)
	runtime := &Runtime{
		ConfigPath:    args.Config,
		StartupConfig: cfg,
		Proxy:         proxy,
		Handler:       mux,
	}
	if cfg.Web.Enabled {
		web := NewWebServer(proxy, args.Config)
		web.SetLogFile(cliserve.ResolveLogFile(args, cfg))
		web.Register(mux)
		runtime.TransportTasks = append(runtime.TransportTasks, func(stop <-chan struct{}) {
			if !web.Start() {
				return
			}
			<-stop
			web.Close()
		})
	}
	return runtime
}

// Reload applies a SIGHUP config reload and logs the outcome.
func (runtime *Runtime) Reload() {
	logx.Infof("[reload] SIGHUP received, reloading config from %s", runtime.ConfigPath)
	if err := runtime.Proxy.Reload(runtime.ConfigPath); err != nil {
		var applied *ReloadAppliedWarning
		if errors.As(err, &applied) {
			logx.Warnf("[reload] WARNING: %v", err)
		} else {
			logx.Warnf("[reload] FAILED: %v (keeping old config)", err)
		}
		return
	}
	snapshot := runtime.Proxy.SnapshotRuntime()
	logx.Infof("[reload] config reloaded successfully (providers: %s, routes: %s)",
		cliframework.ProviderNames(snapshot.Cfg), cliframework.RouteNames(snapshot.Cfg))
}

// Close releases the Proxy (final lifecycle flushes).
func (runtime *Runtime) Close() {
	runtime.Proxy.Close()
}
