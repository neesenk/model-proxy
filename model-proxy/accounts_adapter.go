package main

import (
	"model-proxy/internal/accounts"
	"model-proxy/internal/app"
)

// Account-pool compatibility aliases for root callers and tests while the
// composition root finishes migrating into internal/app.
type accountCred = accounts.Credentials
type poolAccount = accounts.Account
type credentialPool = accounts.Pool

func accountStore() accounts.Store { return app.AccountStore() }

func poolPath(name string) string { return app.PoolPath(name) }

func loadPool(name, providerID string) (credentialPool, error) {
	return app.LoadPool(name, providerID)
}

func savePool(name, providerID string, pool credentialPool) error {
	return app.SavePool(name, providerID, pool)
}

func withPoolLock(name string, fn func() error) error {
	return app.WithPoolLock(name, fn)
}

func accountIDFor(providerID string, cred accountCred) string {
	return app.AccountIDFor(providerID, cred)
}

func nowTS() string { return app.NowTS() }
