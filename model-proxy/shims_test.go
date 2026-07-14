package main

// shims_test.go holds the display/quota helper functions that exist ONLY to
// serve the usage-display / quota tests. They were previously in production
// main.go but had no production callers (production goes through printProviderUsage
// -> buildOne -> p.Usage(), and the quotaTracker calls p.Quota() directly).
// Relocating them here keeps them out of the production binary while leaving the
// tests' call sites unchanged. The volcengine cluster (getAFPUsage /
// resolveVolcengineAKSK / fetchVolcengineQuota) duplicates the provider package's
// own resolveAKSK + Quota(); it remains here as a test seam until those tests are
// rewritten against the provider path directly.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"model-proxy/provider"
)

// getAFPUsage calls the Volcengine signed OpenAPI GetAFPUsage and returns the
// 5h/daily/weekly/monthly AFP quota windows.
func getAFPUsage(ak, sk string) (*provider.AfpUsage, error) {
	req, err := provider.VolcengineSignedGet("GetAFPUsage", "2024-01-01", ak, sk, time.Now(), "")
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("GetAFPUsage: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("GetAFPUsage HTTP %d: %s", resp.StatusCode, truncate(string(body), 300))
	}
	var wrap struct {
		ResponseMetadata json.RawMessage   `json:"ResponseMetadata"`
		Result           provider.AfpUsage `json:"Result"`
	}
	if err := json.Unmarshal(body, &wrap); err != nil {
		return nil, fmt.Errorf("parse GetAFPUsage: %w", err)
	}
	return &wrap.Result, nil
}

// resolveVolcengineAKSK picks the AccessKey/SecretKey to sign GetAFPUsage with.
// When a cred is supplied (the pool-bound path), its AK/SK are used EXCLUSIVELY
// - the on-disk file is never consulted, preserving per-account isolation (a
// sibling virtual's file must not leak into this account's quota call). An
// incomplete cred returns an error rather than falling back to the file. When
// cred is nil (the single-account / pre-pool path), the legacy
// <name>_apikey.json is read for backward compatibility.
func resolveVolcengineAKSK(name string, cred *accountCred) (ak, sk string, err error) {
	if cred != nil {
		if cred.AccessKey != "" && cred.SecretKey != "" {
			return cred.AccessKey, cred.SecretKey, nil
		}
		return "", "", fmt.Errorf("AK/SK not configured")
	}
	c, err := loadVolcengineCreds(name)
	if err != nil || c.AccessKey == "" || c.SecretKey == "" {
		return "", "", fmt.Errorf("AK/SK not configured")
	}
	return c.AccessKey, c.SecretKey, nil
}

// fetchVolcengineQuota calls GetAFPUsage (signed, AK/SK). When cred is non-nil
// the virtual's own AK/SK are used (per-account); otherwise the legacy file is
// read. Delegates parsing to provider.ParseVolcengineQuota.
func fetchVolcengineQuota(name string, cred *accountCred) (*provider.QuotaSnapshot, error) {
	ak, sk, err := resolveVolcengineAKSK(name, cred)
	if err != nil {
		return &provider.QuotaSnapshot{Billing: provider.BillingUnknown, Err: "AK/SK not configured"}, nil
	}
	u, err := getAFPUsage(ak, sk)
	if err != nil {
		return &provider.QuotaSnapshot{Billing: provider.BillingUnknown, Err: err.Error()}, nil
	}
	return provider.ParseVolcengineQuota(u), nil
}

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
