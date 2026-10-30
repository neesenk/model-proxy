package admin

import (
	"errors"
	"os"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/configedit"
	"model-proxy/internal/providerstore"
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
	backup, err := configedit.WriteConfigValidated(configFile, string(data), s.validateYAMLWithStore)
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

// validateYAMLWithStore validates candidate config bytes against the
// provider.json store: a post-migration config.yaml carries no providers:
// section, so raw-YAML saves must validate against the store's set instead of
// the yaml's own (empty) one. Providers pasted INTO the yaml validate as the
// yaml-wins merge — exactly what the next absorbing reload would persist.
func (s *Service) validateYAMLWithStore(path string, raw []byte) error {
	providers, err := providerstore.Resolve(path, providerstore.PathUnder(s.homeDir()))
	if err != nil {
		return err
	}
	_, err = configdomain.LoadConfigEffective(path, raw, providers)
	return err
}
