package config

import (
	"testing"

	"model-proxy/internal/cli/clitest"
)

// TestHelperProcess is the subprocess entrypoint for config command tests
// (`config init` writes ./config.yaml and os.Exits on refusal).
// See clitest.HelperProcess.
func TestHelperProcess(t *testing.T) {
	clitest.HelperProcess(t, map[string]func([]string){
		"config": CmdConfigRun,
	})
}
