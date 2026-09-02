package diag

import (
	"testing"

	"model-proxy/internal/cli/clitest"
)

// TestHelperProcess is the subprocess entrypoint for diag command tests
// (CmdReplay/CmdShadow os.Exit on usage and daemon error paths).
// See clitest.HelperProcess.
func TestHelperProcess(t *testing.T) {
	clitest.HelperProcess(t, map[string]func([]string){
		"replay": RunReplay,
		"shadow": RunShadow,
	})
}
