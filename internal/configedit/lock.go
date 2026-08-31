package configedit

import (
	"fmt"
	"os"
	"path/filepath"
)

// WithConfigLock runs fn while holding an exclusive inter-process lock for
// configFile's read-modify-write cycle. Every writer that loads config.yaml,
// mutates it and writes it back (the web config editor, AddPreset, the CLI
// `add` command — the latter two from a different process than the daemon)
// must wrap the WHOLE load→mutate→write span in this lock; the atomic
// temp+rename write alone keeps the file undamaged but lets a concurrent
// writer silently discard the earlier writer's change (lost update). The lock
// lives in a sibling .lock file (created on demand); it is advisory and never
// blocks readers — only other WithConfigLock callers.
func WithConfigLock(configFile string, fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(configFile), 0o755); err != nil {
		return err
	}
	lockPath := configFile + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("open config lock %s: %w", lockPath, err)
	}
	if err := lockFile(f); err != nil {
		f.Close()
		return fmt.Errorf("acquire config lock %s: %w", lockPath, err)
	}
	defer func() {
		_ = unlockFile(f)
		_ = f.Close()
	}()
	return fn()
}
