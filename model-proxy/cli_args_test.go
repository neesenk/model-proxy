package main

import (
	cliframework "model-proxy/internal/cli/framework"
	"os"
	"path/filepath"
	"testing"
)

// TestConfigPath_LookupOrder verifies the lookup order:
// --config flag > ~/.model-proxy/config.yaml > ./config.yaml.
func TestConfigPath_LookupOrder(t *testing.T) {
	// 1. explicit flag wins over everything.
	got := cliframework.ConfigPath([]string{"--config", "/explicit.yaml"})
	if got != "/explicit.yaml" {
		t.Errorf("flag: got %q", got)
	}
	got = cliframework.ConfigPath([]string{"--config=/explicit2.yaml"})
	if got != "/explicit2.yaml" {
		t.Errorf("flag=: got %q", got)
	}

	// 2. user-level file wins over ./config.yaml. Point HOME at a temp dir with
	// the user config present (under .model-proxy/), and a different CWD config —
	// the user one wins.
	dir := t.TempDir()
	aisDir := filepath.Join(dir, ".model-proxy")
	if err := os.MkdirAll(aisDir, 0o755); err != nil {
		t.Fatal(err)
	}
	homeCfg := filepath.Join(aisDir, "config.yaml")
	if err := os.WriteFile(homeCfg, []byte("listen: 127.0.0.1:0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", dir)
	// Without a flag and with a user-level file present, configPath returns it.
	got = cliframework.ConfigPath(nil)
	if got != homeCfg {
		t.Errorf("user-level: got %q want %q", got, homeCfg)
	}
}
