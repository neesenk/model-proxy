package doctor

import (
	"testing"

	"model-proxy/internal/cli/clitest"
)

// TestHelperProcess is the subprocess entrypoint for doctor command tests
// (RunDoctor os.Exits on an invalid config). See clitest.HelperProcess.
func TestHelperProcess(t *testing.T) {
	clitest.HelperProcess(t, map[string]func([]string){
		"doctor": RunDoctor,
	})
}
