package seclog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// AppendSync appends one record directly to dir without a Logger, for CLI
// paths that run outside the daemon lifecycle (e.g. doctor drift findings).
// It writes a single O_APPEND line into a per-day file, creating the
// directory 0700 and the file 0600, and narrows permissions on pre-existing
// storage. The per-day name never collides with a Logger's active file, so
// both can coexist in one directory and one Query scans both.
func AppendSync(dir string, rec *Record) error {
	if rec == nil {
		return fmt.Errorf("seclog: nil record")
	}
	if dir == "" {
		return fmt.Errorf("seclog: empty directory")
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("seclog: mkdir %s: %w", dir, err)
	}
	if err := os.Chmod(dir, dirMode); err != nil {
		return fmt.Errorf("seclog: chmod %s: %w", dir, err)
	}
	now := time.Now()
	if rec.Ts == 0 {
		rec.Ts = now.UnixMilli()
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("seclog: encode record: %w", err)
	}
	line = append(line, '\n')
	path := filepath.Join(dir, filePrefix+now.Format("20060102")+fileSuffix)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, logFileMode)
	if err != nil {
		return fmt.Errorf("seclog: open %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	if err := file.Chmod(logFileMode); err != nil {
		return fmt.Errorf("seclog: chmod %s: %w", path, err)
	}
	if _, err := file.Write(line); err != nil {
		return fmt.Errorf("seclog: append %s: %w", path, err)
	}
	return nil
}
