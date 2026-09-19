package takeover

import (
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/routing"
)

// ModelFactsFor computes the route table (derived from provider model lists,
// explicit routes overriding) and hydrated models.dev metadata for
// RunTakeover. homeDir locates
// the models.dev catalog cache (callers pass the CLI's HOME seam so tests can
// isolate it).
func ModelFactsFor(cfg *configdomain.Config, which, homeDir, templatesDir string, mode ResolveMode) ModelFacts {
	facts := ModelFacts{
		Routes:        routing.RouteTable(cfg),
		SourceDefault: -1,
	}
	// Metadata is hydrated unconditionally, not just for metadata-writing
	// clients (opencode/pi/kimi): the single-model default placeholders
	// ({{model.primary}}/{{model.fast}}, e.g. claude's default-model envs)
	// also rank models by context window. A missing catalog cache just
	// degrades those to name order; MetadataWarnings keeps its own
	// WritesMetadata gate, so warnings are unaffected.
	cat, _ := configdomain.LoadModelsCatalog(homeDir, false)
	meta, sources := routing.HydrateModels(cfg, cat)
	facts.Meta = meta
	facts.Sources = make(map[string]map[string]int, len(sources))
	for provider, models := range sources {
		facts.Sources[provider] = make(map[string]int, len(models))
		for model, source := range models {
			facts.Sources[provider][model] = int(source)
		}
	}
	facts.SourceDefault = int(routing.SrcDefault)
	facts.DefaultContext = routing.DefaultModelMetadata.Context
	facts.DefaultOutput = routing.DefaultModelMetadata.Output
	return facts
}
