package app

import configdomain "model-proxy/internal/config"

// Type aliases keep the migrated composition-root code readable; the canonical
// definitions live in internal/config.
type Config = configdomain.Config
type FusionConfig = configdomain.FusionConfig
type ShadowTarget = configdomain.ShadowTarget
type WebConfig = configdomain.WebConfig
type StatsConfig = configdomain.StatsConfig
type PricingConfig = configdomain.PricingConfig
type PriceConfig = configdomain.PriceConfig
type RequestLogConfig = configdomain.RequestLogConfig
type CacheConfig = configdomain.CacheConfig
type Scheduling = configdomain.Scheduling
type Provider = configdomain.Provider
type PeakSegment = configdomain.PeakSegment
type PeakConfig = configdomain.PeakConfig
type RouteTarget = configdomain.RouteTarget
type Takeover = configdomain.Takeover

// LoadConfig delegates to internal/config.
func LoadConfig(path string) (*Config, error) {
	return configdomain.LoadConfig(path)
}

// LoadConfigFromBytes delegates to internal/config.
func LoadConfigFromBytes(path string, data []byte) (*Config, error) {
	return configdomain.LoadConfigFromBytes(path, data)
}
