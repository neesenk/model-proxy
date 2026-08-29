package models

import (
	cliserve "model-proxy/internal/cli/serve"

	configdomain "model-proxy/internal/config"
)

// maybeReloadDaemon delegates to internal/cli/serve (single owner for the
// post-write daemon signal). The full command args are forwarded so a serve
// started with `--log-file` is found at the pid file that flag derives.
func maybeReloadDaemon(args []string, cfg *configdomain.Config) {
	cliserve.MaybeReloadDaemon(args, cfg)
}
