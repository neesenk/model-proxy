package main

import (
	cliframework "model-proxy/internal/cli/framework"
	"time"

	"model-proxy/internal/accounts"
)

type accountCred = accounts.Credentials
type poolAccount = accounts.Account
type credentialPool = accounts.Pool

func accountStore() accounts.Store {
	return accounts.NewStore(cliframework.HomeDir())
}

func poolPath(name string) string {
	return accountStore().PoolPath(name)
}

func loadPool(name, providerID string) (credentialPool, error) {
	return accountStore().Load(name, providerID)
}

func savePool(name, providerID string, pool credentialPool) error {
	return accountStore().Save(name, providerID, pool)
}

func withPoolLock(name string, fn func() error) error {
	return accountStore().WithLock(name, fn)
}

func accountIDFor(providerID string, cred accountCred) string {
	return accounts.AccountID(providerID, cred)
}

func nowTS() string {
	return accounts.Timestamp(time.Now())
}
