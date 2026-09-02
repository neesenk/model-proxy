package config

import (
	"path/filepath"
	"testing"

	"model-proxy/internal/catalog"
)

// TestModelsCatalogEndpoint_EnvOverride keeps the config-owned environment
// policy contract separate from the catalog package's fetch/cache behavior.
func TestModelsCatalogEndpoint_EnvOverride(t *testing.T) {
	t.Setenv("MP_MODELSDEV_URL", "http://example.test/api.json")
	if got := ModelsCatalogEndpoint(); got != "http://example.test/api.json" {
		t.Errorf("env override ignored: got %q", got)
	}
}

func TestModelsCatalogDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("MP_MODELSDEV_URL", "")
	if got := ModelsCatalogEndpoint(); got != catalog.DefaultEndpoint {
		t.Errorf("default endpoint = %q, want %q", got, catalog.DefaultEndpoint)
	}
	if got, want := ModelsCatalogPath(home), filepath.Join(home, ".model-proxy", "models_cache.json"); got != want {
		t.Errorf("cache path = %q, want %q", got, want)
	}
}
