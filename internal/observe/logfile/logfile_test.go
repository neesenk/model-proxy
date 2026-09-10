package logfile

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func testDir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func logFileNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	return names
}

func countLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Count(data, []byte{'\n'})
}

const testPrefix = "test-"

func TestWriterAppendsAcrossRestartsOnTheSameDay(t *testing.T) {
	dir := testDir(t)
	day := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)

	first := NewWriter(dir, testPrefix, 1<<30)
	if err := first.WriteLine([]byte(`{"n":1}`), day); err != nil {
		t.Fatalf("first run write: %v", err)
	}
	first.Close()

	// A second writer for the same day (a process restart) must append to the
	// existing per-day file instead of creating another one.
	second := NewWriter(dir, testPrefix, 1<<30)
	if err := second.WriteLine([]byte(`{"n":2}`), day.Add(time.Hour)); err != nil {
		t.Fatalf("second run write: %v", err)
	}
	second.Close()

	names := logFileNames(t, dir)
	if len(names) != 1 || names[0] != "test-20260908.log" {
		t.Fatalf("files = %v, want [test-20260908.log] (one per-day file across restarts)", names)
	}
	if got := countLines(t, filepath.Join(dir, names[0])); got != 2 {
		t.Errorf("per-day file has %d lines, want both restarts appended (2)", got)
	}
}

func TestWriterLazyOpenLeavesNoEmptyFile(t *testing.T) {
	dir := testDir(t)
	writer := NewWriter(dir, testPrefix, 1<<30)
	if writer.Path() != "" {
		t.Errorf("Path before first write = %q, want empty", writer.Path())
	}
	writer.Close() // never wrote a line

	if names := logFileNames(t, dir); len(names) != 0 {
		t.Fatalf("files = %v, want none: a run that never logs must not create files", names)
	}
}

func TestWriterRotateBySize(t *testing.T) {
	dir := testDir(t)
	writer := NewWriter(dir, testPrefix, int64(len(`{"n":1}`)+1))
	line := []byte(`{"n":1}`)
	if err := writer.WriteLine(line, time.Date(2026, 7, 13, 15, 4, 5, 0, time.UTC)); err != nil {
		t.Fatalf("write first record: %v", err)
	}
	if err := writer.WriteLine(line, time.Date(2026, 7, 13, 15, 4, 6, 0, time.UTC)); err != nil {
		t.Fatalf("write second record: %v", err)
	}
	writer.Close()

	const archived = "test-20260713--150406-1.log"
	const active = "test-20260713.log"
	names := logFileNames(t, dir)
	if len(names) != 2 {
		t.Fatalf("files = %v, want [%s %s]", names, active, archived)
	}
	for _, name := range []string{archived, active} {
		if got := countLines(t, filepath.Join(dir, name)); got != 1 {
			t.Errorf("%s has %d lines, want 1", name, got)
		}
	}
}

func TestWriterRotateByDay(t *testing.T) {
	dir := testDir(t)
	writer := NewWriter(dir, testPrefix, 1<<30)
	dayOne := time.Date(2026, 7, 13, 23, 59, 0, 0, time.UTC)
	dayTwo := time.Date(2026, 7, 14, 0, 1, 0, 0, time.UTC)
	if err := writer.WriteLine([]byte(`{"n":1}`), dayOne); err != nil {
		t.Fatalf("write day one: %v", err)
	}
	if err := writer.WriteLine([]byte(`{"n":2}`), dayTwo); err != nil {
		t.Fatalf("write day two: %v", err)
	}
	writer.Close()

	// Day change switches to the new day's file; the old day keeps its
	// per-day name (no rename).
	for name, want := range map[string]int{
		"test-20260713.log": 1,
		"test-20260714.log": 1,
	} {
		if got := countLines(t, filepath.Join(dir, name)); got != want {
			t.Errorf("%s has %d lines, want %d", name, got, want)
		}
	}
	if names := logFileNames(t, dir); len(names) != 2 {
		t.Fatalf("files = %v, want exactly the two per-day files", names)
	}
}

func TestWriterSameDayDoesNotRotate(t *testing.T) {
	dir := testDir(t)
	writer := NewWriter(dir, testPrefix, 1<<30)
	now := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		if err := writer.WriteLine([]byte(`{"n":1}`), now.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	writer.Close()

	names := logFileNames(t, dir)
	if len(names) != 1 {
		t.Fatalf("files = %v, want one active file", names)
	}
	if got := countLines(t, filepath.Join(dir, names[0])); got != 5 {
		t.Errorf("active file has %d lines, want 5", got)
	}
}

func TestWriterEmptyFileRemovedOnDayChange(t *testing.T) {
	dir := testDir(t)
	writer := NewWriter(dir, testPrefix, 1<<30)
	dayOne := time.Date(2026, 7, 13, 23, 59, 0, 0, time.UTC)
	dayTwo := time.Date(2026, 7, 14, 0, 1, 0, 0, time.UTC)
	// Force an open (as a failed write would) without writing a line.
	if err := writer.open(dayOne); err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := writer.WriteLine([]byte(`{"n":1}`), dayTwo); err != nil {
		t.Fatalf("write: %v", err)
	}
	writer.Close()

	names := logFileNames(t, dir)
	if len(names) != 1 {
		t.Fatalf("files = %v, want one day-two active file", names)
	}
	if names[0] != "test-20260714.log" {
		t.Errorf("empty day-one file survived as %s, want removal", names[0])
	}
}

func TestWriterSameSecondSizeRotationsHaveUniqueNames(t *testing.T) {
	dir := testDir(t)
	writer := NewWriter(dir, testPrefix, 1)
	now := time.Date(2026, 7, 13, 15, 4, 5, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if err := writer.WriteLine([]byte(`{"n":1}`), now); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	writer.Close()

	var archives []string
	for _, name := range logFileNames(t, dir) {
		if strings.Contains(name, "--") {
			archives = append(archives, name)
		}
	}
	if len(archives) != 2 {
		t.Fatalf("archives = %v, want two", archives)
	}
	if archives[0] == archives[1] {
		t.Fatalf("archive names collided: %q", archives[0])
	}
	for _, name := range archives {
		if got := countLines(t, filepath.Join(dir, name)); got != 1 {
			t.Errorf("%s has %d lines, want 1", name, got)
		}
	}
}

func TestWriterNarrowsNewAndExistingFilesToOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits are not enforced on Windows")
	}
	t.Run("new file", func(t *testing.T) {
		dir := testDir(t)
		now := time.Date(2026, 7, 13, 15, 4, 5, 0, time.UTC)
		writer := NewWriter(dir, testPrefix, 1<<30)
		if err := writer.WriteLine([]byte(`{"n":1}`), now); err != nil {
			t.Fatal(err)
		}
		path := writer.Path()
		writer.Close()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("new log mode = %o, want 0600", got)
		}
	})

	t.Run("existing file", func(t *testing.T) {
		dir := testDir(t)
		now := time.Date(2026, 7, 13, 15, 4, 5, 0, time.UTC)
		path := filepath.Join(dir, "test-20260713.log")
		if err := os.WriteFile(path, []byte("{\"n\":0}\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		writer := NewWriter(dir, testPrefix, 1<<30)
		if err := writer.WriteLine([]byte(`{"n":1}`), now); err != nil {
			t.Fatal(err)
		}
		writer.Close()

		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("existing log mode = %o, want 0600", got)
		}
		if got := countLines(t, path); got != 2 {
			t.Errorf("existing log has %d lines, want append-preserved 2", got)
		}
	})
}

func TestLoggerRunNarrowsStorageToOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits are not enforced on Windows")
	}
	parent := t.TempDir()
	dir := filepath.Join(parent, "logs")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	logger := New(Options{Directory: dir, FilePrefix: testPrefix, Tag: "test"})
	go logger.Run()
	logger.Enqueue(func(_ time.Time, buf []byte) ([]byte, error) {
		return append(buf[:0], `{"n":1}`...), nil
	})
	logger.Shutdown()

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("directory mode = %o, want 0700", got)
	}
	for _, name := range logFileNames(t, dir) {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %o, want 0600", name, got)
		}
	}
}

func TestLoggerShutdownDrainsEveryAcceptedRecord(t *testing.T) {
	dir := testDir(t)
	logger := New(Options{Directory: dir, FilePrefix: testPrefix, Tag: "test"})
	go logger.Run()

	const count = 1024
	for i := 0; i < count; i++ {
		logger.Enqueue(func(_ time.Time, buf []byte) ([]byte, error) {
			return append(buf[:0], `{"n":1}`...), nil
		})
	}
	logger.Shutdown()

	if got := logger.Dropped(); got != 0 {
		t.Fatalf("dropped = %d, want 0 for a drainable queue", got)
	}
	if got := countLines(t, filepath.Join(dir, "test-"+time.Now().Format(dayFileLayout)+".log")); got != count {
		t.Fatalf("persisted lines = %d, want %d", got, count)
	}
}

func TestLoggerEnqueueDropsWhenQueueIsFull(t *testing.T) {
	dir := testDir(t)
	logger := New(Options{Directory: dir, FilePrefix: testPrefix, Tag: "test"})
	entered := make(chan struct{})
	release := make(chan struct{})
	logger.writeLine = func([]byte, time.Time) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return nil
	}
	go logger.Run()

	logger.Enqueue(func(_ time.Time, buf []byte) ([]byte, error) {
		return append(buf[:0], `{}`...), nil
	})
	<-entered // the single writer is now blocked inside writeLine
	for i := 0; i < DefaultQueueCapacity+10; i++ {
		logger.Enqueue(func(_ time.Time, buf []byte) ([]byte, error) {
			return append(buf[:0], `{}`...), nil
		})
	}
	if got := logger.Dropped(); got != 10 {
		t.Errorf("dropped = %d, want 10", got)
	}
	close(release)
	logger.Shutdown()
}

func TestLoggerCountsEveryWriteError(t *testing.T) {
	dir := testDir(t)
	logger := New(Options{Directory: dir, FilePrefix: testPrefix, Tag: "test"})
	logger.writeLine = func([]byte, time.Time) error {
		return errors.New("injected write failure")
	}
	go logger.Run()
	for i := 0; i < 5; i++ {
		logger.Enqueue(func(_ time.Time, buf []byte) ([]byte, error) {
			return append(buf[:0], `{}`...), nil
		})
	}
	logger.Shutdown()
	if got := logger.WriteErrors(); got != 5 {
		t.Fatalf("writeErrors = %d, want 5", got)
	}
}

func TestLoggerConcurrentProducersNeverLoseAcceptedRecords(t *testing.T) {
	dir := testDir(t)
	logger := New(Options{Directory: dir, FilePrefix: testPrefix, Tag: "test"})
	go logger.Run()

	const workers, perWorker = 8, 128
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				logger.Enqueue(func(_ time.Time, buf []byte) ([]byte, error) {
					return append(buf[:0], `{}`...), nil
				})
			}
		}()
	}
	wg.Wait()
	logger.Shutdown()

	written := countLines(t, filepath.Join(dir, "test-"+time.Now().Format(dayFileLayout)+".log"))
	if written+int(logger.Dropped()) != workers*perWorker {
		t.Fatalf("written %d + dropped %d = %d, want %d", written, logger.Dropped(), written+int(logger.Dropped()), workers*perWorker)
	}
}

func TestLoggerRetentionSweepDeletesExpiredLogsOnly(t *testing.T) {
	dir := testDir(t)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	oldTime := now.Add(-40 * 24 * time.Hour)
	recentTime := now.Add(-24 * time.Hour)

	oldArchive := filepath.Join(dir, "test-20260601--010000-1.log")
	recentArchive := filepath.Join(dir, "test-20260727--010000-1.log")
	orphanedActive := filepath.Join(dir, "test-20260601.log")
	currentActive := filepath.Join(dir, "test-20260728.log")
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

	logger := New(Options{Directory: dir, FilePrefix: testPrefix, Retention: 30 * 24 * time.Hour, Tag: "test"})
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
	dir := testDir(t)
	path := filepath.Join(dir, "test-20200101--010000-1.log")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(path, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	logger := New(Options{Directory: dir, FilePrefix: testPrefix, Tag: "test"})
	logger.sweep(time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC), "")
	if _, err := os.Stat(path); err != nil {
		t.Errorf("zero retention removed old log: %v", err)
	}
}

func TestAppendLineAppendsToPerDayFile(t *testing.T) {
	dir := testDir(t)
	now := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	if err := AppendLine(dir, testPrefix, []byte(`{"n":1}`), now); err != nil {
		t.Fatalf("first append: %v", err)
	}
	if err := AppendLine(dir, testPrefix, []byte(`{"n":2}`), now.Add(time.Hour)); err != nil {
		t.Fatalf("second append: %v", err)
	}

	path := filepath.Join(dir, "test-20260713.log")
	if got := countLines(t, path); got != 2 {
		t.Errorf("%s has %d lines, want 2", path, got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(data, []byte("{\"n\":2}\n")) {
		t.Errorf("file tail = %q, want appended line with trailing newline", data)
	}
}

func TestLoggerAccessorsAreNilSafe(t *testing.T) {
	var nilLogger *Logger
	if got := nilLogger.Dropped(); got != 0 {
		t.Errorf("nil Dropped = %d, want 0", got)
	}
	if got := nilLogger.WriteErrors(); got != 0 {
		t.Errorf("nil WriteErrors = %d, want 0", got)
	}
	if got := nilLogger.Directory(); got != "" {
		t.Errorf("nil Directory = %q, want empty", got)
	}
	// Safe no-ops on nil and safe calls on a live logger.
	nilLogger.Enqueue(nil)
	nilLogger.Run()
	nilLogger.Shutdown()
	logger := New(Options{Directory: t.TempDir(), FilePrefix: testPrefix, Tag: "test"})
	logger.Enqueue(nil) // nil encoders must be ignored without dropping
	if got := logger.Dropped(); got != 0 {
		t.Errorf("Dropped after nil encode = %d, want 0", got)
	}
	go logger.Run()
	logger.Shutdown()
}

func TestNewWriterDefaultMaxBytes(t *testing.T) {
	w := NewWriter(t.TempDir(), testPrefix, 0)
	defer w.Close()
	if w.maxSize != DefaultMaxBytes {
		t.Errorf("maxSize = %d, want default %d", w.maxSize, DefaultMaxBytes)
	}
}

func TestLoggerEncodeErrorsCountAsWriteErrors(t *testing.T) {
	dir := testDir(t)
	logger := New(Options{Directory: dir, FilePrefix: testPrefix, Tag: "test"})
	go logger.Run()
	logger.Enqueue(func(time.Time, []byte) ([]byte, error) { return nil, errors.New("injected encode failure") })
	logger.Shutdown()
	if got := logger.WriteErrors(); got != 1 {
		t.Fatalf("writeErrors = %d, want 1 (encode failure must count as a lost record)", got)
	}
}

func TestLoggerRunWithUnusableDirectoryFailsClosed(t *testing.T) {
	parent := t.TempDir()
	blocker := filepath.Join(parent, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	logger := New(Options{Directory: blocker, FilePrefix: testPrefix, Tag: "test"})
	go logger.Run()
	logger.Shutdown() // must not hang: Run disables logging and returns
	if got := logger.Directory(); got != blocker {
		t.Errorf("Directory = %q, want %q", got, blocker)
	}
}

func TestWriterOpenErrorIsReportedNotPanicked(t *testing.T) {
	parent := t.TempDir()
	blocker := filepath.Join(parent, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	writer := NewWriter(blocker, testPrefix, 1<<30)
	if err := writer.WriteLine([]byte(`{}`), time.Now()); err == nil {
		t.Fatal("WriteLine into an unusable directory must fail")
	}
	writer.Close()
}

func TestSweepReportsUnreadableDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	logger := New(Options{Directory: missing, FilePrefix: testPrefix, Retention: time.Hour, Tag: "test"})
	// Must not panic; readdir failure is a warning, not an error.
	logger.sweep(time.Now(), "")
}

func TestEnsureDirRejectsFileCollision(t *testing.T) {
	parent := t.TempDir()
	blocker := filepath.Join(parent, "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDir(blocker); err == nil {
		t.Fatal("EnsureDir over an existing file must fail")
	}
}

func TestAppendLineReportsUnusableDirectory(t *testing.T) {
	parent := t.TempDir()
	blocker := filepath.Join(parent, "not-a-dir")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AppendLine(blocker, testPrefix, []byte(`{}`), time.Now()); err == nil {
		t.Fatal("AppendLine into an unusable directory must fail")
	}
}

func TestAppendLineNarrowsStorageToOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits are not enforced on Windows")
	}
	dir := testDir(t)
	path := filepath.Join(dir, "test-20260713.log")
	if err := os.WriteFile(path, []byte("{\"n\":0}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := AppendLine(dir, testPrefix, []byte(`{"n":1}`), time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	dirInfo, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Errorf("dir mode = %o, want 0700", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("file mode = %o, want 0600", got)
	}
}
