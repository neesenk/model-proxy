package main

import (
	"net/http"

	webtransport "model-proxy/internal/web"
)

// webServer is the composition adapter around the transport-owned Web server.
// Mutable fields are retained only as construction/test seams; HTTP routing,
// sessions, assets, and background-task ownership live in internal/web.
type webServer struct {
	server *webtransport.Server
	api    *proxyWebAPI

	configFile string
	logFile    string

	newAqpClientFn  func(storePath string) *AqpClient
	newCodexOptions func() *codexLoginServerOptions
}

func newWebServer(proxy *Proxy, configFile string) *webServer {
	server := &webServer{
		configFile:      configFile,
		newAqpClientFn:  newAqpClient,
		newCodexOptions: defaultCodexLoginOptions,
	}
	server.api = newProxyWebAPI(proxy, func() string {
		return server.configFile
	})
	// Keep test endpoint overrides dynamic: tests replace these hooks after
	// construction, while the application adapter reads them at login start.
	server.api.newAqpClientFn = func(path string) *AqpClient {
		return server.newAqpClientFn(path)
	}
	server.api.newCodexOptions = func() *codexLoginServerOptions {
		return server.newCodexOptions()
	}
	server.server = mustNewWebTransport(server)
	return server
}

func defaultCodexLoginOptions() *codexLoginServerOptions {
	options := &codexLoginServerOptions{}
	options.defaults()
	return options
}

func mustNewWebTransport(server *webServer) *webtransport.Server {
	transport, err := webtransport.New(webtransport.Options{
		Reads:    server.api,
		Commands: server.api,
		Version:  version,
		LogFile: func() string {
			return server.logFile
		},
	})
	if err != nil {
		panic("construct Web transport: " + err.Error())
	}
	return transport
}

func (server *webServer) register(mux *http.ServeMux) {
	server.server.Register(mux)
}

func (server *webServer) start() bool {
	return server.server.Start()
}

func (server *webServer) close() {
	server.server.Close()
}

func (server *webServer) serve(response http.ResponseWriter, request *http.Request) {
	server.server.ServeHTTP(response, request)
}
