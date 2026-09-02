package login

import (
	"testing"

	"model-proxy/internal/cli/clitest"
)

// TestHelperProcess is the subprocess entrypoint for login command tests
// (CmdLogin os.Exits on error paths). See clitest.HelperProcess.
func TestHelperProcess(t *testing.T) {
	clitest.HelperProcess(t, map[string]func([]string){
		"login": CmdLogin,
	})
}
