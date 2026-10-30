package doctor

import (
	"fmt"
	"os"

	"model-proxy/internal/accounts"
	cliframework "model-proxy/internal/cli/framework"
	"model-proxy/internal/providerstore"
)

// RunDoctor is the process-level entry for `doctor`: an invalid config is a
// user-facing diagnostic line (not a log.Fatal), then the offline/live
// scheduling diagnostic runs.
func RunDoctor(args []string) {
	// Providers resolve through the provider.json store (read-only).
	cfg, err := providerstore.LoadEffectiveConfig(cliframework.ConfigPath(args), false)
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
