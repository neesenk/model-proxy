package app

import (
	clilogin "model-proxy/internal/cli/login"
	"net/http"

	webtransport "model-proxy/internal/web"
)

// webServer is the composition adapter around the transport-owned Web server.
// Mutable fields are retained only as construction/test seams; HTTP routing,
// sessions, assets, and background-task ownership live in internal/web.
type WebServer struct {
	server *webtransport.Server
	api    *proxyWebAPI

	configFile string
	logFile    string

	newAqpClientFn  func(storePath string) *clilogin.AqpClient
	newCodexOptions func() *clilogin.CodexLoginServerOptions
}

func NewWebServer(proxy *Proxy, configFile string) *WebServer {
	server := &WebServer{
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
	server.server = mustNewWebTransport(server)
	return server
}

func mustNewWebTransport(server *WebServer) *webtransport.Server {
	transport, err := webtransport.New(webtransport.Options{
		Reads:    server.api,
		Commands: server.api,
		Version:  Version,
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
