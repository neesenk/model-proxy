package main

import (
	"os"
	"path/filepath"
	"testing"

	"model-proxy/internal/app"
)

// Pool test support for the remaining root CLI/integration tests. The app
// package owns the account store; these helpers delegate to it.
func setPoolHome(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".model-proxy"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)
}

func writePoolFile(t *testing.T, name, providerID string, keys ...string) {
	t.Helper()
	pool := app.CredentialPool{Version: 1}
	for _, key := range keys {
		id := app.AccountIDFor(providerID, app.AccountCred{APIKey: key})
		pool.Accounts = append(pool.Accounts, app.PoolAccount{
			ID: id, Label: key, APIKey: key, AddedAt: "2026-07-08",
		})
	}
	if err := app.SavePool(name, providerID, pool); err != nil {
		t.Fatal(err)
	}
}

func legacyPoolPath(name string) string {
	return app.AccountStore().LegacyPath(name)
}

func useStaticProviderPools(t *testing.T, names ...string) {
	t.Helper()
	setPoolHome(t, t.TempDir())
	for _, name := range names {
		writePoolFile(t, name, "static", "STATIC-TEST-KEY")
	}
}
