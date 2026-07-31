package pricing

import "path/filepath"

// CachePath is the on-disk catalog cache location.
func CachePath(homeDir string) string {
	return filepath.Join(homeDir, ".model-proxy", "pricing_cache.json")
}
