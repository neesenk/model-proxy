package framework

import (
	"log"

	"model-proxy/internal/accounts"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/daemonctl"
)

// ApplyProcessConfig applies the process-wide settings derived from a loaded
// config: the credentials mode (`credentials:`) for this process's account
// stores AND OAuth blob store, and the S2 auth token files for the shared
// daemon client (a non-loopback deployment demands credentials on both
// surfaces and the CLI's own daemon calls — status/doctor --live/replay/…
// — must present them).
func ApplyProcessConfig(cfg *configdomain.Config) {
	accounts.SetProcessCredentialsMode(cfg.CredentialsMode())
	daemonctl.SetAuthFiles(cfg.Web.Auth.AdminTokenFile, cfg.Web.Auth.APIKeysFile)
}

// LoadCmdConfig loads the CLI config or exits. Commands with custom error
// handling for an invalid config (doctor's diagnostic line) load on their own
// and must still call ApplyProcessConfig.
func LoadCmdConfig(args []string) *configdomain.Config {
	cfg, err := configdomain.LoadConfig(ConfigPath(args))
	if err != nil {
		log.Fatal(err)
	}
	ApplyProcessConfig(cfg)
	return cfg
}
