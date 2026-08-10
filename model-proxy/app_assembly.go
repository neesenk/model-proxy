package main

import (
	"io"
	appdomain "model-proxy/internal/app"
	cliserve "model-proxy/internal/cli/serve"
)

// serveAssembly owns the process-level serve/daemon routing and foreground
// server lifecycle. Its concrete runtime is assembled only after config and
// process logging have been prepared.
type serveAssembly struct{}

// applicationRuntime aliases the app-owned runtime.
type applicationRuntime = appdomain.Runtime

func newApplicationRuntime(cfg *Config, args cliserve.Args) *applicationRuntime {
	return appdomain.NewRuntime(cfg, args)
}

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
