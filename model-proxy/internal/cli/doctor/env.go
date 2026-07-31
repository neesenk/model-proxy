package doctor

import (
	"os"
	"sync"

	"model-proxy/internal/accounts"
)

// accountStore resolves the credential-pool store lazily so tests can isolate
// HOME (t.Setenv) before first use.
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

func homeDir() string {
	h, _ := os.UserHomeDir()
	return h
}
