package requestlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestFileWriterRotateBySize(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 7, 13, 15, 4, 5, 0, time.UTC)
	record := &Record{
		Ts: "2026-07-13T15:04:05Z", RequestID: "r", Protocol: "anthropic",
		Method: "POST", Path: "/v1/messages", CalledModel: "glm-5.2",
		Provider: "aqp", Status: 200, RequestBody: "q", ResponseBody: "a",
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	writer := &fileWriter{dir: dir, maxSize: int64(len(encoded) + 1)}
	writer.open(start)
	if writer.file == nil {
		t.Fatal("open failed")
	}
	if err := writer.write(record, start); err != nil {
		t.Fatalf("write first record: %v", err)
	}
	second := start.Add(time.Second)
	if err := writer.write(record, second); err != nil {
		t.Fatalf("write second record: %v", err)
	}
	writer.close()

	const archived = "requests-20260713-150405--20260713-150406-1.log"
	const active = "requests-20260713-150406.log"
	if got := requestLogFileNames(t, dir); len(got) != 2 || got[0] != archived || got[1] != active {
		t.Fatalf("files = %v, want [%s %s]", got, archived, active)
	}
	for _, name := range []string{archived, active} {
		if got := countJSONLLines(t, filepath.Join(dir, name)); got != 1 {
			t.Errorf("%s has %d lines, want 1", name, got)
		}
	}
}

func TestFileWriterRotateByDay(t *testing.T) {
	dir := t.TempDir()
	writer := &fileWriter{dir: dir, maxSize: 1 << 30}
	dayOne := time.Date(2026, 7, 13, 23, 59, 0, 0, time.UTC)
	dayTwo := time.Date(2026, 7, 14, 0, 1, 0, 0, time.UTC)
	writer.open(dayOne)
	if err := writer.write(&Record{
		Ts: "2026-07-13T23:59:00Z", RequestID: "r1", RequestBody: "q1",
	}, dayOne); err != nil {
		t.Fatalf("write day one: %v", err)
	}
	if err := writer.write(&Record{
		Ts: "2026-07-14T00:01:00Z", RequestID: "r2", RequestBody: "q2",
	}, dayTwo); err != nil {
		t.Fatalf("write day two: %v", err)
	}
	writer.close()

	const archived = "requests-20260713-235900--20260714-000100-1.log"
	const active = "requests-20260714-000100.log"
	if got := requestLogFileNames(t, dir); len(got) != 2 || got[0] != archived || got[1] != active {
		t.Fatalf("files = %v, want [%s %s]", got, archived, active)
	}
	if got := firstJSONLineStringField(t, filepath.Join(dir, archived), "request_body"); got != "q1" {
		t.Errorf("archived request_body = %q, want q1", got)
	}
	if got := firstJSONLineStringField(t, filepath.Join(dir, active), "request_body"); got != "q2" {
		t.Errorf("active request_body = %q, want q2", got)
	}
}

func TestFileWriterSameDayDoesNotRotate(t *testing.T) {
	dir := t.TempDir()
	writer := &fileWriter{dir: dir, maxSize: 1 << 30}
	now := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	writer.open(now)
	for i := 0; i < 5; i++ {
		if err := writer.write(&Record{Ts: "t", RequestID: "r"}, now.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	writer.close()

	names := requestLogFileNames(t, dir)
	if len(names) != 1 {
		t.Fatalf("files = %v, want one active file", names)
	}
	if got := countJSONLLines(t, filepath.Join(dir, names[0])); got != 5 {
		t.Errorf("active file has %d lines, want 5", got)
	}
}

func TestFileWriterDropsEmptyFileOnDayChange(t *testing.T) {
	dir := t.TempDir()
	writer := &fileWriter{dir: dir, maxSize: 1 << 30}
	dayOne := time.Date(2026, 7, 13, 23, 59, 0, 0, time.UTC)
	dayTwo := time.Date(2026, 7, 14, 0, 1, 0, 0, time.UTC)
	writer.open(dayOne)
	if err := writer.write(&Record{Ts: "t", RequestID: "r"}, dayTwo); err != nil {
		t.Fatalf("write: %v", err)
	}
	writer.close()

	names := requestLogFileNames(t, dir)
	if len(names) != 1 {
		t.Fatalf("files = %v, want one day-two active file", names)
	}
	if strings.Contains(names[0], "--") {
		t.Errorf("empty day-one file was archived as %s", names[0])
	}
	if got := countJSONLLines(t, filepath.Join(dir, names[0])); got != 1 {
		t.Errorf("day-two file has %d lines, want 1", got)
	}
}

func TestFileWriterSameSecondRotationsHaveUniqueNames(t *testing.T) {
	dir := t.TempDir()
	writer := &fileWriter{dir: dir, maxSize: 1}
	now := time.Date(2026, 7, 13, 15, 4, 5, 0, time.UTC)
	writer.open(now)
	record := &Record{Ts: "t", RequestID: "r"}
	for i := 0; i < 3; i++ {
		if err := writer.write(record, now); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	writer.close()

	names := requestLogFileNames(t, dir)
	var archives []string
	for _, name := range names {
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
		if got := countJSONLLines(t, filepath.Join(dir, name)); got != 1 {
			t.Errorf("%s has %d lines, want 1", name, got)
		}
	}
}

func TestFileWriterNarrowsNewAndExistingFilesToOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits are not enforced on Windows")
	}
	t.Run("new file", func(t *testing.T) {
		dir := t.TempDir()
		now := time.Date(2026, 7, 13, 15, 4, 5, 0, time.UTC)
		writer := &fileWriter{dir: dir, maxSize: 1 << 30}
		writer.open(now)
		if writer.file == nil {
			t.Fatal("open failed")
		}
		if err := writer.write(&Record{Ts: "t", RequestID: "r"}, now); err != nil {
			t.Fatal(err)
		}
		path := writer.path
		writer.close()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("new log mode = %o, want 0600", got)
		}
	})

	t.Run("existing file", func(t *testing.T) {
		dir := t.TempDir()
		now := time.Date(2026, 7, 13, 15, 4, 5, 0, time.UTC)
		path := filepath.Join(dir, "requests-20260713-150405.log")
		writeRecordFile(t, dir, filepath.Base(path), []Record{{Ts: "old", RequestID: "old"}})
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}

		writer := &fileWriter{dir: dir, maxSize: 1 << 30}
		writer.open(now)
		if writer.file == nil {
			t.Fatal("open existing file failed")
		}
		if err := writer.write(&Record{Ts: "new", RequestID: "new"}, now); err != nil {
			t.Fatal(err)
		}
		writer.close()

		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("existing log mode = %o, want 0600", got)
		}
		if got := countJSONLLines(t, path); got != 2 {
			t.Errorf("existing log has %d lines, want append-preserved 2", got)
		}
	})
}
