package serve

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeServeLog(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readServeLog(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

var rotationStamp = time.Date(2026, 11, 16, 1, 30, 0, 0, time.Local)

// TestPerformRotationArchivesAndTruncates: rotation snapshots the log into a
// stem-stamped archive and truncates the original to zero.
func TestPerformRotationArchivesAndTruncates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "serve.log")
	writeServeLog(t, path, strings.Repeat("line\n", 100))

	archive, err := performRotation(path, 4, rotationStamp)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "serve-20261116-013000.log"); archive != want {
		t.Fatalf("archive = %s, want %s", archive, want)
	}
	if body := readServeLog(t, archive); body != strings.Repeat("line\n", 100) {
		t.Fatalf("archive content mismatch: %d bytes (want %d)", len(body), 5*100)
	}
	if size := len(readServeLog(t, path)); size != 0 {
		t.Fatalf("active log not truncated: %d bytes remain", size)
	}
}

// TestPerformRotationCollisionAppendsSequence: an existing archive with the
// same stamp gets the next -N suffix instead of overwriting.
func TestPerformRotationCollisionAppendsSequence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "serve.log")
	writeServeLog(t, path, "second rotation\n")
	writeServeLog(t, filepath.Join(dir, "serve-20261116-013000.log"), "first rotation\n")

	archive, err := performRotation(path, 4, rotationStamp)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "serve-20261116-013000-2.log"); archive != want {
		t.Fatalf("archive = %s, want %s", archive, want)
	}
	if body := readServeLog(t, filepath.Join(dir, "serve-20261116-013000.log")); body != "first rotation\n" {
		t.Fatalf("existing archive was modified: %q", body)
	}
}

// TestPruneArchivesKeepsNewest: pruning keeps the newest `keep` archives and
// deletes older ones by name order.
func TestPruneArchivesKeepsNewest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "serve.log")
	writeServeLog(t, path, "active\n")
	for _, name := range []string{
		"serve-20261110-010000.log",
		"serve-20261111-010000.log",
		"serve-20261112-010000.log",
		"serve-20261113-010000.log",
		"serve-20261114-010000.log",
		"serve-20261115-010000.log",
	} {
		writeServeLog(t, filepath.Join(dir, name), "x")
	}

	removed, err := pruneArchives(path, 4)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 || removed[0] != "serve-20261110-010000.log" || removed[1] != "serve-20261111-010000.log" {
		t.Fatalf("removed = %v, want the two oldest by name", removed)
	}
	if _, err := os.Stat(filepath.Join(dir, "serve-20261110-010000.log")); !os.IsNotExist(err) {
		t.Errorf("oldest archive still present")
	}
	if _, err := os.Stat(filepath.Join(dir, "serve-20261115-010000.log")); err != nil {
		t.Errorf("newest archive deleted: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("active log deleted: %v", err)
	}
}

// TestRunLogRotatorLoopStopsOnClosedChannel: a closed stop channel ends the
// loop promptly — the done channel fires within the test deadline without
// waiting for the first tick.
func TestRunLogRotatorLoopStopsOnClosedChannel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "serve.log")
	writeServeLog(t, path, "x")

	stop := make(chan struct{})
	close(stop)
	done := make(chan struct{})
	go func() {
		RunLogRotatorLoop(stop, path, 1, 4, time.Minute)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("rotator loop did not return after stop closed")
	}
}

// TestRunLogRotatorLoopRotatesOversizedLog: an oversized log is rotated on a
// tick (interval passed in as milliseconds); the active file ends up
// truncated and an archive exists.
func TestRunLogRotatorLoopRotatesOversizedLog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "serve.log")
	writeServeLog(t, path, strings.Repeat("x", 64))

	stop := make(chan struct{})
	defer close(stop)
	done := make(chan struct{})
	go func() {
		RunLogRotatorLoop(stop, path, 1, 4, time.Millisecond) // maxBytes 1: anything rotates
		close(done)
	}()

	deadline := time.After(10 * time.Second)
	for done := false; !done; {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		sawArchive := false
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "serve-") && strings.HasSuffix(e.Name(), ".log") {
				sawArchive = true
				break
			}
		}
		// The archive appears mid-copy, before the truncate lands — wait for
		// BOTH conditions, not archive presence alone.
		if sawArchive {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			done = info.Size() == 0
		}
		if !done {
			select {
			case <-deadline:
				t.Fatal("oversized log was not rotated (archive + truncated active) on a tick")
			case <-time.After(500 * time.Microsecond):
			}
		}
	}
}
