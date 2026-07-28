package requestlog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoggerEnqueueDropsWhenQueueIsFull(t *testing.T) {
	logger := New(Options{Directory: t.TempDir(), MaxFileSize: 1 << 30, MaxBodyBytes: 1024})
	for i := 0; i < queueCapacity+17; i++ {
		logger.Enqueue(&Record{Ts: "t", RequestID: fmt.Sprintf("r-%d", i)})
	}
	if got := atomic.LoadUint64(&logger.dropped); got != 17 {
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

	if got := atomic.LoadUint64(&logger.dropped); got != 0 {
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

func TestLoggerCountsEveryWriteError(t *testing.T) {
	logger := New(Options{
		Directory: t.TempDir(), MaxFileSize: 1 << 30, MaxBodyBytes: 4096,
	})
	logger.writeRecord = func(*Record, time.Time) error {
		return errors.New("injected write failure")
	}
	go logger.Run()
	for i := 0; i < 5; i++ {
		logger.Enqueue(&Record{Ts: "t", RequestID: fmt.Sprintf("r-%d", i)})
	}
	logger.Shutdown()
	if got := atomic.LoadUint64(&logger.writeErrors); got != 5 {
		t.Fatalf("writeErrors = %d, want 5", got)
	}
}

func TestLoggerRetentionSweepDeletesExpiredLogsOnly(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	oldTime := now.Add(-40 * 24 * time.Hour)
	recentTime := now.Add(-24 * time.Hour)

	oldArchive := filepath.Join(dir, "requests-20260601-000000--20260601-010000-1.log")
	recentArchive := filepath.Join(dir, "requests-20260727-000000--20260727-010000-1.log")
	orphanedActive := filepath.Join(dir, "requests-20260601-000000.log")
	currentActive := filepath.Join(dir, "requests-20260728-120000.log")
	ignoredFile := filepath.Join(dir, "other-20260601.log")

	for _, item := range []struct {
		path    string
		modTime time.Time
	}{
		{oldArchive, oldTime},
		{recentArchive, recentTime},
		{orphanedActive, oldTime},
		{currentActive, oldTime},
		{ignoredFile, oldTime},
	} {
		if err := os.WriteFile(item.path, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(item.path, item.modTime, item.modTime); err != nil {
			t.Fatal(err)
		}
	}

	logger := New(Options{Directory: dir, Retention: 30 * 24 * time.Hour})
	logger.sweep(now, currentActive)

	for _, check := range []struct {
		name       string
		path       string
		wantExists bool
	}{
		{"expired archive", oldArchive, false},
		{"recent archive", recentArchive, true},
		{"expired orphaned active file", orphanedActive, false},
		{"current active file", currentActive, true},
		{"unrelated file", ignoredFile, true},
	} {
		_, err := os.Stat(check.path)
		exists := err == nil
		if exists != check.wantExists {
			t.Errorf("%s exists = %v, want %v (stat err %v)", check.name, exists, check.wantExists, err)
		}
	}
}

func TestLoggerZeroRetentionKeepsExpiredLogs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "requests-20200101-000000--20200101-010000-1.log")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	logger := New(Options{Directory: dir})
	logger.sweep(time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC), "")
	if _, err := os.Stat(path); err != nil {
		t.Errorf("zero retention removed old log: %v", err)
	}
}
