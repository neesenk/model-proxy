package account

import (
	"testing"

	"model-proxy/internal/cli/clitest"
)

// TestHelperProcess is the subprocess entrypoint for logout command tests
// (CmdLogout os.Exits / log.Fatalf on error paths). See clitest.HelperProcess.
func TestHelperProcess(t *testing.T) {
	clitest.HelperProcess(t, map[string]func([]string){
		"logout": RunLogout,
	})
}
