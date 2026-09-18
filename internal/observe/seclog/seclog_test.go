package seclog

import (
	"fmt"
	"model-proxy/internal/observe/logfile"
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

// jsonlLines reads every JSONL trail line in dir (the full-fidelity half).
func jsonlLines(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, filePrefix) || !strings.HasSuffix(name, ".log") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if line != "" {
				lines = append(lines, line)
			}
		}
	}
	return lines
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
			SessionID: fmt.Sprintf("sess-%d", i%2),
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
		newest.SessionID != "sess-0" ||
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

// Verdict fields (reason/evidence/model) ride along the store round-trip —
// they are what the analyze view reads back.
func TestVerdictFieldsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	logger := startLogger(t, dir, Options{})
	logger.Enqueue(&Record{
		Ts: 1000, Kind: KindSecret, RequestID: "r1", Names: []string{"jwt"},
		Verdict: "medium", Reason: "path reference without live material", Evidence: "tool input mentions ~/.ssh/id_rsa", Model: "glm-5.3-flash",
	})
	logger.Shutdown()

	result := queryKinds(t, dir, Filter{})
	if len(result.Records) != 1 {
		t.Fatalf("records = %d, want 1", len(result.Records))
	}
	rec := result.Records[0]
	if rec.Verdict != "medium" || rec.Reason != "path reference without live material" ||
		rec.Evidence != "tool input mentions ~/.ssh/id_rsa" || rec.Model != "glm-5.3-flash" {
		t.Errorf("verdict fields did not round-trip: %+v", rec)
	}
}

// The ignored tier: low verdicts land in the full JSONL trail but never in
// the queryable store.
func TestLowVerdictStaysOutOfQueryableStore(t *testing.T) {
	dir := t.TempDir()
	logger := startLogger(t, dir, Options{})
	logger.Enqueue(&Record{Ts: 1000, Kind: KindSecret, Names: []string{"ssh"}, Verdict: "low", Reason: "test fixture"})
	logger.Enqueue(&Record{Ts: 2000, Kind: KindSecret, Names: []string{"jwt"}, Verdict: "medium", Reason: "ambiguous"})
	logger.Enqueue(&Record{Ts: 3000, Kind: KindSecret, Names: []string{"pem_private_key"}})
	logger.Shutdown()

	if lines := jsonlLines(t, dir); len(lines) != 3 {
		t.Fatalf("JSONL trail lines = %d, want 3 (trail is full-fidelity)", len(lines))
	}
	if !strings.Contains(strings.Join(jsonlLines(t, dir), "\n"), `"verdict":"low"`) {
		t.Error("JSONL trail must carry the low verdict record")
	}
	result := queryKinds(t, dir, Filter{})
	if len(result.Records) != 2 {
		t.Fatalf("queryable records = %d, want 2 (low excluded)", len(result.Records))
	}
	for _, rec := range result.Records {
		if rec.Verdict == "low" {
			t.Errorf("low verdict leaked into the queryable store: %+v", rec)
		}
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
		if strings.HasPrefix(entry.Name(), filePrefix) && strings.HasSuffix(entry.Name(), ".log") {
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
	// The live record survives: the active file is never swept, and the store
	// row is fresh — the row sweep removes only rows older than the window
	// (and the file sweep never touches security.db, whose name does not
	// match the audit-file scheme).
	if _, err := os.Stat(storePath(dir)); err != nil {
		t.Errorf("security.db must survive a retention sweep, stat err = %v", err)
	}
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
	// The SQLite half honors the same keep-forever policy.
	result := queryKinds(t, dir, Filter{})
	if len(result.Records) != 1 {
		t.Fatalf("records = %d, want 1 (retention=0 sweeps nothing)", len(result.Records))
	}
}

// The SQLite half shares the JSONL half's retention window: rows older than
// the window are swept (startup/hourly/shutdown cadence), so the queryable
// store cannot accumulate audit rows long after the rotated trail files are
// gone. Regression: an expired row and a live one; the shutdown sweep must
// delete exactly the expired row.
func TestStoreRetentionSweepDeletesExpiredRows(t *testing.T) {
	dir := t.TempDir()
	expired := time.Now().Add(-2 * time.Hour).UnixMilli()
	logger := startLogger(t, dir, Options{Retention: time.Hour})
	logger.Enqueue(&Record{Ts: expired, Kind: KindSecret, Names: []string{"jwt"}, Action: "log"})
	logger.Enqueue(&Record{Kind: KindSecret, Names: []string{"jwt"}, Action: "log"}) // zero Ts stamps now
	logger.Shutdown()

	result := queryKinds(t, dir, Filter{})
	if len(result.Records) != 1 {
		t.Fatalf("records after sweep = %d, want 1 (expired row deleted)", len(result.Records))
	}
	if result.Records[0].Ts == expired {
		t.Errorf("expired row survived the store sweep: %+v", result.Records[0])
	}
	// The JSONL trail is file-granular: both lines are in today's active
	// file, which the sweep never removes.
	if lines := jsonlLines(t, dir); len(lines) != 2 {
		t.Errorf("JSONL trail lines = %d, want 2 (file-level sweep keeps the active file)", len(lines))
	}
}

// A startup sweep clears rows that aged past the window while the daemon was
// down (no writes happened to trigger the write-path sweep).
func TestStoreRetentionSweepAtStartup(t *testing.T) {
	dir := t.TempDir()
	expired := time.Now().Add(-2 * time.Hour).UnixMilli()
	if err := AppendSync(dir, &Record{Ts: expired, Kind: KindDrift, Detail: "client=claude"}); err != nil {
		t.Fatal(err)
	}
	logger := startLogger(t, dir, Options{Retention: time.Hour})
	logger.Shutdown()

	result := queryKinds(t, dir, Filter{})
	if len(result.Records) != 0 {
		t.Fatalf("records after startup sweep = %d, want 0", len(result.Records))
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
		t.Fatalf("records = %d, want 3 from AppendSync and Logger writers", len(result.Records))
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
	// Run is deliberately not started, so the shared sink queue fills
	// deterministically and the non-blocking offer starts dropping.
	for i := 0; i < logfile.DefaultQueueCapacity+10; i++ {
		logger.Enqueue(&Record{Kind: KindSecret})
	}
	if got := logger.Dropped(); got != 10 {
		t.Errorf("dropped = %d, want 10", got)
	}
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
	// The store file is owner-only like the JSONL trail.
	dbInfo, err := os.Stat(storePath(dir))
	if err != nil {
		t.Fatal(err)
	}
	if got := dbInfo.Mode().Perm(); got != logFileMode {
		t.Errorf("security.db mode = %o, want %o", got, logFileMode)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var files int
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, filePrefix) || !strings.HasSuffix(name, ".log") {
			continue
		}
		files++
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != logFileMode {
			t.Errorf("file %s mode = %o, want %o", name, got, logFileMode)
		}
	}
	if files < 1 {
		t.Fatalf("log files = %d, want at least the shared per-day AppendSync/Logger file", files)
	}
	if result := queryKinds(t, dir, Filter{}); len(result.Records) != 2 {
		t.Fatalf("records in shared store = %d, want 2", len(result.Records))
	}
}

// Query reads the SQLite store only: legacy JSONL files (or any hand-dropped
// audit file) are invisible to the query surface by design.
func TestQueryIgnoresPlainJSONLFiles(t *testing.T) {
	dir := t.TempDir()
	content := `{"ts":2,"kind":"secret","action":"log"}` + "\n" + `{"ts":3,"kind":"drift"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "security-20260101-000000.log"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	result := queryKinds(t, dir, Filter{})
	if len(result.Records) != 0 || result.Truncated || result.Skipped != 0 {
		t.Errorf("records = %+v, want an empty result over a JSONL-only directory", result.Records)
	}
	if _, err := os.Stat(storePath(dir)); !os.IsNotExist(err) {
		t.Errorf("query must not create a store, stat err = %v", err)
	}
}

// A directory with no storage at all is an empty store, not an error.
func TestQueryMissingStoreIsEmpty(t *testing.T) {
	dir := t.TempDir()
	result := queryKinds(t, dir, Filter{})
	if len(result.Records) != 0 || result.Truncated {
		t.Errorf("records = %+v, want empty", result.Records)
	}
}

// TestQueryReportsTruncation pins the Truncated contract: it is true exactly
// when a positive Limit dropped at least one matching record, false when the
// limit is not reached or there is no limit at all. This is what `audit
// --stats` relies on to flag that its counts are a newest-first prefix, not
// the whole filtered set.
func TestQueryReportsTruncation(t *testing.T) {
	dir := t.TempDir()
	for ts := int64(1); ts <= 5; ts++ {
		if err := AppendSync(dir, &Record{Ts: ts, Kind: KindDrift, Agent: "doctor", Detail: "n"}); err != nil {
			t.Fatalf("AppendSync ts=%d: %v", ts, err)
		}
	}

	result, err := Query(dir, Filter{Limit: 2})
	if err != nil {
		t.Fatalf("Query limit=2: %v", err)
	}
	if len(result.Records) != 2 {
		t.Fatalf("records = %d, want 2", len(result.Records))
	}
	if !result.Truncated {
		t.Error("Truncated = false with 5 matches and limit 2, want true")
	}

	// Limit exactly reached: nothing was dropped, so no truncation.
	result, err = Query(dir, Filter{Limit: 5})
	if err != nil {
		t.Fatalf("Query limit=5: %v", err)
	}
	if len(result.Records) != 5 || result.Truncated {
		t.Fatalf("limit=5: records = %d, Truncated = %v; want 5, false", len(result.Records), result.Truncated)
	}

	// No limit: everything is retained, truncation is meaningless.
	result, err = Query(dir, Filter{})
	if err != nil {
		t.Fatalf("Query unlimited: %v", err)
	}
	if len(result.Records) != 5 || result.Truncated {
		t.Fatalf("unlimited: records = %d, Truncated = %v; want 5, false", len(result.Records), result.Truncated)
	}

	// No matches at all: never truncated.
	result, err = Query(dir, Filter{Kind: KindSecret, Limit: 2})
	if err != nil {
		t.Fatalf("Query kind=secret: %v", err)
	}
	if len(result.Records) != 0 || result.Truncated {
		t.Fatalf("kind=secret: records = %d, Truncated = %v; want 0, false", len(result.Records), result.Truncated)
	}
}

// Truncated stays exact under a kind filter (the count predicate must match
// the select predicate).
func TestQueryTruncationUnderKindFilter(t *testing.T) {
	dir := t.TempDir()
	for ts := int64(1); ts <= 10; ts++ {
		kind := KindSecret
		if ts <= 3 {
			kind = KindDrift
		}
		if err := AppendSync(dir, &Record{Ts: ts, Kind: kind}); err != nil {
			t.Fatalf("AppendSync ts=%d: %v", ts, err)
		}
	}
	result := queryKinds(t, dir, Filter{Kind: KindDrift, Limit: 10})
	if len(result.Records) != 3 || result.Truncated {
		t.Fatalf("drift records = %d, Truncated = %v; want 3, false", len(result.Records), result.Truncated)
	}
	result = queryKinds(t, dir, Filter{Kind: KindSecret, Limit: 3})
	if len(result.Records) != 3 || !result.Truncated {
		t.Fatalf("secret records = %d, Truncated = %v; want 3, true", len(result.Records), result.Truncated)
	}
}

// The store tolerates an offline writer (CLI-style AppendSync) hammering it
// while the daemon Logger is running — WAL + busy_timeout is the whole story.
func TestOfflineWriterCoexistsWithRunningLogger(t *testing.T) {
	dir := t.TempDir()
	logger := startLogger(t, dir, Options{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 25; i++ {
			logger.Enqueue(&Record{Ts: int64(1000 + i), Kind: KindSecret, Action: "log"})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 25; i++ {
			if err := AppendSync(dir, &Record{Ts: int64(2000 + i), Kind: KindDrift}); err != nil {
				t.Errorf("offline AppendSync %d: %v", i, err)
				return
			}
		}
	}()
	wg.Wait()
	logger.Shutdown()

	result := queryKinds(t, dir, Filter{})
	if len(result.Records) != 50 {
		t.Fatalf("records = %d, want 50 from both writers", len(result.Records))
	}
}

// TestRecordSchemaHasNoSecretFields pins the audit schema: pattern-type and
// path-category names only, never a field that could carry secret material.
// Reason/Evidence are scrubbed model text by contract (the adjudication
// channel masks hit bytes before they land here).
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
	want := []string{"action", "agent", "detail", "evidence", "exposed", "kind", "model", "names", "protocol", "reason", "request_id", "session_id", "ts", "verdict"}
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
