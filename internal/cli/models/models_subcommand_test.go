package models

import (
	"model-proxy/internal/cli/clitest"
	"strings"
	"testing"
)

func TestCLI_ModelsListsAll(t *testing.T) {
	cfg := clitest.WriteTempConfig(t, minimalConfig)
	stdout, _, code := clitest.RunCLI(t, "models", cfg)
	if code != 0 {
		t.Fatalf("models exit=%d want 0", code)
	}
	if !strings.Contains(stdout, "glm-5.2") {
		t.Errorf("models output missing glm-5.2:\n%s", stdout)
	}
	if !strings.Contains(stdout, "aqp") {
		t.Errorf("models output missing provider name aqp:\n%s", stdout)
	}
}

func TestCLI_ModelsOneProvider(t *testing.T) {
	cfg := clitest.WriteTempConfig(t, minimalConfig)
	stdout, _, code := clitest.RunCLI(t, "models", cfg, "aqp")
	if code != 0 {
		t.Fatalf("models aqp exit=%d want 0", code)
	}
	if !strings.Contains(stdout, "glm-5.2") {
		t.Errorf("models aqp output missing glm-5.2:\n%s", stdout)
	}
}

func TestCLI_ModelsUnknownProviderExits(t *testing.T) {
	cfg := clitest.WriteTempConfig(t, minimalConfig)
	_, stderr, code := clitest.RunCLI(t, "models", cfg, "nope")
	if code == 0 {
		t.Error("models nope: exit=0 want non-zero")
	}
	if !strings.Contains(stderr, "unknown provider") {
		t.Errorf("models nope stderr missing 'unknown provider':\n%s", stderr)
	}
}
