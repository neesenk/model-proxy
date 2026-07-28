package main

import (
	"os"
	"path/filepath"
	"testing"

	"model-proxy/provider"
)

func TestClearAccount(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "acct.json")
	os.WriteFile(p, []byte("{}"), 0o600)
	if err := provider.ClearAqpAccount(p); err != nil {
		t.Fatalf("clearAccount existing: %v", err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Error("clearAccount did not remove the file")
	}
	// Idempotent: missing file is not an error.
	if err := provider.ClearAqpAccount(p); err != nil {
		t.Errorf("clearAccount missing: want nil, got %v", err)
	}
}
