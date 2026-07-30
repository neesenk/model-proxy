package models

import (
	"os"
	"sync"

	"model-proxy/internal/accounts"
	"model-proxy/internal/app"
)

// Environment seams for the models command group. The account store is
// resolved lazily so tests can isolate HOME (t.Setenv) before first use —
// a package-init-time store would pin the real home directory.
var (
	accountStoreMu  sync.Mutex
	accountStoreKey string
	accountStoreV   accounts.Store
)

func accountStore() accounts.Store {
	home := homeDir()
	accountStoreMu.Lock()
	defer accountStoreMu.Unlock()
	if home != accountStoreKey {
		accountStoreV = accounts.NewStore(home)
		accountStoreKey = home
	}
	return accountStoreV
}

func buildOpts() app.BuildOptions {
	return app.BuildOptions{
		HomeDir:                  homeDir(),
		CodexCLIVersion:          app.CodexCLIVersion,
		CodexCacheVersion:        app.CodexCacheVersion,
		ListArkAgentPlanModelIDs: ListArkAgentPlanModelIDs,
	}
}

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}
