package main

import (
	"errors"
	"os"
	"path/filepath"
	"time"
)

// backupConfig copies path to bak. It is best-effort because the live config
// remains the source of truth.
func backupConfig(path, backup string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(backup), 0o755)
	_ = os.WriteFile(backup, data, 0o644)
}

// atomicWrite makes the target appear whole or not at all.
func atomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o644); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

// writeConfigValidated is shared by Web config writes and models refresh.
func writeConfigValidated(configFile, data string) (backup string, err error) {
	if _, err := LoadConfigFromBytes(configFile, []byte(data)); err != nil {
		return "", err
	}
	backup = backupConfigPath(configFile)
	backupConfig(configFile, backup)
	if err := atomicWrite(configFile, []byte(data)); err != nil {
		return backup, err
	}
	return backup, nil
}

func backupConfigPath(configFile string) string {
	directory := filepath.Dir(configFile)
	base := filepath.Base(configFile)
	stamp := time.Now().Format("20060102-150405")
	return filepath.Join(directory, "back", base+"."+stamp+".bak")
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
