package doctor

import (
	"fmt"
	"os"

	"model-proxy/internal/accounts"
	cliframework "model-proxy/internal/cli/framework"
	configdomain "model-proxy/internal/config"
)

// RunDoctor is the process-level entry for `doctor`: an invalid config is a
// user-facing diagnostic line (not a log.Fatal), then the offline/live
// scheduling diagnostic runs.
func RunDoctor(args []string) {
	cfg, err := configdomain.LoadConfig(cliframework.ConfigPath(args))
	if err != nil {
		fmt.Println("✗ config invalid: " + err.Error())
		os.Exit(1)
	}
	// doctor handles an invalid config as a diagnostic line instead of
	// log.Fatal, so it loads on its own — the process-wide daemon credentials
	// still must be wired (doctor --live talks to the daemon's /api/*).
	cliframework.ApplyProcessConfig(cfg)
	accounts.SetProcessCredentialsMode(cfg.CredentialsMode())
	CmdDoctor(args, cfg, cliframework.ConfigPath(args))
}
