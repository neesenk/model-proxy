package main

import configdomain "model-proxy/internal/config"

// Test-only aliases so root CLI/integration tests keep the short names; the
// canonical config types live in internal/config.
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

func LoadConfig(path string) (*Config, error) {
	return configdomain.LoadConfig(path)
}

func LoadConfigFromBytes(path string, data []byte) (*Config, error) {
	return configdomain.LoadConfigFromBytes(path, data)
}
