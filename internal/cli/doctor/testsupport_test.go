package doctor

import (
	"testing"

	configdomain "model-proxy/internal/config"
)

// Shared CLI fixtures (config/pool/stdout capture) live in internal/cli/clitest;
// this file keeps only doctor-specific helpers.

// mustCfg loads the config at path or fails.
func mustCfg(t *testing.T, path string) *configdomain.Config {
	t.Helper()
	cfg, err := configdomain.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}
