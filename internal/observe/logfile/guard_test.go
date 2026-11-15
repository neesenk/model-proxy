package logfile

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"testing"
)

func writeGuardFile(t *testing.T, dir, name string, size int) {
	t.Helper()
	path := filepath.Join(dir, name)
	body := make([]byte, size)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func newGuardLogger(dir string, minFree int64, maxUsed int) *Logger {
	l := New(Options{Directory: dir, FilePrefix: "requests-", MinFreeBytes: minFree, MaxUsedPercent: maxUsed})
	l.freeSpace = func(string) (int64, int64, error) { return 100, 1000, nil }
	return l
}

// TestGuardDeletesOldestUntilFloorMet: with 100 free against a 250 floor and
// 100-byte rotated files, the guard deletes oldest-first until the
// accumulated free space clears the floor. The active file is never deleted.
func TestGuardDeletesOldestUntilFloorMet(t *testing.T) {
	dir := t.TempDir()
	writeGuardFile(t, dir, "requests-20261101--010000-1.log", 100)
	writeGuardFile(t, dir, "requests-20261102--010000-1.log", 100)
	writeGuardFile(t, dir, "requests-20261103--010000-1.log", 100)
	writeGuardFile(t, dir, "requests-20261115.log", 100) // active
	l := newGuardLogger(dir, 250, 0)

	l.guard("requests-20261115.log")

	for _, gone := range []string{"requests-20261101--010000-1.log", "requests-20261102--010000-1.log"} {
		if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
			t.Errorf("%s still present, want deleted oldest-first", gone)
		}
	}
	for _, kept := range []string{"requests-20261103--010000-1.log", "requests-20261115.log"} {
		if _, err := os.Stat(filepath.Join(dir, kept)); err != nil {
			t.Errorf("%s missing: %v", kept, err)
		}
	}
}

// TestGuardPercentCondition: the usage ceiling triggers on used% >= threshold
// even when the free floor is not armed, and stays quiet below it.
func TestGuardPercentCondition(t *testing.T) {
	dir := t.TempDir()
	writeGuardFile(t, dir, "requests-20261101--010000-1.log", 100)
	writeGuardFile(t, dir, "requests-20261115.log", 100)
	l := newGuardLogger(dir, 0, 95)

	// 960/1000 used = 96% >= 95% → delete the one rotated file.
	l.freeSpace = func(string) (int64, int64, error) { return 40, 1000, nil }
	l.guard("requests-20261115.log")
	if _, err := os.Stat(filepath.Join(dir, "requests-20261101--010000-1.log")); !os.IsNotExist(err) {
		t.Fatalf("rotated file still present, want deleted at 96%% usage")
	}
	if _, err := os.Stat(filepath.Join(dir, "requests-20261115.log")); err != nil {
		t.Fatalf("active file deleted: %v", err)
	}

	// 500/1000 used = 50% < 95% → nothing deleted.
	writeGuardFile(t, dir, "requests-20261102--010000-1.log", 100)
	l.freeSpace = func(string) (int64, int64, error) { return 500, 1000, nil }
	l.guard("requests-20261115.log")
	if _, err := os.Stat(filepath.Join(dir, "requests-20261102--010000-1.log")); err != nil {
		t.Fatalf("guard deleted below the usage ceiling: %v", err)
	}
}

// TestGuardDisabledWhenBothConditionsOff: zero conditions are a no-op even
// with a nearly-full volume.
func TestGuardDisabledWhenBothConditionsOff(t *testing.T) {
	dir := t.TempDir()
	writeGuardFile(t, dir, "requests-20261101--010000-1.log", 100)
	l := newGuardLogger(dir, 0, 0)
	l.freeSpace = func(string) (int64, int64, error) { return 1, 1000, nil }

	l.guard("requests-20261115.log")

	if _, err := os.Stat(filepath.Join(dir, "requests-20261101--010000-1.log")); err != nil {
		t.Fatalf("guard ran despite both conditions off: %v", err)
	}
}

// TestGuardStatsErrorWarnsWithoutDeleting: a failing volume query must warn
// and touch nothing.
func TestGuardStatsErrorWarnsWithoutDeleting(t *testing.T) {
	dir := t.TempDir()
	writeGuardFile(t, dir, "requests-20261101--010000-1.log", 100)
	l := newGuardLogger(dir, 250, 0)
	l.freeSpace = func(string) (int64, int64, error) { return 0, 0, os.ErrPermission }

	var logs bytes.Buffer
	original := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(original)
	l.guard("requests-20261115.log")
	log.SetOutput(original)

	if _, err := os.Stat(filepath.Join(dir, "requests-20261101--010000-1.log")); err != nil {
		t.Fatalf("guard deleted files despite a stats error: %v", err)
	}
	if !bytes.Contains(logs.Bytes(), []byte("disk guard: volume stats")) {
		t.Errorf("missing stats-error warning:\n%s", logs.String())
	}
}

// TestGuardNothingDeletableWarns: a triggered guard with no rotated files
// left must say so and never remove the active file.
func TestGuardNothingDeletableWarns(t *testing.T) {
	dir := t.TempDir()
	writeGuardFile(t, dir, "requests-20261115.log", 100)
	l := newGuardLogger(dir, 250, 0)

	var logs bytes.Buffer
	original := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(original)
	l.guard("requests-20261115.log")
	log.SetOutput(original)

	if _, err := os.Stat(filepath.Join(dir, "requests-20261115.log")); err != nil {
		t.Fatalf("active file deleted: %v", err)
	}
	if !bytes.Contains(logs.Bytes(), []byte("no rotated file is deletable")) {
		t.Errorf("missing exhausted warning:\n%s", logs.String())
	}
}
