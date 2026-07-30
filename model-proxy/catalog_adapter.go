package main

import (
	"model-proxy/internal/app"
	"model-proxy/internal/catalog"
)

// modelSource records where a model's effective runtime metadata came from.
// The authoritative definition lives in internal/app; aliases keep root call
// sites compiling while they migrate.
type modelSource = app.ModelSource

const (
	srcModelsDev = app.SrcModelsDev
	srcDefault   = app.SrcDefault
)

var defaultModelMetadata = app.DefaultModelMetadata

// loadModelsCatalog delegates to internal/app with the production home dir.
func loadModelsCatalog(force bool) (*catalog.Catalog, error) {
	return app.LoadModelsCatalog(homeDir(), force)
}

// hydrateModels delegates to internal/app; it owns config traversal and
// fallback/source policy over the neutral catalog leaf.
func hydrateModels(cfg *Config, cat *catalog.Catalog) (map[string]map[string]catalog.Model, map[string]map[string]modelSource) {
	return app.HydrateModels(cfg, cat)
}
