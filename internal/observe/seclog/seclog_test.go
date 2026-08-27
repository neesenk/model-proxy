package seclog

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

func startLogger(t *testing.T, dir string, opts Options) *Logger {
	t.Helper()
	logger, err := New(dir, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	go logger.Run()
	t.Cleanup(logger.Shutdown)
	return logger
}

func queryKinds(t *testing.T, dir string, filter Filter) *Result {
	t.Helper()
	result, err := Query(dir, filter)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	return result
}

func TestWriteQueryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	logger := startLogger(t, dir, Options{})
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC).UnixMilli()
	kinds := []string{KindSecret, KindPath, KindDrift, KindSecret, KindPath}
	for i, kind := range kinds {
		logger.Enqueue(&Record{
			Ts:        base + int64(i)*1000,
			Kind:      kind,
			RequestID: fmt.Sprintf("req-%d", i),
			Agent:     "claude",
			Protocol:  "anthropic",
			Exposed:   "claude-sonnet-4",
			Names:     []string{"aws-access-token"},
			Action:    "log",
		})
	}
	logger.Shutdown()

	all := queryKinds(t, dir, Filter{})
	if len(all.Records) != len(kinds) {
		t.Fatalf("records = %d, want %d", len(all.Records), len(kinds))
	}
	if all.Skipped != 0 {
		t.Fatalf("skipped = %d, want 0", all.Skipped)
	}
	for i := 1; i < len(all.Records); i++ {
		if all.Records[i-1].Ts < all.Records[i].Ts {
			t.Fatalf("records not newest-first at %d: %d then %d", i, all.Records[i-1].Ts, all.Records[i].Ts)
		}
	}
	// Round-trip preserves fields; newest record is the last enqueued.
	newest := all.Records[0]
	if newest.Kind != KindPath || newest.RequestID != "req-4" || newest.Exposed != "claude-sonnet-4" ||
		len(newest.Names) != 1 || newest.Names[0] != "aws-access-token" || newest.Action != "log" {
		t.Errorf("newest record = %+v, fields did not round-trip", newest)
	}

	secrets := queryKinds(t, dir, Filter{Kind: KindSecret})
	if len(secrets.Records) != 2 {
		t.Fatalf("kind filter records = %d, want 2", len(secrets.Records))
	}
	for _, record := range secrets.Records {
		if record.Kind != KindSecret {
			t.Errorf("kind filter returned kind %q", record.Kind)
		}
	}

	window := queryKinds(t, dir, Filter{From: base + 1000, To: base + 3000})
	if len(window.Records) != 3 {
		t.Fatalf("time window records = %d, want 3", len(window.Records))
	}
	for _, record := range window.Records {
		if record.Ts < base+1000 || record.Ts > base+3000 {
			t.Errorf("time window returned ts %d outside [%d, %d]", record.Ts, base+1000, base+3000)
		}
	}

	limited := queryKinds(t, dir, Filter{Limit: 2})
	if len(limited.Records) != 2 {
		t.Fatalf("limit records = %d, want 2", len(limited.Records))
	}
	if limited.Records[0].Ts != base+4000 || limited.Records[1].Ts != base+3000 {
		t.Errorf("limit kept ts [%d %d], want the two newest [%d %d]",
			limited.Records[0].Ts, limited.Records[1].Ts, base+4000, base+3000)
	}
}

func TestRotationAcrossFiles(t *testing.T) {
	dir := t.TempDir()
	logger := startLogger(t, dir, Options{MaxBytes: 512})
	const total = 20
	for i := 0; i < total; i++ {
		logger.Enqueue(&Record{
			Ts:     int64(1_700_000_000_000 + i),
			Kind:   KindSecret,
			Names:  []string{"openai-api-key"},
			Action: "redact",
			Detail: strings.Repeat("x", 64),
		})
	}
	logger.Shutdown()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var files int
	for _, entry := range entries {
		if isLogFile(entry.Name()) {
			files++
		}
	}
	if files < 2 {
		t.Fatalf("log files = %d, want rotation into multiple files", files)
	}
	result := queryKinds(t, dir, Filter{})
	if len(result.Records) != total {
		t.Fatalf("query across rotated files = %d records, want %d", len(result.Records), total)
	}
}

func TestRetentionSweepRemovesAgedFiles(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "security-20200101-000000.log")
	if err := os.WriteFile(stale, []byte(`{"ts":1,"kind":"drift"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	aged := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stale, aged, aged); err != nil {
		t.Fatal(err)
	}

	logger := startLogger(t, dir, Options{Retention: time.Hour})
	logger.Enqueue(&Record{Kind: KindSecret, Names: []string{"github-token"}, Action: "block"})
	logger.Shutdown()

	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale file still present after sweep (stat err = %v)", err)
	}
	// The live record survives: the active file is never swept.
	result := queryKinds(t, dir, Filter{Kind: KindSecret})
	if len(result.Records) != 1 {
		t.Fatalf("records after sweep = %d, want 1", len(result.Records))
	}
}

func TestRetentionZeroKeepsFilesForever(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "security-20200101-000000.log")
	if err := os.WriteFile(stale, []byte(`{"ts":1,"kind":"drift"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	aged := time.Now().Add(-10 * 365 * 24 * time.Hour)
	if err := os.Chtimes(stale, aged, aged); err != nil {
		t.Fatal(err)
	}

	logger := startLogger(t, dir, Options{Retention: 0})
	logger.Enqueue(&Record{Kind: KindDrift, Detail: "client=claude"})
	logger.Shutdown()

	if _, err := os.Stat(stale); err != nil {
		t.Errorf("retention=0 must keep files forever, stat err = %v", err)
	}
}

func TestAppendSyncCoexistsWithLogger(t *testing.T) {
	dir := t.TempDir()
	drift := &Record{Kind: KindDrift, Agent: "doctor", Detail: "client=claude host=api.anthropic.com"}
	if err := AppendSync(dir, drift); err != nil {
		t.Fatalf("AppendSync: %v", err)
	}
	if drift.Ts == 0 {
		t.Error("AppendSync must stamp a zero Ts")
	}

	logger := startLogger(t, dir, Options{})
	logger.Enqueue(&Record{Ts: drift.Ts + 1, Kind: KindSecret, Names: []string{"pem-private-key"}, Action: "log"})
	logger.Shutdown()

	if err := AppendSync(dir, &Record{Ts: drift.Ts + 2, Kind: KindDrift, Agent: "doctor", Detail: "client=codex"}); err != nil {
		t.Fatalf("second AppendSync: %v", err)
	}

	result := queryKinds(t, dir, Filter{})
	if len(result.Records) != 3 {
		t.Fatalf("records = %d, want 3 from AppendSync and Logger files", len(result.Records))
	}
	drifts := queryKinds(t, dir, Filter{Kind: KindDrift})
	if len(drifts.Records) != 2 {
		t.Fatalf("drift records = %d, want 2", len(drifts.Records))
	}
}

func TestAppendSyncValidation(t *testing.T) {
	if err := AppendSync(t.TempDir(), nil); err == nil {
		t.Error("AppendSync(nil record) must fail")
	}
	if err := AppendSync("", &Record{Kind: KindDrift}); err == nil {
		t.Error("AppendSync(empty dir) must fail")
	}
}

func TestEnqueueDropsWhenQueueFull(t *testing.T) {
	dir := t.TempDir()
	logger, err := New(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	logger.writeRecord = func(*Record, time.Time) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return nil
	}
	go logger.Run()

	logger.Enqueue(&Record{Kind: KindSecret})
	<-entered // the single writer is now blocked inside writeRecord
	for i := 0; i < queueCapacity+10; i++ {
		logger.Enqueue(&Record{Kind: KindSecret})
	}
	if got := logger.Dropped(); got != 10 {
		t.Errorf("dropped = %d, want 10", got)
	}
	close(release)
	logger.Shutdown()
}

func TestShutdownDrainsQueuedRecords(t *testing.T) {
	dir := t.TempDir()
	logger, err := New(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	go logger.Run()
	const total = 200
	for i := 0; i < total; i++ {
		logger.Enqueue(&Record{Ts: int64(i + 1), Kind: KindPath, Names: []string{"dotenv"}, Action: "log"})
	}
	logger.Shutdown()

	if got := logger.Dropped(); got != 0 {
		t.Fatalf("dropped = %d, want 0 for a drainable queue", got)
	}
	result := queryKinds(t, dir, Filter{})
	if len(result.Records) != total {
		t.Fatalf("records after shutdown drain = %d, want %d", len(result.Records), total)
	}
}

func TestConcurrentEnqueueShutdown(t *testing.T) {
	dir := t.TempDir()
	logger, err := New(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	go logger.Run()

	const workers = 8
	const perWorker = 100
	var wg sync.WaitGroup
	start := make(chan struct{})
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			<-start
			for i := 0; i < perWorker; i++ {
				logger.Enqueue(&Record{Ts: int64(w*perWorker + i + 1), Kind: KindSecret, Action: "log"})
			}
		}(w)
	}
	close(start)
	wg.Wait()
	logger.Shutdown()

	result := queryKinds(t, dir, Filter{})
	if written, dropped := len(result.Records), logger.Dropped(); int64(written)+int64(dropped) != workers*perWorker {
		t.Fatalf("written %d + dropped %d = %d, want %d", written, dropped, int64(written)+int64(dropped), workers*perWorker)
	}
}

func TestFileAndDirectoryPermissions(t *testing.T) {
	dir := t.TempDir()
	if err := AppendSync(dir, &Record{Kind: KindDrift, Detail: "client=pi"}); err != nil {
		t.Fatal(err)
	}
	logger := startLogger(t, dir, Options{})
	logger.Enqueue(&Record{Kind: KindSecret, Names: []string{"google-api-key"}, Action: "log"})
	logger.Shutdown()

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != dirMode {
		t.Errorf("dir mode = %o, want %o", got, dirMode)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var files int
	for _, entry := range entries {
		if !isLogFile(entry.Name()) {
			continue
		}
		files++
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != logFileMode {
			t.Errorf("file %s mode = %o, want %o", entry.Name(), got, logFileMode)
		}
	}
	if files < 2 {
		t.Fatalf("log files = %d, want the AppendSync and Logger files", files)
	}
}

func TestQuerySkipsCorruptLines(t *testing.T) {
	dir := t.TempDir()
	content := `{"ts":2,"kind":"secret","action":"log"}` + "\n" +
		`{"ts":1,"kind":` + "\n" +
		`{"ts":3,"kind":"drift"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "security-20260101-000000.log"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	result := queryKinds(t, dir, Filter{})
	if result.Skipped != 1 {
		t.Errorf("skipped = %d, want 1 corrupt line", result.Skipped)
	}
	if len(result.Records) != 2 || result.Records[0].Ts != 3 || result.Records[1].Ts != 2 {
		t.Errorf("records = %+v, want valid lines newest-first", result.Records)
	}
}

// TestQueryIgnoresTornTail: the daemon appends to the active file, so the
// last line can be half-written when a query races it. That torn tail must
// not count as an unreadable line — but a corrupt line in the middle still
// does.
func TestQueryIgnoresTornTail(t *testing.T) {
	dir := t.TempDir()
	// Good line + torn tail (no trailing newline): nothing skipped.
	content := `{"ts":1,"kind":"secret"}` + "\n" + `{"ts":2,"kind":`
	if err := os.WriteFile(filepath.Join(dir, "security-20260101-000000.log"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	result := queryKinds(t, dir, Filter{})
	if result.Skipped != 0 {
		t.Errorf("skipped = %d, want 0 for a torn tail (daemon mid-write)", result.Skipped)
	}
	if len(result.Records) != 1 || result.Records[0].Ts != 1 {
		t.Errorf("records = %+v, want the one complete line", result.Records)
	}

	// Corrupt line in the middle + torn tail: only the middle line counts.
	content = `{"ts":1,"kind":"secret"}` + "\n" +
		`{"ts":9,"kind":` + "\n" +
		`{"ts":2,"kind":`
	if err := os.WriteFile(filepath.Join(dir, "security-20260102-000000.log"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	result = queryKinds(t, dir, Filter{Kind: KindSecret})
	if result.Skipped != 1 {
		t.Errorf("skipped = %d, want 1 (middle corrupt line; torn tail ignored)", result.Skipped)
	}
	if len(result.Records) != 2 {
		t.Errorf("records = %d, want the two complete secret lines across both files", len(result.Records))
	}
}

// TestQueryCountsUnopenableFile: a log file that cannot be opened (e.g.
// damaged permissions on a rotated file) must surface in Skipped, not vanish.
func TestQueryCountsUnopenableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can open chmod-000 files; permission failure is not observable")
	}
	dir := t.TempDir()
	blocked := filepath.Join(dir, "security-20260101-000000.log")
	if err := os.WriteFile(blocked, []byte(`{"ts":1,"kind":"secret"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blocked, 0o000); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "security-20260102-000000.log"),
		[]byte(`{"ts":2,"kind":"drift"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := queryKinds(t, dir, Filter{})
	if result.Skipped != 1 {
		t.Errorf("skipped = %d, want 1 for the unopenable file", result.Skipped)
	}
	if len(result.Records) != 1 || result.Records[0].Ts != 2 {
		t.Errorf("records = %+v, want only the readable file's record", result.Records)
	}
}

// TestRecordSchemaHasNoSecretFields pins the audit schema: pattern-type and
// path-category names only, never a field that could carry secret material.
func TestRecordSchemaHasNoSecretFields(t *testing.T) {
	typ := reflect.TypeOf(Record{})
	names := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			t.Fatalf("field %s must have a json name", typ.Field(i).Name)
		}
		names = append(names, strings.Split(tag, ",")[0])
	}
	sort.Strings(names)
	want := []string{"action", "agent", "detail", "exposed", "kind", "names", "protocol", "request_id", "ts"}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("record json fields = %v, want exactly %v", names, want)
	}
	for _, name := range names {
		for _, banned := range []string{"secret", "value", "token", "password", "credential", "body", "payload"} {
			if strings.Contains(name, banned) {
				t.Errorf("field %q looks like it could carry secret material", name)
			}
		}
	}
}

func TestNewRejectsEmptyDirectory(t *testing.T) {
	if _, err := New("", Options{}); err == nil {
		t.Error("New with empty dir must fail")
	}
}

func TestNilLoggerIsSafe(t *testing.T) {
	var logger *Logger
	logger.Enqueue(&Record{Kind: KindSecret})
	logger.Shutdown()
	if logger.Dropped() != 0 || logger.Directory() != "" {
		t.Error("nil logger must be a no-op")
	}
}
