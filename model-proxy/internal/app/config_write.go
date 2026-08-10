package app

import (
	"errors"
	"os"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/configedit"
)

// saveAndReload validates, backs up, atomically writes, then hot-reloads the
// config; on reload failure the previous file is restored from the backup.
func (api *proxyWebAPI) saveAndReload(data []byte) error {
	configFile := api.currentConfigFile()
	backup, err := configedit.WriteConfigValidated(configFile, string(data), func(path string, raw []byte) error {
		_, err := configdomain.LoadConfigFromBytes(path, raw)
		return err
	})
	if err != nil {
		return err
	}
	if err := api.commands.reload(configFile); err != nil {
		var applied *ReloadAppliedWarning
		if errors.As(err, &applied) {
			return err
		}
		if original, readErr := os.ReadFile(backup); readErr == nil {
			_ = configedit.AtomicWrite(configFile, original)
		}
		return err
	}
	return nil
}
