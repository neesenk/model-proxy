package main

// shims_test.go holds the display/quota helper functions that exist ONLY to
// serve the usage-display / quota tests. They were previously in production
// main.go but had no production callers (production goes through printProviderUsage
// -> buildOne -> p.Usage(), and the quotaTracker calls p.Quota() directly).
// Relocating them here keeps them out of the production binary while leaving the
// tests' call sites unchanged.
//
// Each is a thin buildOne().Usage() / .Quota() dispatcher; the fetch + parse
// logic lives in the provider package. The volcengine cluster (getAFPUsage /
// resolveVolcengineAKSK / fetchVolcengineQuota) that used to live here was
// removed - it duplicated provider/volcengine.go's getAFPUsage/resolveAKSK/Quota,
// and its tests were redundant with provider/quota_fetch_test.go.

import (
	"model-proxy/provider"
)

// fetchCodexQuota delegates to the codex provider's Quota() via buildOne (the
// provider now owns the fetch + parse).
func fetchCodexQuota(cfg *Config, prov Provider) (*provider.QuotaSnapshot, error) {
	return buildOne(cfg, "codex", prov, accountCred{}).Quota()
}

// fetchZhipuQuota delegates to the zhipu provider's Quota() via buildOne.
func fetchZhipuQuota(cfg *Config, name string, prov Provider, cred *accountCred) (*provider.QuotaSnapshot, error) {
	c := accountCred{}
	if cred != nil {
		c = *cred
	}
	return buildOne(cfg, name, prov, c).Quota()
}

// fetchDeepseekQuota delegates to the deepseek provider's Quota() via buildOne.
func fetchDeepseekQuota(cfg *Config, name string, prov Provider, cred *accountCred) (*provider.QuotaSnapshot, error) {
	c := accountCred{}
	if cred != nil {
		c = *cred
	}
	return buildOne(cfg, name, prov, c).Quota()
}

// fetchAqpQuota delegates to the aqp provider's Quota() via buildOne.
func fetchAqpQuota(cfg *Config) (*provider.QuotaSnapshot, error) {
	prov := cfg.Providers["aqp"]
	if prov.Provider == "" {
		prov = Provider{Provider: "aqp"}
	}
	return buildOne(cfg, "aqp", prov, accountCred{}).Quota()
}

// show*Usage are 1-line dispatch shims to the provider's Usage() display method
// (the display logic lives in provider/usage_display.go).
func showAqpUsage(cfg *Config) {
	prov := cfg.Providers["aqp"]
	if prov.Provider == "" {
		prov = Provider{Provider: "aqp"}
	}
	buildOne(cfg, "aqp", prov, accountCred{}).Usage()
}
func showCodexUsage(cfg *Config, prov Provider) { buildOne(cfg, "codex", prov, accountCred{}).Usage() }
func showGenericUsage(cfg *Config, name string, prov Provider, cred *accountCred) {
	showApiKeyUsage(cfg, name, prov, cred)
}
func showDeepseekUsage(cfg *Config, name string, prov Provider, cred *accountCred) {
	showApiKeyUsage(cfg, name, prov, cred)
}
func showVolcengineUsage(cfg *Config, name string, prov Provider, cred *accountCred) {
	showApiKeyUsage(cfg, name, prov, cred)
}
func showApiKeyUsage(cfg *Config, name string, prov Provider, cred *accountCred) {
	c := accountCred{}
	if cred != nil {
		c = *cred
	}
	if p := buildOne(cfg, name, prov, c); p != nil {
		p.Usage()
	}
}
