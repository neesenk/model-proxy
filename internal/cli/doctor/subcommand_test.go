package doctor

import (
	"model-proxy/internal/cli/clitest"
	"strings"
	"testing"
)

func TestCLI_DoctorValidConfig(t *testing.T) {
	cfg := clitest.WriteTempConfig(t, minimalConfig)
	stdout, _, code := clitest.RunCLI(t, "doctor", cfg)
	if code != 0 {
		t.Fatalf("doctor exit=%d want 0", code)
	}
	if !strings.Contains(stdout, "config valid") {
		t.Errorf("doctor output missing 'config valid':\n%s", stdout)
	}
}

func TestCLI_DoctorInvalidConfig(t *testing.T) {
	bad := `listen: 127.0.0.1:15721
providers:
  aqp:
    openai_base_url: https://x
    provider_id: aqp
    anthropic_base_url: https://x/v1   # invalid: ends with /v1
    models: []
routes: {}
`
	cfg := clitest.WriteTempConfig(t, bad)
	stdout, _, code := clitest.RunCLI(t, "doctor", cfg)
	if code == 0 {
		t.Error("doctor invalid config: exit=0 want non-zero")
	}
	if !strings.Contains(stdout, "config invalid") {
		t.Errorf("doctor invalid stdout missing 'config invalid':\n%s", stdout)
	}
}
