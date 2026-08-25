package cli

import (
	"io"

	clilogin "model-proxy/internal/cli/login"
	climodels "model-proxy/internal/cli/models"
)

// Application is the CLI process composition owner: it binds the concrete
// command handlers once; main only supplies process arguments, streams, and
// the final exit boundary.
type Application struct {
	Serve    func(args []string)
	Commands map[string]Command
}

// NewApplication builds the CLI application. serve is the process-level serve
// driver (it owns signal handling and the listener, so it stays caller-owned).
func NewApplication(serve func(args []string)) *Application {
	app := &Application{Serve: serve}
	app.Commands = map[string]Command{
		"serve":    ProcessCommand(app.Serve),
		"takeover": ProcessCommand(RunTakeover),
		"restore":  ProcessCommand(RunRestore),
		"login":    ProcessCommand(clilogin.CmdLogin),
		"add":      ProcessCommand(RunAdd),
		"presets":  ProcessCommand(RunPresets),
		"logout":   ProcessCommand(RunLogout),
		"usage":    ProcessCommand(RunUsage),
		"models":   ProcessCommand(RunModels),
		"config":   ProcessCommand(RunConfig),
		"schedule": ProcessCommand(RunSchedule),
		"pin":      ProcessCommand(RunPin),
		"unpin":    ProcessCommand(RunUnpin),
		"unfreeze": ProcessCommand(RunUnfreeze),
		"stats":    ProcessCommand(RunStats),
		"doctor":   ProcessCommand(RunDoctor),
		"audit":    ProcessCommand(RunAudit),
		"test":     ProcessCommand(climodels.CmdTest),
		"replay":   ProcessCommand(RunReplay),
		"shadow":   ProcessCommand(RunShadow),
		"wire":     ProcessCommand(RunWire),
	}
	return app
}

// Run dispatches args through the registered commands.
func (app *Application) Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return RunArgsWithCommands(args, stdin, stdout, stderr, app.Commands)
}
