package admin

import (
	"testing"

	"model-proxy/internal/cli/clitest"
)

// TestHelperProcess is the subprocess entrypoint for admin command tests
// (CmdPin/CmdUnpin/CmdUnfreeze/CmdFreeze/ParseTTLValue os.Exit on usage and
// daemon error paths). See clitest.HelperProcess.
func TestHelperProcess(t *testing.T) {
	clitest.HelperProcess(t, map[string]func([]string){
		"pin":      RunPin,
		"unpin":    RunUnpin,
		"unfreeze": RunUnfreeze,
		"freeze":   RunFreeze,
	})
}
