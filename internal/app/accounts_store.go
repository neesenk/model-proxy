package app

import (
	"time"

	"model-proxy/internal/accounts"
)

type AccountCred = accounts.Credentials
type PoolAccount = accounts.Account
type CredentialPool = accounts.Pool

func AccountStore() accounts.Store {
	return accounts.NewStore(accounts.HomeDir())
}

func PoolPath(name string) string {
	return AccountStore().PoolPath(name)
}

func LoadPool(name, providerID string) (CredentialPool, error) {
	return AccountStore().Load(name, providerID)
}

func SavePool(name, providerID string, pool CredentialPool) error {
	return AccountStore().Save(name, providerID, pool)
}

func WithPoolLock(name string, fn func() error) error {
	return AccountStore().WithLock(name, fn)
}

func AccountIDFor(providerID string, cred AccountCred) string {
	return accounts.AccountID(providerID, cred)
}

func NowTS() string {
	return accounts.Timestamp(time.Now())
}
