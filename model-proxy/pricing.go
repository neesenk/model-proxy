package main

import (
	"path/filepath"

	"model-proxy/internal/pricing"
)

// pricingEndpoint returns the catalog endpoint, overridable via MP_PRICING_URL.
func pricingEndpoint() string {
	if v := envOrEmpty("MP_PRICING_URL"); v != "" {
		return v
	}
	return pricing.DefaultEndpoint
}

// pricingCachePath is the on-disk catalog cache location.
func pricingCachePath() string {
	return filepath.Join(homeDir(), ".model-proxy", "pricing_cache.json")
}
