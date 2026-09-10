package requestlog

import (
	"fmt"
	"model-proxy/internal/observe/logfile"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoggerEnqueueDropsWhenQueueIsFull(t *testing.T) {
	logger := New(Options{Directory: t.TempDir(), MaxFileSize: 1 << 30, MaxBodyBytes: 1024})
	for i := 0; i < logfile.DefaultQueueCapacity+17; i++ {
		logger.Enqueue(&Record{Ts: "t", RequestID: fmt.Sprintf("r-%d", i)})
	}
	if got := logger.Dropped(); got != 17 {
		t.Errorf("dropped = %d, want 17", got)
	}
}

func TestLoggerAccessors(t *testing.T) {
	dir := t.TempDir()
	logger := New(Options{Directory: dir, MaxBodyBytes: 1234})
	if got := logger.Directory(); got != dir {
		t.Errorf("Directory = %q, want %q", got, dir)
	}
	if got := logger.MaxBodyBytes(); got != 1234 {
		t.Errorf("MaxBodyBytes = %d, want 1234", got)
	}
	var nilLogger *Logger
	if got := nilLogger.Directory(); got != "" {
		t.Errorf("nil Directory = %q, want empty", got)
	}
	if got := nilLogger.MaxBodyBytes(); got != 0 {
		t.Errorf("nil MaxBodyBytes = %d, want 0", got)
	}
}

func TestLoggerShutdownDrainsEveryAcceptedUniqueRecord(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "request-log")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	logger := New(Options{
		Directory: dir, MaxFileSize: 1 << 30, MaxBodyBytes: 4096,
	})
	go logger.Run()

	const count = 1024
	for i := 0; i < count; i++ {
		logger.Enqueue(&Record{
			Ts:        "2026-07-28T01:02:03Z",
			RequestID: fmt.Sprintf("unique-%04d", i),
			Status:    200,
		})
	}
	logger.Shutdown()

	if got := logger.Dropped(); got != 0 {
		t.Fatalf("dropped = %d, want 0", got)
	}
	records, err := QueryRecords(dir, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != count {
		t.Fatalf("persisted records = %d, want %d", len(records), count)
	}
	seen := make(map[string]int, count)
	for _, record := range records {
		seen[record.RequestID]++
	}
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("unique-%04d", i)
		if seen[id] != 1 {
			t.Errorf("%s persisted %d times, want exactly once", id, seen[id])
		}
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Errorf("directory mode = %o, want 0700", got)
		}
		for _, name := range requestLogFileNames(t, dir) {
			info, err := os.Stat(filepath.Join(dir, name))
			if err != nil {
				t.Fatal(err)
			}
			if got := info.Mode().Perm(); got != 0o600 {
				t.Errorf("%s mode = %o, want 0600", name, got)
			}
		}
	}
}

// TestLoggerRestartsAppendToTheSamePerDayFile pins the file-naming contract
// that keeps the log directory to one file per day even across process
// restarts: two logger generations writing on the same day must produce a
// single file holding both records.
func TestLoggerRestartsAppendToTheSamePerDayFile(t *testing.T) {
	dir := t.TempDir()

	first := New(Options{Directory: dir, MaxFileSize: 1 << 30, MaxBodyBytes: 4096})
	go first.Run()
	first.Enqueue(&Record{Ts: "t", RequestID: "first", Status: 200})
	first.Shutdown()

	second := New(Options{Directory: dir, MaxFileSize: 1 << 30, MaxBodyBytes: 4096})
	go second.Run()
	second.Enqueue(&Record{Ts: "t", RequestID: "second", Status: 200})
	second.Shutdown()

	names := requestLogFileNames(t, dir)
	if len(names) != 1 {
		t.Fatalf("files = %v, want exactly one per-day file across restarts", names)
	}
	records, err := QueryRecords(dir, Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 {
		t.Fatalf("records in shared per-day file = %d, want 2", len(records))
	}
	if records[0].RequestID != "first" || records[1].RequestID != "second" {
		t.Errorf("request ids = [%s %s], want [first second] (append order preserved)", records[0].RequestID, records[1].RequestID)
	}
}
