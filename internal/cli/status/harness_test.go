package status

import (
	"testing"

	"model-proxy/internal/cli/clitest"
)

// TestHelperProcess is the subprocess entrypoint for status command tests
// (CmdSchedule/CmdRoutes os.Exit on error paths). See clitest.HelperProcess.
func TestHelperProcess(t *testing.T) {
	clitest.HelperProcess(t, map[string]func([]string){
		"schedule": RunSchedule,
		"routes":   RunRoutes,
		"cache":    RunCache,
	})
}
