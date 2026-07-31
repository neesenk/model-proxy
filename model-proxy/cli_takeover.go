package main

import (
	"log"
	cliframework "model-proxy/internal/cli/framework"

	"model-proxy/internal/takeover"
)

func cmdTakeover(args []string) {
	cp := cliframework.ConfigPath(args)
	cfg, err := LoadConfig(cp)
	if err != nil {
		log.Fatal(err)
	}
	which := cliframework.Positional(args)
	if err := takeover.RunTakeover(cfg, which, takeover.BackupDir(cp), takeoverFacts(cfg, which)); err != nil {
		log.Fatal(err)
	}
}

func cmdRestore(args []string) {
	cp := cliframework.ConfigPath(args)
	cfg, err := LoadConfig(cp)
	if err != nil {
		log.Fatal(err)
	}
	which := cliframework.Positional(args)
	if err := takeover.RunRestore(cfg, which, takeover.BackupDir(cp)); err != nil {
		log.Fatal(err)
	}
}

// takeoverFacts computes the application-owned implicit routes and (only when a
// metadata-writing client is selected) hydrated models.dev metadata for the
// takeover package. Catalog loading and source markers stay in the root.
func takeoverFacts(cfg *Config, which string) takeover.ModelFacts {
	// Implicit routes (auto-derived from logged-in providers' model lists) are
	// callable through the proxy and listed in /v1/models — include them so the
	// takeover client config matches. Cheap (loadPool file I/O, no network).
	implicit, _ := synthesizeImplicitRoutes(cfg)
	facts := takeover.ModelFacts{
		Implicit:      implicit,
		SourceDefault: -1,
	}
	if takeover.WritesMetadata(takeover.ListClients(cfg, which)) {
		cat, _ := loadModelsCatalog(false)
		meta, sources := hydrateModels(cfg, cat)
		facts.Meta = meta
		facts.Sources = make(map[string]map[string]int, len(sources))
		for provider, models := range sources {
			facts.Sources[provider] = make(map[string]int, len(models))
			for model, source := range models {
				facts.Sources[provider][model] = int(source)
			}
		}
		facts.SourceDefault = int(srcDefault)
		facts.DefaultContext = defaultModelMetadata.Context
		facts.DefaultOutput = defaultModelMetadata.Output
	}
	return facts
}
