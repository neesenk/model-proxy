package main

import (
	"errors"
	"os"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/configedit"
)

// backupConfig / atomicWrite / backupConfigPath delegate to
// internal/configedit; wrappers keep root call sites stable during migration.
func backupConfig(path, backup string) { configedit.BackupConfig(path, backup) }
func atomicWrite(path string, data []byte) error {
	return configedit.AtomicWrite(path, data)
}
func backupConfigPath(configFile string) string { return configedit.BackupPath(configFile) }

// writeConfigValidated is shared by Web config writes and models refresh.
func writeConfigValidated(configFile, data string) (backup string, err error) {
	return configedit.WriteConfigValidated(configFile, data, func(path string, raw []byte) error {
		_, err := configdomain.LoadConfigFromBytes(path, raw)
		return err
	})
}

func (api *proxyWebAPI) saveAndReload(data []byte) error {
	configFile := api.currentConfigFile()
	backup, err := writeConfigValidated(configFile, string(data))
	if err != nil {
		return err
	}
	if err := api.commands.reload(configFile); err != nil {
		var applied *reloadAppliedWarning
		if errors.As(err, &applied) {
			return err
		}
		if original, readErr := os.ReadFile(backup); readErr == nil {
			_ = atomicWrite(configFile, original)
		}
		return err
	}
	return nil
}
