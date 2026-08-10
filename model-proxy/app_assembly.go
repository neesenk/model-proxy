package main

import (
	"io"
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
	return clicmd.NewApplication(serveAssembly{}.command)
}

// runCLIArgs is the compatibility entry used by tests and embedders that still
// call the pre-composition boundary.
func runCLIArgs(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return newApplication().Run(args, stdin, stdout, stderr)
}
