package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// --- newRequestID ---

func TestNewRequestID(t *testing.T) {
	a, b := newRequestID(), newRequestID()
	if len(a) != 32 || len(b) != 32 {
		t.Fatalf("requestID len = %d/%d, want 32 hex chars", len(a), len(b))
	}
	for _, c := range a {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Fatalf("requestID %q has non-hex char", a)
		}
	}
	if a == b {
		t.Errorf("requestIDs collided: %s", a)
	}
}

// --- requestFileWriter: rotation by size and by day ---

// readFileLines reads a file and returns its line count (each JSONL record is
// one line). Used to assert how many records landed in a given file.
func readFileLines(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return bytes.Count(data, []byte("\n"))
}

// firstJSONLineField unmarshals the first JSONL line in path and returns the
// named field, to assert the record content landed intact.
func firstJSONLineField(t *testing.T, path, field string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	nl := bytes.IndexByte(data, '\n')
	line := data
	if nl >= 0 {
		line = data[:nl]
	}
	var m map[string]any
	if err := json.Unmarshal(line, &m); err != nil {
		t.Fatalf("unmarshal first line of %s: %v\nline=%q", path, err, line)
	}
	v, _ := m[field].(string)
	return v
}

func TestRequestFileWriter_RotateBySize(t *testing.T) {
	dir := t.TempDir()
	// A full record is ~313 bytes; cap at 400 so the 1st record fits and the
	// 2nd (curSize+line > 400) forces a rotation.
	const maxSz = 400
	w := &requestFileWriter{dir: dir, maxSize: maxSz}
	start := time.Date(2026, 7, 13, 15, 4, 5, 0, time.UTC)
	w.open(start)
	if w.f == nil {
		t.Fatal("open failed")
	}

	rec := func(i int) *requestLogRecord {
		return &requestLogRecord{Ts: "2026-07-13T15:04:05Z", RequestID: "r", Protocol: "anthropic",
			Method: "POST", Path: "/v1/messages", CalledModel: "glm-5.2", Provider: "aqp",
			Status: 200, RequestBody: "q", ResponseBody: "a"}
	}
	if err := w.write(rec(1), start); err != nil {
		t.Fatalf("write 1: %v", err)
	}
	// Same day, but size now exceeded -> rotate on the 2nd write.
	second := start.Add(1 * time.Second)
	if err := w.write(rec(2), second); err != nil {
		t.Fatalf("write 2: %v", err)
	}
	w.close()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Expect: one archived (start--end) file holding 1 record, one active file
	// holding 1 record.
	var archived, active []os.DirEntry
	for _, e := range entries {
		name := e.Name()
		if strings.Contains(name, "--") {
			archived = append(archived, e)
		} else {
			active = append(active, e)
		}
	}
	if len(archived) != 1 {
		t.Fatalf("expected 1 archived file, got %d: %v", len(archived), entries)
	}
	// Archived name embeds start (150405) and end (150406) timestamps.
	wantArchived := "requests-20260713-150405--20260713-150406-1.log"
	if archived[0].Name() != wantArchived {
		t.Errorf("archived name = %q, want %q", archived[0].Name(), wantArchived)
	}
	if got := readFileLines(t, filepath.Join(dir, archived[0].Name())); got != 1 {
		t.Errorf("archived file has %d lines, want 1 (the pre-rotate record)", got)
	}
	// Active file has the post-rotate record.
	if len(active) != 1 {
		t.Fatalf("expected 1 active file, got %d", len(active))
	}
	if got := readFileLines(t, filepath.Join(dir, active[0].Name())); got != 1 {
		t.Errorf("active file has %d lines, want 1 (the post-rotate record)", got)
	}
}

func TestRequestFileWriter_RotateByDay(t *testing.T) {
	dir := t.TempDir()
	w := &requestFileWriter{dir: dir, maxSize: 1 << 30} // large size, rotation only by day
	day1 := time.Date(2026, 7, 13, 23, 59, 0, 0, time.UTC)
	w.open(day1)
	rec := &requestLogRecord{Ts: "2026-07-13T23:59:00Z", RequestID: "r1", Protocol: "anthropic",
		Method: "POST", Path: "/v1/messages", CalledModel: "glm-5.2", Provider: "aqp",
		Status: 200, RequestBody: "q1", ResponseBody: "a1"}
	if err := w.write(rec, day1); err != nil {
		t.Fatalf("write day1: %v", err)
	}
	// Next request is the next calendar day -> rotate even though size is tiny.
	day2 := time.Date(2026, 7, 14, 0, 1, 0, 0, time.UTC)
	rec2 := &requestLogRecord{Ts: "2026-07-14T00:01:00Z", RequestID: "r2", Protocol: "anthropic",
		Method: "POST", Path: "/v1/messages", CalledModel: "glm-5.2", Provider: "aqp",
		Status: 200, RequestBody: "q2", ResponseBody: "a2"}
	if err := w.write(rec2, day2); err != nil {
		t.Fatalf("write day2: %v", err)
	}
	w.close()

	entries, _ := os.ReadDir(dir)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	// Archived (day1 record, start day1 23:59 -- end day2 00:01) + active (day2).
	var archivedName string
	for _, n := range names {
		if strings.Contains(n, "--") {
			archivedName = n
		}
	}
	if archivedName == "" {
		t.Fatalf("expected an archived (start--end) file, got %v", names)
	}
	// Archived holds exactly the day1 record.
	archivedPath := filepath.Join(dir, archivedName)
	if got := readFileLines(t, archivedPath); got != 1 {
		t.Errorf("archived (day1) file has %d lines, want 1", got)
	}
	if rb := firstJSONLineField(t, archivedPath, "request_body"); rb != "q1" {
		t.Errorf("archived record request_body = %q, want q1 (day1 record)", rb)
	}
	// Active file holds the day2 record.
	var activeName string
	for _, n := range names {
		if !strings.Contains(n, "--") {
			activeName = n
		}
	}
	if activeName == "" {
		t.Fatalf("expected an active file, got %v", names)
	}
	activePath := filepath.Join(dir, activeName)
	if got := readFileLines(t, activePath); got != 1 {
		t.Errorf("active (day2) file has %d lines, want 1", got)
	}
	if rb := firstJSONLineField(t, activePath, "request_body"); rb != "q2" {
		t.Errorf("active record request_body = %q, want q2 (day2 record)", rb)
	}
}

func TestRequestFileWriter_SameDayNoRotate(t *testing.T) {
	dir := t.TempDir()
	w := &requestFileWriter{dir: dir, maxSize: 1 << 30}
	now := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	w.open(now)
	for i := 0; i < 5; i++ {
		if err := w.write(&requestLogRecord{Ts: "t", RequestID: "r", Provider: "a", Status: 200,
			RequestBody: "q", ResponseBody: "a"}, now.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	w.close()
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("expected 1 file (no rotation same-day under size), got %d", len(entries))
	}
	if got := readFileLines(t, filepath.Join(dir, entries[0].Name())); got != 5 {
		t.Errorf("file has %d lines, want 5", got)
	}
}

// TestRequestFileWriter_EmptyFileDroppedOnDayChange verifies that a day-change
// rotation when the current file is empty does NOT leave an empty archived file
// behind - the empty file is removed and a fresh file opened for the new day.
func TestRequestFileWriter_EmptyFileDroppedOnDayChange(t *testing.T) {
	dir := t.TempDir()
	w := &requestFileWriter{dir: dir, maxSize: 1 << 30}
	day1 := time.Date(2026, 7, 13, 23, 59, 0, 0, time.UTC)
	w.open(day1)
	// No records written on day1 -> curSize == 0. First record lands on day2.
	day2 := time.Date(2026, 7, 14, 0, 1, 0, 0, time.UTC)
	if err := w.write(&requestLogRecord{Ts: "t", RequestID: "r", Provider: "a", Status: 200,
		RequestBody: "q", ResponseBody: "a"}, day2); err != nil {
		t.Fatalf("write: %v", err)
	}
	w.close()
	entries, _ := os.ReadDir(dir)
	// Exactly one file: the day2 active file. No empty day1 archive.
	if len(entries) != 1 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("expected 1 file (empty day1 file dropped), got %d: %v", len(entries), names)
	}
	if strings.Contains(entries[0].Name(), "--") {
		t.Errorf("should not have an archived (empty) file: %s", entries[0].Name())
	}
	if got := readFileLines(t, filepath.Join(dir, entries[0].Name())); got != 1 {
		t.Errorf("day2 file has %d lines, want 1", got)
	}
}

// TestRequestFileWriter_SameSecondRotationsDisambiguated verifies that two
// rotations within the same wall-clock second produce distinct archive names
// (the -<seq> suffix), so the second rename doesn't clobber the first.
func TestRequestFileWriter_SameSecondRotationsDisambiguated(t *testing.T) {
	dir := t.TempDir()
	// Tiny cap so each record forces a rotation; all within the same second.
	w := &requestFileWriter{dir: dir, maxSize: 1}
	start := time.Date(2026, 7, 13, 15, 4, 5, 0, time.UTC)
	w.open(start)
	rec := &requestLogRecord{Ts: "t", RequestID: "r", Provider: "a", Status: 200,
		RequestBody: "q", ResponseBody: "a"}
	// Three writes, all at the same `now` -> 2 rotations (writes 2 and 3 each
	// exceed the cap), both archived with start=150405 end=150405 but seq 1, 2.
	for i := 0; i < 3; i++ {
		if err := w.write(rec, start); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	w.close()
	entries, _ := os.ReadDir(dir)
	var archives []string
	for _, e := range entries {
		if strings.Contains(e.Name(), "--") {
			archives = append(archives, e.Name())
		}
	}
	if len(archives) != 2 {
		t.Fatalf("expected 2 archives (2 rotations), got %d: %v", len(archives), archives)
	}
	if archives[0] == archives[1] {
		t.Errorf("archives collided: %s (seq suffix should disambiguate)", archives[0])
	}
	// Each archive holds exactly 1 record (no clobber).
	for _, a := range archives {
		if got := readFileLines(t, filepath.Join(dir, a)); got != 1 {
			t.Errorf("archive %s has %d lines, want 1 (clobbered?)", a, got)
		}
	}
}

// TestRequestFileWriter_FilePerms0600 verifies log files are created owner-only
// (0o600) since they contain user code/prompts.
func TestRequestFileWriter_FilePerms0600(t *testing.T) {
	dir := t.TempDir()
	w := &requestFileWriter{dir: dir, maxSize: 1 << 30}
	w.open(time.Date(2026, 7, 13, 15, 4, 5, 0, time.UTC))
	if w.f == nil {
		t.Fatal("open failed")
	}
	w.write(&requestLogRecord{Ts: "t", RequestID: "r", Provider: "a", Status: 200,
		RequestBody: "q", ResponseBody: "a"}, time.Date(2026, 7, 13, 15, 4, 5, 0, time.UTC))
	w.close()
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			continue
		}
		// Mask off file-type bits; want 0o600.
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("file %s perm = %o, want 0600 (sensitive bodies)", e.Name(), got)
		}
	}
}

// --- logger: drop-on-full + loop flush + shutdown durability ---

func TestRequestLogger_DropsOnFull(t *testing.T) {
	dir := t.TempDir()
	l := newRequestLogger(dir, 1<<30, 1024, 0)
	// Don't start the loop - so the channel fills up deterministically and record
	// must NOT block (it drops instead).
	for i := 0; i < 3000; i++ {
		l.record(&requestLogRecord{Ts: "t", RequestID: "r"})
	}
	if atomic.LoadUint64(&l.dropped) == 0 {
		t.Error("expected dropped > 0 when channel is full, got 0 (record must not block)")
	}
}

func TestRequestLogger_LoopWritesAndShutdownDurability(t *testing.T) {
	dir := t.TempDir()
	l := newRequestLogger(dir, 1<<30, 4096, 0)
	go l.loop()
	// Enqueue 250 records then shutdown - all must land in the file.
	for i := 0; i < 250; i++ {
		l.record(&requestLogRecord{Ts: "t", RequestID: "r", SessionID: "sess",
			Protocol: "anthropic", Method: "POST", Path: "/v1/messages",
			CalledModel: "glm-5.2", Provider: "aqp", Status: 200,
			RequestBody: "q", ResponseBody: "a"})
	}
	l.shutdown() // drain + write + close

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var total int
	for _, e := range entries {
		total += readFileLines(t, filepath.Join(dir, e.Name()))
	}
	if total != 250 {
		t.Errorf("after shutdown, %d records persisted, want 250 (no loss)", total)
	}
	if atomic.LoadUint64(&l.dropped) != 0 {
		t.Errorf("dropped = %d, want 0 (no overflow expected)", atomic.LoadUint64(&l.dropped))
	}
}

// TestRequestLogger_WriteErrorsCounted verifies that every write failure is
// counted. The injected writer makes the failure deterministic across users,
// filesystems, and platforms.
func TestRequestLogger_WriteErrorsCounted(t *testing.T) {
	dir := t.TempDir()
	l := newRequestLogger(dir, 1<<30, 4096, 0)
	l.writeRecord = func(*requestLogRecord, time.Time) error {
		return errors.New("injected write failure")
	}
	go l.loop()
	for i := 0; i < 5; i++ {
		l.record(&requestLogRecord{Ts: "t", RequestID: "r", Provider: "a", Status: 200,
			RequestBody: "q", ResponseBody: "a"})
	}
	l.shutdown()
	if got := atomic.LoadUint64(&l.writeErrors); got != 5 {
		t.Fatalf("writeErrors = %d, want 5", got)
	}
}

// TestRequestLogger_RetentionSweepDeletesOldArchives verifies the sweep deletes
// rotated archive files older than retention, while keeping the active file and
// recent archives.
func TestRequestLogger_RetentionSweepDeletesOldArchives(t *testing.T) {
	dir := t.TempDir()
	// Create an old archive (mtime 40 days ago) - should be deleted.
	oldArch := filepath.Join(dir, "requests-20260601-000000--20260601-010000-1.log")
	if err := os.WriteFile(oldArch, []byte(`{"ts":"old"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(oldArch, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	// Create a recent archive (mtime 1 day ago) - should be kept.
	recentArch := filepath.Join(dir, "requests-20260712-000000--20260712-010000-1.log")
	if err := os.WriteFile(recentArch, []byte(`{"ts":"recent"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Create an active file (no "--") - should ALWAYS be kept even if old.
	oldActive := filepath.Join(dir, "requests-20260601-000000.log")
	if err := os.WriteFile(oldActive, []byte(`{"ts":"active-old"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(oldActive, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	// Create the CURRENT active file (old mtime) — the sweep's curActive
	// exemption must keep it even though it's past cutoff. Without a real file
	// on disk the exemption branch is never exercised.
	curActive := filepath.Join(dir, "requests-20990101-000000.log")
	if err := os.WriteFile(curActive, []byte(`{"ts":"current"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(curActive, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	// retention 30d -> sweep deletes oldArch (40d) AND the orphaned old active
	// file (40d, left by a previous run — F6a: restart orphans are now reclaimed,
	// not held forever). recentArch (1d) is kept; curActive is exempt even if old.
	l := newRequestLogger(dir, 1<<30, 4096, 30*24*time.Hour)
	l.sweep(time.Now(), curActive)

	for _, tc := range []struct {
		name string
		path string
		want bool // wantExists
	}{
		{"old archive", oldArch, false},
		{"recent archive", recentArch, true},
		{"orphaned active file (old)", oldActive, false},
		{"current active file (exempt)", curActive, true},
	} {
		_, err := os.Stat(tc.path)
		exists := !os.IsNotExist(err)
		if exists != tc.want {
			t.Errorf("%s: exists=%v, want %v", tc.name, exists, tc.want)
		}
	}
}

// TestRequestLogger_RetentionZeroKeepsAll verifies retention 0 (keep forever)
// deletes nothing.
func TestRequestLogger_RetentionZeroKeepsAll(t *testing.T) {
	dir := t.TempDir()
	oldArch := filepath.Join(dir, "requests-20260601-000000--20260601-010000-1.log")
	os.WriteFile(oldArch, []byte(`{}`+"\n"), 0o600)
	oldTime := time.Now().Add(-400 * 24 * time.Hour)
	os.Chtimes(oldArch, oldTime, oldTime)

	l := newRequestLogger(dir, 1<<30, 4096, 0) // 0 = forever
	l.sweep(time.Now(), "")
	if _, err := os.Stat(oldArch); err != nil {
		t.Errorf("retention 0 should keep old archive, got err: %v", err)
	}
}

// --- buildRecord ---

func TestBuildRecord_FieldsAndTruncation(t *testing.T) {
	dir := t.TempDir()
	const maxBody = 64
	l := newRequestLogger(dir, 1<<30, maxBody, 0)

	// Build a real resp via a httptest upstream so resp.Header is real.
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.Header().Set("x-request-id", "up-rid-123")
		w.Write([]byte("data: hi\n\n"))
	}))
	defer up.Close()
	resp, err := http.Post(up.URL, "application/json", strings.NewReader(`{"model":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"glm-5.2"}`))
	start := time.Now().Add(-77 * time.Millisecond)
	flc := forwardLogCtx{requestID: "rid-abc", attempt: 2, exposed: "glm-5.2"}
	tgt := RouteTarget{Provider: "aqp", Model: "glm-5.2"}
	reqBody := []byte(`{"model":"glm-5.2"}`)
	captured := []byte("data: hi\n\n")

	rec := l.buildRecord(recordInputs{
		flc: flc, r: req, proto: "anthropic", calledModel: "glm-5.2", t: tgt,
		resp: resp, start: start, requestBody: reqBody, captured: captured,
		total: int64(len(captured)), truncated: false,
	})
	// Exact field assertions (not "non-empty").
	if rec.RequestID != "rid-abc" || rec.Attempt != 2 || rec.Exposed != "glm-5.2" {
		t.Errorf("flc fields = %q/%d/%q", rec.RequestID, rec.Attempt, rec.Exposed)
	}
	if rec.Protocol != "anthropic" || rec.Method != "POST" || rec.Path != "/v1/messages" {
		t.Errorf("proto/method/path = %q/%q/%q", rec.Protocol, rec.Method, rec.Path)
	}
	if rec.CalledModel != "glm-5.2" || rec.UpstreamModel != "glm-5.2" || rec.Provider != "aqp" {
		t.Errorf("models/provider = %q/%q/%q", rec.CalledModel, rec.UpstreamModel, rec.Provider)
	}
	if rec.Status != 200 {
		t.Errorf("status = %d, want 200", rec.Status)
	}
	if rec.RequestBody != `{"model":"glm-5.2"}` {
		t.Errorf("RequestBody = %q, want exact", rec.RequestBody)
	}
	if rec.ResponseBody != "data: hi\n\n" {
		t.Errorf("ResponseBody = %q, want exact", rec.ResponseBody)
	}
	if rec.ResponseSize != int64(len(captured)) {
		t.Errorf("ResponseSize = %d, want %d", rec.ResponseSize, len(captured))
	}
	if !strings.Contains(rec.ResponseHeaders, `"x-request-id":"up-rid-123"`) {
		t.Errorf("ResponseHeaders = %q, want x-request-id captured", rec.ResponseHeaders)
	}
	if rec.LatencyMs < 77 {
		t.Errorf("LatencyMs = %d, want >= 77", rec.LatencyMs)
	}
	// The record must marshal to a single line (JSONL invariant).
	line, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if bytes.Count(line, []byte("\n")) != 0 {
		t.Errorf("record must marshal to one line (no embedded newline), got %d", bytes.Count(line, []byte("\n")))
	}

	// Truncation: body over cap -> marker appended, size is the original.
	big := bytes.Repeat([]byte("q"), maxBody+50)
	rec2 := l.buildRecord(recordInputs{
		flc: flc, r: req, proto: "anthropic", calledModel: "glm-5.2", t: tgt,
		resp: resp, start: start, requestBody: big, captured: big,
		total: int64(len(big)), truncated: true,
	})
	if rec2.RequestSize != len(big) {
		t.Errorf("truncated RequestSize = %d, want %d (original size, not capped)", rec2.RequestSize, len(big))
	}
	if !strings.HasSuffix(rec2.RequestBody, truncMarker) {
		t.Errorf("truncated RequestBody should end with truncMarker, got suffix %q", rec2.RequestBody[len(rec2.RequestBody)-40:])
	}
	if len(rec2.RequestBody) != maxBody+len(truncMarker) {
		t.Errorf("truncated RequestBody len = %d, want %d", len(rec2.RequestBody), maxBody+len(truncMarker))
	}
	if rec2.ResponseSize != int64(len(big)) {
		t.Errorf("truncated ResponseSize = %d, want %d (total, not capped)", rec2.ResponseSize, len(big))
	}
	if !strings.HasSuffix(rec2.ResponseBody, truncMarker) {
		t.Errorf("truncated ResponseBody should end with truncMarker")
	}
}

// --- end-to-end forward: bodies captured exactly ---

// newReqLogProxy builds a Proxy wired with a real requestLogger (temp dir +
// running loop) so forward() captures bodies. Returns the proxy + dir + a
// shutdown func (call after the request to flush).
func newReqLogProxy(t *testing.T, cfg *Config) (*Proxy, string, func()) {
	t.Helper()
	p := newTestProxy(t, cfg)
	dir := t.TempDir()
	l := newRequestLogger(dir, 1<<30, 1<<20, 0) // 1MiB body cap, plenty for test bodies
	go l.loop()
	p.reqLog = l
	return p, dir, func() { l.shutdown() }
}

// allRecords reads every .log file in dir and returns the concatenated parsed
// records (order across files is not guaranteed; tests here produce 1 record).
func allRecords(t *testing.T, dir string) []requestLogRecord {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []requestLogRecord
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range bytes.Split(data, []byte("\n")) {
			if len(line) == 0 {
				continue
			}
			var r requestLogRecord
			if err := json.Unmarshal(line, &r); err != nil {
				t.Fatalf("unmarshal line: %v\nline=%q", err, line)
			}
			out = append(out, r)
		}
	}
	return out
}

func TestForward_RequestLog_CapturesBodies_NonSSE(t *testing.T) {
	const respBody = `{"id":"msg_1","content":"hello"}`
	var upReceivedBody []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upReceivedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(respBody))
	}))
	defer up.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"aqp": {OpenAIBaseURL: up.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"glm-5.2": {{Provider: "aqp", Model: "glm-5.2"}},
		},
	}
	p, dir, shutdown := newReqLogProxy(t, cfg)
	p.providers["aqp"] = &testProv{key: "tok"}
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	const reqBody = `{"model":"glm-5.2","input":[{"role":"user","content":"hi"}]}`
	post(t, px.URL+"/v1/responses", reqBody)
	shutdown() // flush the enqueued record to disk before reading

	recs := allRecords(t, dir)
	if len(recs) != 1 {
		t.Fatalf("expected 1 request log record, got %d (captureReader wrap must produce one)", len(recs))
	}
	r := recs[0]
	// The logged request body must equal what the upstream received - EXACT.
	if r.RequestBody != reqBody {
		t.Errorf("logged RequestBody = %q, want exact %q", r.RequestBody, reqBody)
	}
	if r.RequestBody != string(upReceivedBody) {
		t.Errorf("logged RequestBody != upstream-received body %q", upReceivedBody)
	}
	if r.ResponseBody != respBody {
		t.Errorf("logged ResponseBody = %q, want exact %q", r.ResponseBody, respBody)
	}
	if r.Status != 200 {
		t.Errorf("logged Status = %d, want 200", r.Status)
	}
	if r.Provider != "aqp" || r.CalledModel != "glm-5.2" || r.UpstreamModel != "glm-5.2" {
		t.Errorf("logged Provider/Models = %q/%q/%q, want aqp/glm-5.2/glm-5.2", r.Provider, r.CalledModel, r.UpstreamModel)
	}
	if r.Protocol != "responses" {
		t.Errorf("logged Protocol = %q, want responses (/v1/responses path)", r.Protocol)
	}
	if r.RequestID == "" {
		t.Errorf("logged RequestID is empty, want a non-empty id")
	}
}

func TestForward_RequestLog_CapturesBodies_SSE(t *testing.T) {
	// SSE stream: usageScanner wraps captureReader (p.tokens != nil). The full
	// event stream must be captured verbatim, and the cumulative token counter
	// must still accrue (token path unchanged by request logging).
	const sseBody = "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":42,"cache_creation_input_tokens":3,"cache_read_input_tokens":0}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":7}}` + "\n\n" +
		"data: [DONE]\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		w.Write([]byte(sseBody))
	}))
	defer up.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"aqp": {OpenAIBaseURL: up.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"glm-5.2": {{Provider: "aqp", Model: "glm-5.2"}},
		},
	}
	p, dir, shutdown := newReqLogProxy(t, cfg)
	p.providers["aqp"] = &testProv{key: "tok"}
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	post(t, px.URL+"/v1/responses", `{"model":"glm-5.2","input":[]}`)
	shutdown()

	recs := allRecords(t, dir)
	if len(recs) != 1 {
		t.Fatalf("expected 1 SSE request log record, got %d", len(recs))
	}
	// The entire SSE event stream captured verbatim (scanner is pass-through).
	if recs[0].ResponseBody != sseBody {
		t.Errorf("logged SSE ResponseBody != upstream stream\n got: %q\nwant: %q", recs[0].ResponseBody, sseBody)
	}
	if recs[0].Status != 200 {
		t.Errorf("Status = %d, want 200", recs[0].Status)
	}
	// Token path UNCHANGED by request logging: cumulative counter still accrued.
	snap := p.tokens.snapshot()
	tu, ok := snap[pmKey{Provider: "aqp", Model: "glm-5.2"}]
	if !ok {
		t.Fatal("token counter has no aqp/glm-5.2 entry (scanner should have run)")
	}
	if tu.Input != 42 || tu.Output != 7 || tu.CacheCreation != 3 {
		t.Errorf("cumulative tokens = input=%d output=%d cache_create=%d, want 42/7/3 (scanner unaffected)", tu.Input, tu.Output, tu.CacheCreation)
	}
}

func TestForward_RequestLog_NilLoggerPassThrough(t *testing.T) {
	// reqLog == nil (disabled): no capture, but the response must still reach the
	// client intact (nil-safety, zero overhead).
	const respBody = `{"ok":true}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(respBody))
	}))
	defer up.Close()
	cfg := &Config{
		Providers: map[string]Provider{"aqp": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"glm-5.2": {{Provider: "aqp", Model: "glm-5.2"}}},
	}
	p := newTestProxy(t, cfg) // p.reqLog stays nil
	p.providers["aqp"] = &testProv{key: "tok"}
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	resp, err := http.Post(px.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"glm-5.2","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200 (nil reqLog must not break forwarding)", resp.StatusCode)
	}
	if string(body) != respBody {
		t.Errorf("body = %q, want %q (pass-through intact with nil reqLog)", body, respBody)
	}
	if p.reqLog != nil {
		t.Error("p.reqLog should be nil for a plain NewProxy (disabled by default)")
	}
}

// TestReload_WarnsWhenRequestLogEnabledButInactive verifies reload() logs a
// warning when the new config has request_log.enabled:true but the logger isn't
// running (p.reqLog == nil) - so enabling via SIGHUP isn't a silent no-op.
func TestReload_WarnsWhenRequestLogEnabledButInactive(t *testing.T) {
	useStaticProviderPools(t, "p")
	// Capture log output.
	var buf bytes.Buffer
	origOut := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(origOut)
		log.SetFlags(origFlags)
	}()

	cfgYAML := "listen: 127.0.0.1:0\nproviders:\n  p:\n    openai_base_url: http://127.0.0.1:1\n    provider_id: static\n" +
		"routes:\n  m:\n    - {provider: p, model: m}\n" +
		"request_log:\n  enabled: true\n"
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	p := newTestProxy(t, cfg)
	// p.reqLog stays nil -> reload should warn.
	if err := p.reload(cfgPath); err != nil {
		t.Fatalf("reload: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "request_log.enabled is true") || !strings.Contains(out, "restart the daemon") {
		t.Errorf("reload should warn about enabled-but-inactive request_log, got log:\n%s", out)
	}
}

// TestReload_NoWarnWhenRequestLogDisabled verifies reload() does NOT warn when
// request_log is disabled (the common case).
func TestReload_NoWarnWhenRequestLogDisabled(t *testing.T) {
	useStaticProviderPools(t, "p")
	var buf bytes.Buffer
	origOut := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(origOut)
		log.SetFlags(origFlags)
	}()

	cfgYAML := "listen: 127.0.0.1:0\nproviders:\n  p:\n    openai_base_url: http://127.0.0.1:1\n    provider_id: static\n" +
		"routes:\n  m:\n    - {provider: p, model: m}\n"
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	p := newTestProxy(t, cfg)
	if err := p.reload(cfgPath); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if strings.Contains(buf.String(), "request_log.enabled is true") {
		t.Errorf("reload should NOT warn when request_log disabled, got:\n%s", buf.String())
	}
}

// TestQueryRequestRecords_LargeLineDoesNotLoseTail (regression #4): a valid JSONL
// line larger than bufio.Scanner's 8 MiB cap — the default 5 MiB body cap allows
// ~10 MiB request+response lines — must not halt reading. The Scanner aborted at
// the first oversized line and silently ignored the error, losing that record
// AND every record after it in the file (unqueryable, unreplayable).
func TestQueryRequestRecords_LargeLineDoesNotLoseTail(t *testing.T) {
	dir := t.TempDir()
	// One oversized (>8 MiB) but valid record, then one normal record after it.
	oversized := requestLogRecord{RequestID: "oversized", RequestBody: strings.Repeat("x", 9*1024*1024)}
	after := requestLogRecord{RequestID: "after", CalledModel: "glm-5"}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, r := range []requestLogRecord{oversized, after} {
		if err := enc.Encode(&r); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "requests-20260720-120000.log"), buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	recs, err := queryRequestRecords(dir, recordFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range recs {
		got[r.RequestID] = true
	}
	if !got["oversized"] || !got["after"] {
		t.Errorf("missing records: got %v, want both oversized+after (oversized line halted the scanner, losing it + the tail)", got)
	}
}

// TestQueryRequestRecords_BoundedToNewestLimit (follow-up to #4): with a small
// Limit and many matching records, the query returns exactly the newest Limit.
// Guards the bounded top-K collector refactor's correctness (the memory bound —
// retaining ~Limit records, not the whole matching set — is by construction via
// the min-heap; this test proves it still returns the right records).
func TestQueryRequestRecords_BoundedToNewestLimit(t *testing.T) {
	dir := t.TempDir()
	ts := func(sec int64) string { return time.Unix(sec, 0).UTC().Format(time.RFC3339) }
	write := func(name string, count, baseTs int64) {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		for i := int64(0); i < count; i++ {
			if err := enc.Encode(requestLogRecord{RequestID: name, Ts: ts(baseTs + i)}); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(dir, name), buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Three time-disjoint files (names sort oldest→first; query reads newest-first).
	// The newest (active) file has far more records than the Limit.
	write("requests-20260701-000000.log", 50, 1000)   // oldest: sec 1000..1049
	write("requests-20260702-000000.log", 50, 2000)   // mid:    sec 2000..2049
	write("requests-20260703-000000.log", 5000, 3000) // newest:  sec 3000..7999

	const limit = 100
	recs, err := queryRequestRecords(dir, recordFilter{Limit: limit})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != limit {
		t.Fatalf("got %d records, want %d (the newest Limit)", len(recs), limit)
	}
	// The newest file's 5000 records swamp the Limit; the cross-file early break
	// skips the older files. All kept records are the newest file's NEWEST `limit`,
	// in descending Ts order (sec 7999 down to 7900).
	for i, r := range recs {
		if r.RequestID != "requests-20260703-000000.log" {
			t.Errorf("rec[%d] from %s, want the newest file", i, r.RequestID)
		}
		if i > 0 && r.Ts >= recs[i-1].Ts {
			t.Errorf("rec[%d] Ts=%q not strictly descending after rec[%d] Ts=%q", i, r.Ts, i-1, recs[i-1].Ts)
		}
	}
	if recs[0].Ts != ts(7999) {
		t.Errorf("newest rec Ts=%q, want %q", recs[0].Ts, ts(7999))
	}
	if recs[len(recs)-1].Ts != ts(7999-int64(limit)+1) {
		t.Errorf("oldest kept Ts=%q, want %q (the Limit-th newest)", recs[len(recs)-1].Ts, ts(7999-int64(limit)+1))
	}
}
