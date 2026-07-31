package models

import (
	cliserve "model-proxy/internal/cli/serve"
	"strings"

	configdomain "model-proxy/internal/config"
)

// configPath scans args for --config (same shape as the root CLI helper).
func configPath(args []string) string {
	for i, a := range args {
		if (a == "--config" || a == "-config") && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(a, "--config=") {
			return strings.TrimPrefix(a, "--config=")
		}
	}
	return "config.yaml"
}

// maybeReloadDaemon delegates to internal/cli/serve (single owner for the
// post-write daemon signal).
func maybeReloadDaemon(cfg *configdomain.Config) {
	cliserve.MaybeReloadDaemon(cfg)
}
