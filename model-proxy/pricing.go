package main

import "path/filepath"

// pricingCachePath is the on-disk catalog cache location.
func pricingCachePath() string {
	return filepath.Join(homeDir(), ".model-proxy", "pricing_cache.json")
}
