package status

import (
	"model-proxy/internal/cli/clitest"
	"strings"
	"testing"
)

func TestCLI_ScheduleNoDaemon(t *testing.T) {
	// Use a port nothing is listening on to guarantee "cannot reach daemon".
	cfg := clitest.WriteTempConfig(t, "listen: 127.0.0.1:1\nproviders:\n  aqp:\n    openai_base_url: https://x\n    provider_id: aqp\n    models:\n      - m\nroutes:\n  m:\n    - {provider: aqp, model: m}\n")
	_, stderr, code := clitest.RunCLI(t, "schedule", cfg)
	if code == 0 {
		t.Error("schedule no daemon: exit=0 want non-zero")
	}
	// Assert the specific "cannot reach" message (not a 3-way OR that a panic
	// stack trace would pass).
	if !strings.Contains(stderr, "cannot reach") {
		t.Errorf("schedule no-daemon stderr missing 'cannot reach':\n%s", stderr)
	}
}
