package admin

import (
	"errors"
	"os"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/configedit"
)

// saveAndReload serializes every whole-document config replacement against
// the structured and CLI read-modify-write paths. Without the common lock, a
// raw /api/config save can race a structured edit and silently overwrite it.
func (s *Service) saveAndReload(data []byte) error {
	configFile := s.currentConfigFile()
	return configedit.WithConfigLock(configFile, func() error {
		return s.saveAndReloadUnderLock(configFile, data)
	})
}

// saveAndReloadUnderLock validates, backs up, atomically writes, then
// hot-reloads the config; on reload failure the previous file is restored from
// the backup. The caller must hold configFile's configedit lock.
func (s *Service) saveAndReloadUnderLock(configFile string, data []byte) error {
	backup, err := configedit.WriteConfigValidated(configFile, string(data), func(path string, raw []byte) error {
		_, err := configdomain.LoadConfigFromBytes(path, raw)
		return err
	})
	if err != nil {
		return err
	}
	if err := s.ports.Reload(configFile); err != nil {
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
