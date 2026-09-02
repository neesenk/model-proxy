package models

import (
	"testing"

	"model-proxy/internal/cli/clitest"
)

// TestHelperProcess is the subprocess entrypoint for models/test command
// tests (CmdModels/CmdTest os.Exit on error paths). See clitest.HelperProcess.
func TestHelperProcess(t *testing.T) {
	clitest.HelperProcess(t, map[string]func([]string){
		"models": RunModels,
		"test":   CmdTest,
	})
}
