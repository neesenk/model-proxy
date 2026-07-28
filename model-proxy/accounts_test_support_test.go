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
