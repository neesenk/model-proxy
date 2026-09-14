package account

import (
	"model-proxy/internal/accounts"
	cliframework "model-proxy/internal/cli/framework"
	"model-proxy/internal/providerbuild"
)

// env.go holds the account package's environment seams (same shape as the
// other CLI command packages): the credential store and the provider-build
// options, resolved from the per-call HOME so tests can isolate them.
// The lazy store seam is shared in cli/framework.
var accountStoreLazy cliframework.LazyAccountStore

func accountStore() accounts.Store {
	return accountStoreLazy.Get()
}

func buildOpts() providerbuild.BuildOptions {
	return providerbuild.BuildOptions{
		HomeDir:                  homeDir(),
		CodexCLIVersion:          providerbuild.CodexCLIVersion,
		CodexCacheVersion:        providerbuild.CodexCacheVersion,
		ListArkAgentPlanModelIDs: providerbuild.ListArkAgentPlanModelIDs,
	}
}

func homeDir() string {
	return cliframework.HomeDir()
}
