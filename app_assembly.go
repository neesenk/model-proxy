package main

import (
	appdomain "model-proxy/internal/app"
	clicmd "model-proxy/internal/cli"
	cliserve "model-proxy/internal/cli/serve"
	configdomain "model-proxy/internal/config"
)

// serveAssembly owns the process-level serve/daemon routing and foreground
// server lifecycle. Its concrete runtime is assembled only after config and
// process logging have been prepared.
type serveAssembly struct{}

// applicationRuntime aliases the app-owned runtime.
type applicationRuntime = appdomain.Runtime

func newApplicationRuntime(cfg *configdomain.Config, args cliserve.Args) *applicationRuntime {
	return appdomain.NewRuntime(cfg, args)
}

// application is the process composition owner; the command registry lives in
// internal/cli (newApplication keeps the root serve driver injected).
type application = clicmd.Application

func newApplication() *application {
	// Forward the build-time -ldflags version stamp (main.version) to the app
	// package so /api/status and `serve status` report the release version
	// instead of the package default "dev".
	appdomain.Version = version
	return clicmd.NewApplication(serveAssembly{}.command)
}
