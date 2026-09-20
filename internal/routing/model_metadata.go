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
// lookup; this routing policy owns Config traversal and fallback/source rules —
// including the per-provider catalog_alias remap consulted before the direct
// id. cfg and its maps are never mutated.
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
	// resolve looks a model up in the catalog through the single Lookup site
	// below. lookupIDs orders the candidate ids: a provider's catalog_alias
	// remaps the lookup id (for upstream ids the catalog doesn't name); a
	// missed alias falls back to the direct id so a stale mapping degrades to
	// pre-alias behavior instead of masking a direct hit.
	resolve := func(providerConfig configdomain.Provider, modelName string) (catalog.Model, ModelSource) {
		for _, id := range lookupIDs(providerConfig, modelName) {
			if model, ok := cat.Lookup(id); ok {
				return model, SrcModelsDev
			}
		}
		return DefaultModelMetadata, SrcDefault
	}

	for providerName, providerConfig := range cfg.Providers {
		models, modelSources := ensure(providerName)
		for _, modelName := range providerConfig.Models {
			if _, duplicate := models[modelName]; duplicate {
				continue
			}
			models[modelName], modelSources[modelName] = resolve(providerConfig, modelName)
		}
	}
	for _, targets := range cfg.Routes {
		for _, target := range targets {
			providerConfig, exists := cfg.Providers[target.Provider]
			if !exists {
				continue
			}
			models, modelSources := ensure(target.Provider)
			if _, duplicate := models[target.Model]; duplicate {
				continue
			}
			models[target.Model], modelSources[target.Model] = resolve(providerConfig, target.Model)
		}
	}
	return meta, sources
}

// lookupIDs lists the catalog ids one model resolves through, in probe order:
// the provider's catalog_alias remap first (when set), then the direct id.
func lookupIDs(providerConfig configdomain.Provider, modelName string) []string {
	if alias := providerConfig.CatalogAlias[modelName]; alias != "" && alias != modelName {
		return []string{alias, modelName}
	}
	return []string{modelName}
}
