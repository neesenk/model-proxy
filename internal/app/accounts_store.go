package app

import (
	"model-proxy/internal/accounts"
)

type AccountCred = accounts.Credentials
type PoolAccount = accounts.Account
type CredentialPool = accounts.Pool

func AccountStore() accounts.Store {
	return accounts.NewStore(accounts.HomeDir())
}

func LoadPool(name, providerID string) (CredentialPool, error) {
	return AccountStore().Load(name, providerID)
}
