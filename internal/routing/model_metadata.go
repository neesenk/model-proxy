package routing

import (
	"model-proxy/internal/catalog"
	configdomain "model-proxy/internal/config"
)

// ModelSource records where a model's effective runtime metadata came from.
// Config carries model names only, so metadata is either catalog-backed or the
// conservative fallback used by takeover/client config generation.
type ModelSource int

const (
	SrcModelsDev ModelSource = iota
	SrcDefault
)

var DefaultModelMetadata = catalog.Model{
	Context:    200000,
	Output:     16384,
	Modalities: catalog.Modalities{Input: []string{"text"}, Output: []string{"text"}},
}

// HydrateModels computes effective metadata for the union of configured model
// names and models referenced by routes. The neutral catalog owns metadata
// lookup; this routing policy owns Config traversal and fallback/source rules.
// cfg and its maps are never mutated.
func HydrateModels(cfg *configdomain.Config, cat *catalog.Catalog) (meta map[string]map[string]catalog.Model, sources map[string]map[string]ModelSource) {
	meta = map[string]map[string]catalog.Model{}
	sources = map[string]map[string]ModelSource{}

	ensure := func(providerName string) (map[string]catalog.Model, map[string]ModelSource) {
		if meta[providerName] == nil {
			meta[providerName] = map[string]catalog.Model{}
			sources[providerName] = map[string]ModelSource{}
		}
		return meta[providerName], sources[providerName]
	}
	resolve := func(modelName string) (catalog.Model, ModelSource) {
		if model, ok := cat.Lookup(modelName); ok {
			return model, SrcModelsDev
		}
		return DefaultModelMetadata, SrcDefault
	}

	for providerName, providerConfig := range cfg.Providers {
		models, modelSources := ensure(providerName)
		for _, modelName := range providerConfig.Models {
			if _, duplicate := models[modelName]; duplicate {
				continue
			}
			models[modelName], modelSources[modelName] = resolve(modelName)
		}
	}
	for _, targets := range cfg.Routes {
		for _, target := range targets {
			if _, exists := cfg.Providers[target.Provider]; !exists {
				continue
			}
			models, modelSources := ensure(target.Provider)
			if _, duplicate := models[target.Model]; duplicate {
				continue
			}
			models[target.Model], modelSources[target.Model] = resolve(target.Model)
		}
	}
	return meta, sources
}
