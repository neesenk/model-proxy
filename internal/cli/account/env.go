package account

import (
	"os"
	"sync"

	"model-proxy/internal/accounts"
	"model-proxy/internal/providerbuild"
)

// env.go holds the account package's environment seams (same shape as the
// other CLI command packages): the credential store and the provider-build
// options, resolved from the per-call HOME so tests can isolate them.

var (
	accountStoreMu  sync.Mutex
	accountStoreKey string
	accountStoreVal accounts.Store
)

func accountStore() accounts.Store {
	home := homeDir()
	accountStoreMu.Lock()
	defer accountStoreMu.Unlock()
	if home != accountStoreKey {
		accountStoreVal = accounts.NewStore(home)
		accountStoreKey = home
	}
	return accountStoreVal
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
	h, _ := os.UserHomeDir()
	return h
}
