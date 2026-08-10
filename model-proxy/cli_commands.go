package main

import (
	"fmt"
	"log"
	"model-proxy/internal/app"
	"os"

	clicmd "model-proxy/internal/cli"
	clidoctor "model-proxy/internal/cli/doctor"
	cliframework "model-proxy/internal/cli/framework"
	climodels "model-proxy/internal/cli/models"
	"model-proxy/internal/takeover"
)

// Command wrappers: each loads config (when needed) and delegates to the
// migrated internal/cli/* implementation. Kept in one file because the
// per-command files became pure forwarding shells after migration.

func loadCmdConfig(args []string) *Config {
	cfg, err := LoadConfig(cliframework.ConfigPath(args))
	if err != nil {
		log.Fatal(err)
	}
	return cfg
}

func cmdStats(args []string) {
	cfg := loadCmdConfig(args)
	os.Exit(clicmd.CmdStats(args, cfg.Listen, os.Stdout, os.Stderr))
}

func cmdModels(args []string) {
	cfg := loadCmdConfig(args)
	climodels.CmdModels(args, cfg, cliframework.ConfigPath(args))
}

func cmdUsage(args []string) {
	cfg := loadCmdConfig(args)
	clicmd.CmdUsage(args, cfg)
}

func cmdLogout(args []string) {
	cfg := loadCmdConfig(args)
	clicmd.CmdLogout(args, cfg)
}

func cmdDoctor(args []string) {
	cfg, err := LoadConfig(cliframework.ConfigPath(args))
	if err != nil {
		fmt.Println("✗ config invalid: " + err.Error())
		os.Exit(1)
	}
	clidoctor.CmdDoctor(args, cfg, cliframework.ConfigPath(args))
}

func cmdConfig(args []string) {
	clicmd.CmdConfigRun(args)
}

func cmdSchedule(args []string) {
	cfg := loadCmdConfig(args)
	clicmd.CmdSchedule(args, cfg)
}

func cmdPin(args []string) {
	cfg := loadCmdConfig(args)
	clicmd.CmdPin(args, cfg)
}

func cmdUnpin(args []string) {
	cfg := loadCmdConfig(args)
	clicmd.CmdUnpin(args, cfg)
}

func cmdUnfreeze(args []string) {
	cfg := loadCmdConfig(args)
	clicmd.CmdUnfreeze(args, cfg)
}

func cmdReplay(args []string) {
	cfg := loadCmdConfig(args)
	clicmd.CmdReplay(args, cfg)
}

func cmdShadow(args []string) {
	cfg := loadCmdConfig(args)
	clicmd.CmdShadow(args, cfg)
}

func cmdShadowReport(args []string) {
	cfg := loadCmdConfig(args)
	clicmd.CmdShadowReport(args, cfg)
}

func cmdWire(args []string) {
	cfg := loadCmdConfig(args)
	clicmd.CmdWire(args, cfg)
}

func cmdWireRecord(args []string) {
	cfg := loadCmdConfig(args)
	clicmd.CmdWireRecord(args, cfg)
}

func cmdServeStatusCLI(args []string) {
	cfg := loadCmdConfig(args)
	clicmd.CmdServeStatus(args, cfg)
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

// takeoverFacts computes the application-owned implicit routes and (only when a
// metadata-writing client is selected) hydrated models.dev metadata for the
// takeover package. Catalog loading and source markers stay in the root.
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
