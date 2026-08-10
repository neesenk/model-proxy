package main

import (
	"model-proxy/internal/app"
	cliframework "model-proxy/internal/cli/framework"
	"model-proxy/provider"
)

// providerBuild mirrors internal/app.Build for root-package call sites while
// the Proxy and remaining commands migrate to the app package.
type providerBuild = app.Build

// buildProviders wires the production environment seams (home dir, codex
// version probes, volcengine signed model list) and delegates to internal/app.
func buildProviders(cfg *Config) providerBuild {
	return app.BuildProviders(cfg, app.AccountStore(), app.BuildOptions{
		HomeDir:                  cliframework.HomeDir(),
		CodexCLIVersion:          app.CodexCLIVersion,
		CodexCacheVersion:        app.CodexCacheVersion,
		ListArkAgentPlanModelIDs: app.ListArkAgentPlanModelIDs,
	})
}

// buildOne delegates to internal/app.BuildOne with the production environment
// seams (used by `usage` for per-account iteration without starting a Proxy).
func buildOne(cfg *Config, name string, prov Provider, cred app.AccountCred) provider.Provider {
	return app.BuildOne(cfg, app.BuildOptions{
		HomeDir:                  cliframework.HomeDir(),
		CodexCLIVersion:          app.CodexCLIVersion,
		CodexCacheVersion:        app.CodexCacheVersion,
		ListArkAgentPlanModelIDs: app.ListArkAgentPlanModelIDs,
	}, name, prov, cred)
}
