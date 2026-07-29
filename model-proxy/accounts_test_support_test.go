package main

import (
	"os"
	"path/filepath"
	"testing"
)

// setPoolHome isolates account storage for root-package integration tests.
func setPoolHome(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".model-proxy"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)
}

// legacyPoolPath is test support for constructing compatibility fixtures. The
// production adapter intentionally exposes no legacy-path wrapper; fallback
// ownership remains inside accounts.Store.LoadSnapshot.
func legacyPoolPath(name string) string {
	return accountStore().LegacyPath(name)
}

// useStaticProviderPools prepares real plural credentials for tests that load
// production-style static providers from YAML. Behavior tests that construct
// Config directly should use testProviderID instead.
func useStaticProviderPools(t *testing.T, names ...string) {
	t.Helper()
	setPoolHome(t, t.TempDir())
	for _, name := range names {
		writePoolFile(t, name, "static", "STATIC-TEST-KEY")
	}
}
