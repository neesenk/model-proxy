package main

import (
	"fmt"
	"log"
	"model-proxy/internal/app"
	"os"

	clicmd "model-proxy/internal/cli"
	clidoctor "model-proxy/internal/cli/doctor"
	cliframework "model-proxy/internal/cli/framework"
	"model-proxy/internal/takeover"
)

// Command wrappers delegate to internal/cli Run* entries (config loading lives
// there). Doctor/takeover/restore stay root-side because they cross into
// doctor/app packages that import internal/cli.
func cmdStats(args []string)          { clicmd.RunStats(args) }
func cmdModels(args []string)         { clicmd.RunModels(args) }
func cmdUsage(args []string)          { clicmd.RunUsage(args) }
func cmdLogout(args []string)         { clicmd.RunLogout(args) }
func cmdConfig(args []string)         { clicmd.RunConfig(args) }
func cmdSchedule(args []string)       { clicmd.RunSchedule(args) }
func cmdPin(args []string)            { clicmd.RunPin(args) }
func cmdUnpin(args []string)          { clicmd.RunUnpin(args) }
func cmdUnfreeze(args []string)       { clicmd.RunUnfreeze(args) }
func cmdReplay(args []string)         { clicmd.RunReplay(args) }
func cmdShadow(args []string)         { clicmd.RunShadow(args) }
func cmdShadowReport(args []string)   { clicmd.RunShadowReport(args) }
func cmdWire(args []string)           { clicmd.RunWire(args) }
func cmdWireRecord(args []string)     { clicmd.RunWireRecordCLI(args) }
func cmdServeStatusCLI(args []string) { clicmd.RunServeStatus(args) }

func cmdDoctor(args []string) {
	cfg, err := LoadConfig(cliframework.ConfigPath(args))
	if err != nil {
		fmt.Println("✗ config invalid: " + err.Error())
		os.Exit(1)
	}
	clidoctor.CmdDoctor(args, cfg, cliframework.ConfigPath(args))
}

func cmdTakeover(args []string) {
	cfg := loadCmdConfig(args)
	which := cliframework.Positional(args)
	if err := takeover.RunTakeover(cfg, which, takeover.BackupDir(cliframework.ConfigPath(args)), takeoverFacts(cfg, which)); err != nil {
		log.Fatal(err)
	}
}

func cmdRestore(args []string) {
	cfg := loadCmdConfig(args)
	which := cliframework.Positional(args)
	if err := takeover.RunRestore(cfg, which, takeover.BackupDir(cliframework.ConfigPath(args))); err != nil {
		log.Fatal(err)
	}
}

func loadCmdConfig(args []string) *Config {
	cfg, err := LoadConfig(cliframework.ConfigPath(args))
	if err != nil {
		log.Fatal(err)
	}
	return cfg
}

// takeoverFacts computes the application-owned implicit routes and (only when a
// metadata-writing client is selected) hydrated models.dev metadata for the
// takeover package.
func takeoverFacts(cfg *Config, which string) takeover.ModelFacts {
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
