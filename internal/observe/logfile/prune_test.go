package logfile

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writePruneFile creates dir/name with size bytes of content and the given
// mtime (content is a repeated byte so Size is deterministic).
func writePruneFile(t *testing.T, dir, name string, size int, mtime time.Time) {
	t.Helper()
	data := make([]byte, size)
	for i := range data {
		data[i] = 'x'
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

// TestPrunableSelectsOldRotatedFilesOnly pins the one-shot cleanup candidate
// set: mtime-older-than-cutoff rotated files only — the active (today's)
// file, non-log entries (index.db), foreign prefixes and directories are
// never listed, even when old.
func TestPrunableSelectsOldRotatedFilesOnly(t *testing.T) {
	dir := testDir(t)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	cutoff := now.AddDate(0, 0, -7)
	old := cutoff.Add(-time.Hour)
	fresh := cutoff.Add(time.Hour)

	writePruneFile(t, dir, "test-20260701--010000-1.log", 100, old)  // eligible
	writePruneFile(t, dir, "test-20260721.log", 50, old)             // old but rotated-day name — eligible
	writePruneFile(t, dir, "test-20260728.log", 10, old)             // ACTIVE file, old mtime — never
	writePruneFile(t, dir, "test-20260727--000000-2.log", 20, fresh) // fresh — no
	writePruneFile(t, dir, "index.db", 999, old)                     // not a log file — no
	writePruneFile(t, dir, "other-20260701.log", 30, old)            // foreign prefix — no
	if err := os.Mkdir(filepath.Join(dir, "test-sub-20260701.log"), 0o700); err != nil {
		t.Fatal(err)
	}

	targets, err := Prunable(dir, testPrefix, cutoff, now)
	if err != nil {
		t.Fatalf("Prunable: %v", err)
	}
	var names []string
	var bytes int64
	for _, tgt := range targets {
		names = append(names, tgt.Name)
		bytes += tgt.Size
	}
	if len(names) != 2 || names[0] != "test-20260701--010000-1.log" || names[1] != "test-20260721.log" {
		t.Errorf("Prunable names = %v, want the two old rotated files", names)
	}
	if bytes != 150 {
		t.Errorf("Prunable total size = %d, want 150", bytes)
	}
}

// TestPruneRemovesExactlyTheCandidates pins that Prune deletes what
// Prunable lists and nothing else.
func TestPruneRemovesExactlyTheCandidates(t *testing.T) {
	dir := testDir(t)
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	cutoff := now.AddDate(0, 0, -7)
	old := cutoff.Add(-time.Hour)

	writePruneFile(t, dir, "test-20260601.log", 40, old)
	writePruneFile(t, dir, "test-20260728.log", 10, now)
	writePruneFile(t, dir, "index.db", 999, old)

	removed, err := Prune(dir, testPrefix, cutoff, now)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(removed) != 1 || removed[0].Name != "test-20260601.log" {
		t.Fatalf("removed = %+v, want [test-20260601.log]", removed)
	}
	for _, name := range []string{"test-20260728.log", "index.db"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("Prune removed %s which it must keep: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "test-20260601.log")); !os.IsNotExist(err) {
		t.Errorf("old file still exists after Prune (stat err %v)", err)
	}
}

// TestPruneMissingDir verifies the error shape for a missing directory.
func TestPruneMissingDir(t *testing.T) {
	now := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	if _, err := Prune(filepath.Join(t.TempDir(), "missing"), testPrefix, now.Add(-time.Hour), now); err == nil {
		t.Fatal("Prune on a missing directory returned nil error")
	}
}
