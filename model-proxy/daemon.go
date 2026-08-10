package main

import (
	"net"
	"net/http"
	"os"
	"time"

	cliserve "model-proxy/internal/cli/serve"
)

// Process drain contract lives in internal/cli/serve; these aliases keep root
// assembly call sites stable while the composition root migrates.
const gracefulShutdownTimeout = cliserve.GracefulShutdownTimeout

type transportTask = cliserve.TransportTask

func runReloadLoop(stop <-chan struct{}, hup <-chan os.Signal, reload func()) {
	cliserve.RunReloadLoop(stop, hup, reload)
}

func serveHTTPUntilShutdown(
	server *http.Server,
	listener net.Listener,
	shutdown <-chan struct{},
	timeout time.Duration,
	tasks []transportTask,
	closeProxy func(),
) error {
	return cliserve.ServeHTTPUntilShutdown(server, listener, shutdown, timeout, tasks, closeProxy)
}
