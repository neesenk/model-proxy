package cli

import (
	"io"

	cliaccount "model-proxy/internal/cli/account"
	cliadmin "model-proxy/internal/cli/admin"
	cliaudit "model-proxy/internal/cli/audit"
	cliconfig "model-proxy/internal/cli/config"
	clidiag "model-proxy/internal/cli/diag"
	clidoctor "model-proxy/internal/cli/doctor"
	clilogin "model-proxy/internal/cli/login"
	climodels "model-proxy/internal/cli/models"
	clipresets "model-proxy/internal/cli/presets"
	clistats "model-proxy/internal/cli/stats"
	clistatus "model-proxy/internal/cli/status"
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
		"add":      ProcessCommand(clipresets.RunAdd),
		"presets":  ProcessCommand(clipresets.RunPresets),
		"logout":   ProcessCommand(cliaccount.RunLogout),
		"usage":    ProcessCommand(clistats.RunUsage),
		"models":   ProcessCommand(climodels.RunModels),
		"config":   ProcessCommand(cliconfig.CmdConfigRun),
		"routes":   ProcessCommand(clistatus.RunRoutes),
		"schedule": ProcessCommand(clistatus.RunSchedule),
		"pin":      ProcessCommand(cliadmin.RunPin),
		"unpin":    ProcessCommand(cliadmin.RunUnpin),
		"unfreeze": ProcessCommand(cliadmin.RunUnfreeze),
		"freeze":   ProcessCommand(cliadmin.RunFreeze),
		"stats":    ProcessCommand(clistats.RunStats),
		"cache":    ProcessCommand(clistatus.RunCache),
		"doctor":   ProcessCommand(clidoctor.RunDoctor),
		"audit":    ProcessCommand(cliaudit.RunAudit),
		"test":     ProcessCommand(climodels.CmdTest),
		"replay":   ProcessCommand(clidiag.RunReplay),
		"shadow":   ProcessCommand(clidiag.RunShadow),
		"wire":     ProcessCommand(clidiag.RunWire),
	}
	return app
}

// Run dispatches args through the registered commands.
func (app *Application) Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return RunArgsWithCommands(args, stdin, stdout, stderr, app.Commands)
}
