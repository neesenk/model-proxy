package configedit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
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
	return WithConfigLockContext(context.Background(), configFile, fn)
}

// WithConfigLockContext cancels acquisition without spawning a lock-waiting
// goroutine. Once fn starts, its caller owns the commit/cancellation boundary.
func WithConfigLockContext(ctx context.Context, configFile string, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(configFile), 0o755); err != nil {
		return err
	}
	lockPath := configFile + ".lock"
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return fmt.Errorf("open config lock %s: %w", lockPath, err)
	}
	if err := acquireConfigLock(ctx, f); err != nil {
		f.Close()
		return fmt.Errorf("acquire config lock %s: %w", lockPath, err)
	}
	defer func() {
		_ = unlockFile(f)
		_ = f.Close()
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}

func acquireConfigLock(ctx context.Context, f *os.File) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		locked, err := tryLockFile(f)
		if err != nil {
			return err
		}
		if locked {
			return nil
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
