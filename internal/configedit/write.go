package configedit

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// BackupConfig copies path to backup. It is best-effort because the live
// config remains the source of truth.
func BackupConfig(path, backup string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(backup), 0o755)
	_ = os.WriteFile(backup, data, 0o644)
}

// AtomicWrite makes the target appear whole or not at all.
func AtomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o644); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

// BackupPath derives the timestamped backup path for a config file.
func BackupPath(configFile string) string {
	directory := filepath.Dir(configFile)
	base := filepath.Base(configFile)
	stamp := time.Now().Format("20060102-150405")
	return filepath.Join(directory, "back", base+"."+stamp+".bak")
}

// WriteConfigValidated writes config text after the caller's validation
// accepts it, backing up the previous file first. Validation is injected so
// this package stays free of the config schema dependency direction.
func WriteConfigValidated(configFile, data string, validate func(path string, data []byte) error) (backup string, err error) {
	if err := validate(configFile, []byte(data)); err != nil {
		return "", fmt.Errorf("refusing to write invalid config: %w", err)
	}
	backup = BackupPath(configFile)
	BackupConfig(configFile, backup)
	if err := AtomicWrite(configFile, []byte(data)); err != nil {
		return backup, err
	}
	return backup, nil
}
