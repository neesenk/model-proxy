package seclog

import (
	"encoding/json"
	"fmt"
	"model-proxy/internal/observe/logfile"
	"time"
)

// AppendSync appends one record directly to dir without a Logger, for CLI
// paths that run outside the daemon lifecycle (e.g. doctor drift findings).
// It writes a single O_APPEND line into the per-day <prefix>YYYYMMDD.log file
// (the same naming a running Logger uses), creating the directory 0700 and
// the file 0600, and narrows permissions on pre-existing storage. Coexistence
// with a running Logger in the same directory is safe: both append single
// lines with O_APPEND, so one Query scans them all.
func AppendSync(dir string, rec *Record) error {
	if rec == nil {
		return fmt.Errorf("seclog: nil record")
	}
	if dir == "" {
		return fmt.Errorf("seclog: empty directory")
	}
	now := time.Now()
	if rec.Ts == 0 {
		rec.Ts = now.UnixMilli()
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("seclog: encode record: %w", err)
	}
	if err := logfile.AppendLine(dir, filePrefix, line, now); err != nil {
		return fmt.Errorf("seclog: %w", err)
	}
	return nil
}
