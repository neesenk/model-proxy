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

// AtomicWrite makes the target appear whole or not at all. The temp file is
// UNIQUE per writer: the daemon's Web config editor and CLI commands run in
// different processes and can write the same config.yaml concurrently — a
// shared ".tmp" name let one process rename away another's half-written file
// (docs/engineering/pitfalls.md #18).
func AtomicWrite(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	temporary := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(temporary)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(temporary)
		return err
	}
	if err := os.Chmod(temporary, 0o644); err != nil {
		os.Remove(temporary)
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
