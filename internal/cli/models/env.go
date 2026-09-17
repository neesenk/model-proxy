package models

import (
	"model-proxy/internal/accounts"
	cliframework "model-proxy/internal/cli/framework"
)

// Environment seams for the models command group. The account store is
// resolved lazily (shared seam in cli/framework) so tests can isolate HOME
// (t.Setenv) before first use — a package-init-time store would pin the real
// home directory. Provider-build options come from providerbuild.BuildOpts.
var accountStoreLazy cliframework.LazyAccountStore

func accountStore() accounts.Store {
	return accountStoreLazy.Get()
}

func homeDir() string {
	return cliframework.HomeDir()
}
