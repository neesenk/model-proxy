// runtime.go — application runtime assembly around one Proxy generation (HTTP mux/Web wiring, reload projection, transport tasks) plus the process version string.
package app

import (
	"errors"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/observe/logx"
	"net/http"
)

// Runtime is the concrete application runtime assembled around one Proxy
// generation. It owns the Proxy lifecycle boundary, HTTP mux/Web wiring,
// reload projection, and transport tasks handed to the process server.
type Runtime struct {
	ConfigPath    string
	StartupConfig *configdomain. // immutable listen/startup-log view; reload state lives in Proxy
			Config
	Proxy          *Proxy
	Handler        http.Handler
	TransportTasks []func(stop <-chan struct{})
}

// NewRuntime assembles the runtime: one Proxy, its process-owned services, and
// the HTTP mux with the Web admin when enabled. configPath/logFile are
// resolved by the process layer (cli/serve) before assembly so the composition
// root never imports the CLI packages.
func NewRuntime(cfg *configdomain.Config, configPath, logFile string) *Runtime {
	proxy := NewProxy(cfg)
	// Start all process-owned optional services through the Proxy lifecycle
	// owner (stats flusher, request logger, startup catalog load).
	proxy.StartRuntimeServices(cfg)

	mux := http.NewServeMux()
	mux.HandleFunc("/", proxy.Handler)
	runtime := &Runtime{
		ConfigPath:    configPath,
		StartupConfig: cfg,
		Proxy:         proxy,
		Handler:       mux,
	}
	if cfg.Web.Enabled {
		web := NewWebServer(proxy, configPath)
		web.SetLogFile(logFile)
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
		snapshot.Cfg.ProviderNames(), snapshot.Cfg.RouteNames())
}

// Close releases the Proxy (final lifecycle flushes).
func (runtime *Runtime) Close() {
	runtime.Proxy.Close()
}

// Version is the process version string, set by main at boot from the
// build-time-injected root version.
var Version = "dev"
