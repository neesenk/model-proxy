package framework

import (
	"log"

	"model-proxy/internal/accounts"
	configdomain "model-proxy/internal/config"
)

// LoadCmdConfig loads the CLI config or exits. It also applies the configured
// credentials mode (`credentials:`) to this process's account stores AND OAuth
// blob store, so every command that reads credentials (usage/logout/models/
// doctor/...) sees the same backend without threading cfg through each call
// site.
func LoadCmdConfig(args []string) *configdomain.Config {
	cfg, err := configdomain.LoadConfig(ConfigPath(args))
	if err != nil {
		log.Fatal(err)
	}
	accounts.SetProcessCredentialsMode(cfg.CredentialsMode())
	return cfg
}
