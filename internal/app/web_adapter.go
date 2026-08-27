package app

import (
	clilogin "model-proxy/internal/cli/login"
	"net/http"

	observeevents "model-proxy/internal/observe/events"
	webtransport "model-proxy/internal/web"
	"model-proxy/internal/webauth"
)

// webServer is the composition adapter around the transport-owned Web server.
// Mutable fields are retained only as construction/test seams; HTTP routing,
// sessions, assets, and background-task ownership live in internal/web.
type WebServer struct {
	server *webtransport.Server
	api    *proxyWebAPI
	// adminAuth binds the proxy's S2 admin-auth source (reload-swapped); a
	// closure, not a *Proxy retention, keeps the root adapter composition-only.
	adminAuth  func() *webauth.Source
	configFile string
	logFile    string
	events     http.HandlerFunc

	newAqpClientFn  func(storePath string) *clilogin.AqpClient
	newCodexOptions func() *clilogin.CodexLoginServerOptions
}

func NewWebServer(proxy *Proxy, configFile string) *WebServer {
	server := &WebServer{
		adminAuth:       proxy.adminAuth.Load,
		configFile:      configFile,
		newAqpClientFn:  clilogin.NewAqpClient,
		newCodexOptions: defaultCodexLoginOptions,
	}
	server.api = newProxyWebAPI(proxy, func() string {
		return server.configFile
	})
	// Keep test endpoint overrides dynamic: tests replace these hooks after
	// construction, while the application adapter reads them at login start.
	server.api.newAqpClientFn = func(path string) *clilogin.AqpClient {
		return server.newAqpClientFn(path)
	}
	server.api.newCodexOptions = func() *clilogin.CodexLoginServerOptions {
		return server.newCodexOptions()
	}
	// The live SSE stream is served by the web transport's /api/ subtree; the
	// hub itself stays owned by the Proxy (snapshot/event producers publish
	// there), so only the handler is injected. See webtransport.Options.Events.
	server.events = func(w http.ResponseWriter, r *http.Request) {
		observeevents.ServeEvents(proxy.events, w, r)
	}
	server.server = mustNewWebTransport(server)
	return server
}

func mustNewWebTransport(server *WebServer) *webtransport.Server {
	transport, err := webtransport.New(webtransport.Options{
		Reads:    server.api,
		Commands: server.api,
		Version:  Version,
		Events:   server.events,
		// S2 admin-surface auth follows the proxy's config generation (the
		// transport is built once; the closure picks up reload-swapped sources).
		AdminAuth: server.adminAuth,
		LogFile: func() string {
			return server.logFile
		},
	})
	if err != nil {
		panic("construct Web transport: " + err.Error())
	}
	return transport
}

func (server *WebServer) Register(mux *http.ServeMux) {
	server.server.Register(mux)
}

func (server *WebServer) Start() bool {
	return server.server.Start()
}

func (server *WebServer) Close() {
	server.server.Close()
}

func (server *WebServer) Serve(response http.ResponseWriter, request *http.Request) {
	server.server.ServeHTTP(response, request)
}

// SetLogFile records the daemon log path shown in the UI.
func (server *WebServer) SetLogFile(path string) { server.logFile = path }

// defaultCodexLoginOptions wires production codex OAuth endpoints.
func defaultCodexLoginOptions() *clilogin.CodexLoginServerOptions {
	options := &clilogin.CodexLoginServerOptions{}
	options.Defaults()
	return options
}
