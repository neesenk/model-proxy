package presets

import (
	"testing"

	"model-proxy/internal/cli/clitest"
)

// TestHelperProcess is the subprocess entrypoint for presets/add command
// tests (RunAdd/RunPresets os.Exit). See clitest.HelperProcess.
func TestHelperProcess(t *testing.T) {
	clitest.HelperProcess(t, map[string]func([]string){
		"add":     RunAdd,
		"presets": RunPresets,
	})
}
