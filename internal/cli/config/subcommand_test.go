package config

import (
	"model-proxy/internal/cli/clitest"
	"strings"
	"testing"
)

func TestCLI_ConfigCheck(t *testing.T) {
	cfg := clitest.WriteTempConfig(t, clitest.MinimalConfig)
	stdout, _, code := clitest.RunCLI(t, "config", cfg, "check")
	if code != 0 {
		t.Fatalf("config check exit=%d want 0", code)
	}
	if !strings.Contains(stdout, "aqp") {
		t.Errorf("config check output missing provider aqp:\n%s", stdout)
	}
}

func TestCLI_ConfigNoSubcommand(t *testing.T) {
	cfg := clitest.WriteTempConfig(t, clitest.MinimalConfig)
	_, _, code := clitest.RunCLI(t, "config", cfg)
	if code == 0 {
		t.Error("config (no subcommand): exit=0 want non-zero")
	}
}
