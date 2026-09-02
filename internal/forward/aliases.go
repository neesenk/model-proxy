package forward

import configdomain "model-proxy/internal/config"

// Type aliases keep the moved pipeline code readable; the canonical
// definitions live in internal/config (same convention as
// internal/app/config_alias.go).
type Config = configdomain.Config
type FusionConfig = configdomain.FusionConfig
type Scheduling = configdomain.Scheduling
type Provider = configdomain.Provider
type RouteTarget = configdomain.RouteTarget
type GuardConfig = configdomain.GuardConfig
