package doctor

import (
	"model-proxy/internal/accounts"
	cliframework "model-proxy/internal/cli/framework"
)

// accountStore resolves the credential-pool store lazily so tests can isolate
// HOME (t.Setenv) before first use. The shared seam lives in cli/framework.
var accountStoreLazy cliframework.LazyAccountStore

func accountStore() accounts.Store {
	return accountStoreLazy.Get()
}

func homeDir() string {
	return cliframework.HomeDir()
}
