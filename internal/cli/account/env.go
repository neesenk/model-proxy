package account

import (
	"model-proxy/internal/accounts"
	cliframework "model-proxy/internal/cli/framework"
)

// env.go holds the account package's environment seam (same shape as the
// other CLI command packages): the credential store, resolved from the
// per-call HOME so tests can isolate it. The lazy store seam is shared in
// cli/framework; provider-build options come from providerbuild.BuildOpts.
var accountStoreLazy cliframework.LazyAccountStore

func accountStore() accounts.Store {
	return accountStoreLazy.Get()
}
