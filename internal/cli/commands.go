// commands.go owns the process-level CLI entries: each loads config (when
// needed) and delegates to the command implementation. The composition root
// (main) only registers these by name.
package cli

import (
	"fmt"
	"io"
	"log"
	"model-proxy/internal/app"
	clidoctor "model-proxy/internal/cli/doctor"
	"model-proxy/internal/takeover"
	"os"

	cliframework "model-proxy/internal/cli/framework"
	climodels "model-proxy/internal/cli/models"
	configdomain "model-proxy/internal/config"
)

// LoadCmdConfig loads the CLI config or exits.
func LoadCmdConfig(args []string) *configdomain.Config {
	cfg, err := configdomain.LoadConfig(cliframework.ConfigPath(args))
	if err != nil {
		log.Fatal(err)
	}
	return cfg
}

func RunStats(args []string) {
	cfg := LoadCmdConfig(args)
	os.Exit(CmdStats(args, cfg.Listen, os.Stdout, os.Stderr))
}

func RunModels(args []string) {
	cfg := LoadCmdConfig(args)
	climodels.CmdModels(args, cfg, cliframework.ConfigPath(args))
}

func RunUsage(args []string)  { CmdUsage(args, LoadCmdConfig(args)) }
func RunLogout(args []string) { CmdLogout(args, LoadCmdConfig(args)) }

func RunConfig(args []string)        { CmdConfigRun(args) }
func RunSchedule(args []string)      { CmdSchedule(args, LoadCmdConfig(args)) }
func RunPin(args []string)           { CmdPin(args, LoadCmdConfig(args)) }
func RunUnpin(args []string)         { CmdUnpin(args, LoadCmdConfig(args)) }
func RunUnfreeze(args []string)      { CmdUnfreeze(args, LoadCmdConfig(args)) }
func RunReplay(args []string)        { CmdReplay(args, LoadCmdConfig(args)) }
func RunShadow(args []string)        { CmdShadow(args, LoadCmdConfig(args)) }
func RunShadowReport(args []string)  { CmdShadowReport(args, LoadCmdConfig(args)) }
func RunWire(args []string)          { CmdWire(args, LoadCmdConfig(args)) }
func RunWireRecordCLI(args []string) { CmdWireRecord(args, LoadCmdConfig(args)) }
func RunServeStatus(args []string)   { CmdServeStatus(args, LoadCmdConfig(args)) }

// Command is the process-level adapter for an existing command handler. The
// stream parameters make the front-door contract explicit.
type Command func(args []string, stdin io.Reader, stdout, stderr io.Writer) int

// ProcessCommand adapts a plain handler to the Command contract.
func ProcessCommand(run func([]string)) Command {
	return func(args []string, _ io.Reader, _, _ io.Writer) int {
		run(args)
		return 0
	}
}

// RunArgsWithCommands is the CLI dispatch loop: help handling, then the named
// command. Neither layer reads process globals.
func RunArgsWithCommands(
	args []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
	commands map[string]Command,
) int {
	if len(args) == 0 {
		_, _ = io.WriteString(stdout, Usage)
		return 1
	}

	cmd := args[0]
	if cmd == "-h" || cmd == "--help" || cmd == "help" {
		_, _ = io.WriteString(stdout, Usage)
		return 0
	}

	if help, ok := Help[cmd]; ok && HasHelpFlag(args[1:]) {
		_, _ = fmt.Fprintln(stdout, help)
		if TakesProvider(cmd) {
			PrintConfigProvidersTo(stdout, args[1:])
		}
		return 0
	}

	run, ok := commands[cmd]
	if !ok {
		_, _ = fmt.Fprintf(stderr, "unknown command: %s\n\n", cmd)
		_, _ = io.WriteString(stdout, Usage)
		return 1
	}
	return run(args[1:], stdin, stdout, stderr)
}

// HasHelpFlag reports whether args include -h/--help.
func HasHelpFlag(args []string) bool {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			return true
		}
	}
	return false
}

// RunDoctor runs the offline/live scheduling diagnostic.
func RunDoctor(args []string) {
	cfg, err := configdomain.LoadConfig(cliframework.ConfigPath(args))
	if err != nil {
		fmt.Println("✗ config invalid: " + err.Error())
		os.Exit(1)
	}
	clidoctor.CmdDoctor(args, cfg, cliframework.ConfigPath(args))
}

// RunTakeover rewrites a client config to point at the proxy.
func RunTakeover(args []string) {
	cfg := LoadCmdConfig(args)
	which := cliframework.Positional(args)
	if err := takeover.RunTakeover(cfg, which, takeover.BackupDir(cliframework.ConfigPath(args)), takeoverFacts(cfg, which)); err != nil {
		log.Fatal(err)
	}
}

// RunRestore restores a client config from its takeover backup.
func RunRestore(args []string) {
	cfg := LoadCmdConfig(args)
	which := cliframework.Positional(args)
	if err := takeover.RunRestore(cfg, which, takeover.BackupDir(cliframework.ConfigPath(args))); err != nil {
		log.Fatal(err)
	}
}

// takeoverFacts computes the application-owned implicit routes and (only when a
// metadata-writing client is selected) hydrated models.dev metadata for the
// takeover package.
func takeoverFacts(cfg *configdomain.Config, which string) takeover.ModelFacts {
	implicit, _ := app.SynthesizeImplicitRoutes(cfg, app.AccountStore())
	facts := takeover.ModelFacts{
		Implicit:      implicit,
		SourceDefault: -1,
	}
	if takeover.WritesMetadata(takeover.ListClients(cfg, which)) {
		cat, _ := app.LoadModelsCatalog(cliframework.HomeDir(), false)
		meta, sources := app.HydrateModels(cfg, cat)
		facts.Meta = meta
		facts.Sources = make(map[string]map[string]int, len(sources))
		for provider, models := range sources {
			facts.Sources[provider] = make(map[string]int, len(models))
			for model, source := range models {
				facts.Sources[provider][model] = int(source)
			}
		}
		facts.SourceDefault = int(app.SrcDefault)
		facts.DefaultContext = app.DefaultModelMetadata.Context
		facts.DefaultOutput = app.DefaultModelMetadata.Output
	}
	return facts
}
