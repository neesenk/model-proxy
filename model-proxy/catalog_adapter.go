package main

import (
	"os"
	"path/filepath"

	"model-proxy/internal/catalog"
)

// modelSource records where a model's effective runtime metadata came from.
// Config carries model names only, so metadata is either catalog-backed or the
// conservative fallback used by takeover/client config generation.
type modelSource int

const (
	srcModelsDev modelSource = iota
	srcDefault
)

var defaultModelMetadata = catalog.Model{
	Context:    200000,
	Output:     16384,
	Modalities: catalog.Modalities{Input: []string{"text"}, Output: []string{"text"}},
}

// modelsCatalogEndpoint is the application adapter for the models.dev source.
// Environment interpretation stays in the composition root; internal/catalog
// only receives the resolved endpoint.
func modelsCatalogEndpoint() string {
	if endpoint := envOrEmpty("MP_MODELSDEV_URL"); endpoint != "" {
		return endpoint
	}
	return catalog.DefaultEndpoint
}

// modelsCatalogPath resolves the application-owned cache location.
func modelsCatalogPath() string {
	return filepath.Join(homeDir(), ".model-proxy", "models_cache.json")
}

// loadModelsCatalog applies model-proxy's endpoint, cache, TTL, fetcher, and
// warning policy to the repository-leaf catalog package.
func loadModelsCatalog(force bool) (*catalog.Catalog, error) {
	return catalog.EnsureFresh(catalog.RefreshOptions{
		CacheFile: modelsCatalogPath(),
		Endpoint:  modelsCatalogEndpoint(),
		Fetch:     catalog.FetchHTTP,
		Force:     force,
		TTL:       catalog.DefaultTTL,
		Warnings:  os.Stderr,
	})
}

// hydrateModels computes effective metadata for the union of configured model
// names and models referenced by routes. The neutral catalog owns metadata
// lookup; this root adapter owns Config traversal and fallback/source policy.
// cfg and its maps are never mutated.
func hydrateModels(cfg *Config, cat *catalog.Catalog) (meta map[string]map[string]catalog.Model, sources map[string]map[string]modelSource) {
	meta = map[string]map[string]catalog.Model{}
	sources = map[string]map[string]modelSource{}

	ensure := func(providerName string) (map[string]catalog.Model, map[string]modelSource) {
		if meta[providerName] == nil {
			meta[providerName] = map[string]catalog.Model{}
			sources[providerName] = map[string]modelSource{}
		}
		return meta[providerName], sources[providerName]
	}
	resolve := func(modelName string) (catalog.Model, modelSource) {
		if model, ok := cat.Lookup(modelName); ok {
			return model, srcModelsDev
		}
		return defaultModelMetadata, srcDefault
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
