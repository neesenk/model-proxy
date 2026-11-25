package requestlog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"model-proxy/internal/observe/logfile"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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

// TestLoggerPassesHotRetentionToSink pins the Options.HotRetention →
// logfile.Options.HotRetention pass-through: the composition root resolves
// request_log.hot_retention once and shares it between both streams, and the
// accessor reports the sink's window (0 = archiving disabled; nil logger = 0).
func TestLoggerPassesHotRetentionToSink(t *testing.T) {
	logger := New(Options{Directory: t.TempDir(), HotRetention: 48 * time.Hour})
	if got := logger.HotRetention(); got != 48*time.Hour {
		t.Fatalf("HotRetention() = %v, want 48h", got)
	}
	if got := New(Options{Directory: t.TempDir()}).HotRetention(); got != 0 {
		t.Fatalf("default HotRetention() = %v, want 0 (disabled)", got)
	}
	var nilLogger *Logger
	if got := nilLogger.HotRetention(); got != 0 {
		t.Fatalf("nil HotRetention() = %v, want 0", got)
	}
}

// TestLoggerWritesOneJSONLinePerRecord pins the on-disk JSONL contract at the
// sink boundary: exactly one newline per record. The encoder must not append
// its own trailing newline because logfile.Writer.WriteLine adds one — a 2026-09
// regression double-newlined every record for two months unnoticed (in-process
// readers TrimSpace away blank lines; external line-based tooling does not).
func TestLoggerWritesOneJSONLinePerRecord(t *testing.T) {
	dir := t.TempDir()
	logger := New(Options{Directory: dir, MaxFileSize: 1 << 30, MaxBodyBytes: 4096})
	go logger.Run()
	for i := 0; i < 3; i++ {
		logger.Enqueue(&Record{
			Ts: "2026-11-22T01:02:03Z", RequestID: fmt.Sprintf("line-%d", i), Status: 200,
		})
	}
	logger.Shutdown()

	names := requestLogFileNames(t, dir)
	if len(names) != 1 {
		t.Fatalf("files = %v, want exactly one per-day file", names)
	}
	raw, err := os.ReadFile(filepath.Join(dir, names[0]))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(raw, []byte{'\n'}) {
		t.Fatal("final record lacks a newline")
	}
	if blank := strings.Count(string(raw), "\n\n"); blank != 0 {
		t.Errorf("file contains %d blank line(s) between records; the sink owns the trailing newline: %q", blank, raw)
	}
	var ids []string
	for _, line := range bytes.Split(bytes.TrimSuffix(raw, []byte{'\n'}), []byte{'\n'}) {
		var record Record
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("line %q is not valid JSONL: %v", line, err)
		}
		ids = append(ids, record.RequestID)
	}
	if len(ids) != 3 {
		t.Fatalf("records persisted = %d, want 3 (one per line)", len(ids))
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
	first.Enqueue(&Record{Ts: "2026-11-23T00:00:00Z", RequestID: "first", Status: 200})
	first.Shutdown()

	second := New(Options{Directory: dir, MaxFileSize: 1 << 30, MaxBodyBytes: 4096})
	go second.Run()
	second.Enqueue(&Record{Ts: "2026-11-23T00:00:01Z", RequestID: "second", Status: 200})
	second.Shutdown()

	names := requestLogFileNames(t, dir)
	if len(names) != 1 {
		t.Fatalf("files = %v, want exactly one per-day file across restarts", names)
	}
	raw, err := os.ReadFile(filepath.Join(dir, names[0]))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSuffix(raw, []byte{'\n'}), []byte{'\n'})
	if len(lines) != 2 {
		t.Fatalf("got %d physical lines, want 2", len(lines))
	}
	for i, want := range []string{"first", "second"} {
		var rec Record
		if err := json.Unmarshal(lines[i], &rec); err != nil {
			t.Fatalf("line %d: %v", i+1, err)
		}
		if rec.RequestID != want {
			t.Errorf("line %d request ID = %q, want %q", i+1, rec.RequestID, want)
		}
	}
}
