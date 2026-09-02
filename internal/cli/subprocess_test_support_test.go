package cli

import (
	"testing"

	"model-proxy/internal/cli/clitest"
	cliserve "model-proxy/internal/cli/serve"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/display"
	"os"
)

// This file covers the root-owned CLI entries that call os.Exit / log.Fatal —
// they cannot be tested in-process (they'd kill the test binary). The shared
// harness lives in internal/cli/clitest; each command package registers its
// own TestHelperProcess there with the handlers it owns. The root keeps
// takeover/restore (process entries in commands.go) and the stop/reload serve
// subcommands (their handlers are cliserve.Cmd*).
func TestHelperProcess(t *testing.T) {
	daemonEnv := func() cliserve.DaemonEnv {
		return cliserve.DaemonEnv{LoadConfig: configdomain.LoadConfig, Executable: os.Args[0], Stdout: os.Stdout, Stderr: os.Stderr}
	}
	clitest.HelperProcess(t, map[string]func([]string){
		"takeover": RunTakeover,
		"restore":  RunRestore,
		"stop": func(args []string) {
			cliserve.CmdStop(daemonEnv(), cliserve.ParseArgs(args), display.Yellow, display.Gray, display.Green)
		},
		"reload": func(args []string) {
			cliserve.CmdReload(daemonEnv(), cliserve.ParseArgs(args), display.Yellow, display.Gray, display.Green)
		},
	})
}
