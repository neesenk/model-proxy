package seclog

import (
	"encoding/json"
	"errors"
	"fmt"
	"model-proxy/internal/observe/logfile"
	"time"
)

// AppendSync appends one record directly to dir without a Logger, for CLI
// paths that run outside the daemon lifecycle (e.g. doctor drift findings).
// It writes the JSONL trail line (same per-day naming a running Logger uses,
// creating the directory 0700 and the file 0600, narrowing permissions on
// pre-existing storage) and inserts the record into the queryable SQLite
// store. Coexistence with a running Logger is safe: WAL + busy_timeout
// serializes the writers. Both halves are attempted; a failure on either
// surface is returned (joined) — callers like doctor's drift dedup query the
// store, so a dropped insert must not look like a success.
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
	var lineErr, storeErr error
	if err := logfile.AppendLine(dir, filePrefix, line, now); err != nil {
		lineErr = fmt.Errorf("seclog: %w", err)
	}
	st, err := openStore(dir)
	if err == nil {
		storeErr = st.insert(rec)
		if cerr := st.Close(); storeErr == nil {
			storeErr = cerr
		}
	} else {
		storeErr = err
	}
	return errors.Join(lineErr, storeErr)
}
