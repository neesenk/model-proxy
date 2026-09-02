package config

import (
	"os"
	"path/filepath"

	"model-proxy/internal/catalog"
)

// ModelsCatalogEndpoint resolves the models.dev source endpoint. Environment
// interpretation (MP_MODELSDEV_URL) stays in the config domain, mirroring
// MP_PRICING_URL; internal/catalog only receives the resolved endpoint.
func ModelsCatalogEndpoint() string {
	if endpoint := os.Getenv("MP_MODELSDEV_URL"); endpoint != "" {
		return endpoint
	}
	return catalog.DefaultEndpoint
}

// ModelsCatalogPath resolves the config-owned cache location.
func ModelsCatalogPath(homeDir string) string {
	return filepath.Join(homeDir, ".model-proxy", "models_cache.json")
}

// LoadModelsCatalog applies model-proxy's endpoint, cache, TTL, fetcher, and
// warning policy to the repository-leaf catalog package.
func LoadModelsCatalog(homeDir string, force bool) (*catalog.Catalog, error) {
	return catalog.EnsureFresh(catalog.RefreshOptions{
		CacheFile: ModelsCatalogPath(homeDir),
		Endpoint:  ModelsCatalogEndpoint(),
		Fetch:     catalog.FetchHTTP,
		Force:     force,
		TTL:       catalog.DefaultTTL,
		Warnings:  os.Stderr,
	})
}
