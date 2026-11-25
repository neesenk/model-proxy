package logfile

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// triggeredGuard returns a logger whose disk guard is armed and triggered:
// the floor is 250 bytes, the volume holds 100 free of 1000.
func triggeredGuard(dir string) *Logger {
	l := New(Options{Directory: dir, FilePrefix: "requests-", Tag: "t", MinFreeBytes: 250})
	l.freeSpace = func(string) (int64, int64, error) { return 100, 1000, nil }
	return l
}

// TestGuardAccountsForInFlightArchivesItCannotDelete: an archive still being
// written occupies space like any other file of the stream but is in no
// deletable set. The guard's report has to say so: a volume still under the
// floor after a pass otherwise reads as "nothing left to take" while the
// stream holds bytes the guard just walked past.
func TestGuardAccountsForInFlightArchivesItCannotDelete(t *testing.T) {
	const note = "; 1 unfinished archive(s) hold 0.0GiB in .zst.tmp (never deleted)\n"

	t.Run("alongside the deletion report", func(t *testing.T) {
		dir := t.TempDir()
		writeGuardFile(t, dir, "requests-20261101.log", 100)         // deletable
		writeGuardFile(t, dir, "requests-20261102.log", 100)         // deletable; its archive is being written
		writeGuardFile(t, dir, "requests-20261102.log.zst.tmp", 300) // in flight: never deletable
		writeGuardFile(t, dir, "requests-20261115.log", 100)         // active
		logs := captureLogs(t)

		triggeredGuard(dir).guard("requests-20261115.log")

		// free=100 < 250: 20261101 lifts it to 200 (still short), 20261102 to 300.
		for _, gone := range []string{"requests-20261101.log", "requests-20261102.log"} {
			if _, err := os.Stat(filepath.Join(dir, gone)); !os.IsNotExist(err) {
				t.Errorf("%s still present, want deleted oldest-first", gone)
			}
		}
		for _, kept := range []string{"requests-20261102.log.zst.tmp", "requests-20261115.log"} {
			if _, err := os.Stat(filepath.Join(dir, kept)); err != nil {
				t.Errorf("%s missing: %v", kept, err)
			}
		}
		want := "[t] disk guard: deleted 2 rotated file(s) (0.0GiB), volume free now ~0.0GiB" + note
		if got := logs.String(); got != want {
			t.Errorf("guard log = %q, want the deletion report followed by the in-flight accounting:\n%s", got, want)
		}
	})

	t.Run("when nothing is deletable", func(t *testing.T) {
		dir := t.TempDir()
		writeGuardFile(t, dir, "requests-20261115.log", 100)         // active
		writeGuardFile(t, dir, "requests-20261120.log.zst.tmp", 300) // a dead run's leftover: still never deleted
		logs := captureLogs(t)
		pinLogLevel(t, "warn") // an exhausted guard warns: the line must get through at warn

		triggeredGuard(dir).guard("requests-20261115.log")

		if _, err := os.Stat(filepath.Join(dir, "requests-20261120.log.zst.tmp")); err != nil {
			t.Fatalf("in-flight tmp deleted by the guard: %v", err)
		}
		want := "[t] disk guard: triggered but no rotated file is deletable (the active file and index are never removed)" + note
		if got := logs.String(); got != want {
			t.Errorf("guard log = %q, want the exhausted warning followed by the in-flight accounting:\n%s", got, want)
		}
	})

	t.Run("the deletion report is an info line", func(t *testing.T) {
		dir := t.TempDir()
		writeGuardFile(t, dir, "requests-20261101.log", 100) // deletable
		writeGuardFile(t, dir, "requests-20261115.log", 100) // active
		logs := captureLogs(t)
		pinLogLevel(t, "warn")

		triggeredGuard(dir).guard("requests-20261115.log")

		if _, err := os.Stat(filepath.Join(dir, "requests-20261101.log")); !os.IsNotExist(err) {
			t.Fatalf("rotated file still present (%v): the pass did not run, so a silent log would prove nothing", err)
		}
		if got := logs.String(); got != "" {
			t.Fatalf("guard log at warn = %q, want nothing: the deletion report is info", got)
		}
	})

	t.Run("not at all when no archive is in flight", func(t *testing.T) {
		dir := t.TempDir()
		writeGuardFile(t, dir, "requests-20261115.log", 100) // active
		logs := captureLogs(t)

		triggeredGuard(dir).guard("requests-20261115.log")

		want := "[t] disk guard: triggered but no rotated file is deletable (the active file and index are never removed)\n"
		if got := logs.String(); got != want {
			t.Errorf("guard log = %q, want the unchanged line with nothing appended:\n%s", got, want)
		}
	})
}

// TestNoDeletionPathSelectsAnInFlightTmp: the retention sweep, the one-shot
// Prune and the disk guard all leave an archive still being written alone,
// however old it looks — its source is mid-archive, and nothing can be
// concluded about it until the archive is done (the dispatch pass reaps a
// tmp whose source is gone). Every path runs over the same directory, with a
// live tmp (source present) and an orphan one (source absent).
func TestNoDeletionPathSelectsAnInFlightTmp(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	ancient := now.Add(-100 * 24 * time.Hour)
	live := filepath.Join(dir, "requests-20200101.log.zst.tmp")
	orphan := filepath.Join(dir, "requests-20200102.log.zst.tmp")
	source := filepath.Join(dir, "requests-20200101.log")
	for _, p := range []string{live, orphan, source} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, ancient, ancient); err != nil {
			t.Fatal(err)
		}
	}

	// Retention: the expired plain source goes, the tmps do not.
	retention := New(Options{Directory: dir, FilePrefix: "requests-", Retention: 30 * 24 * time.Hour, Tag: "t"})
	retention.sweep(now, "")
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("expired plain source kept by the sweep: %v", err)
	}
	if targets, err := Prunable(dir, "requests-", now.Add(-30*24*time.Hour), now); err != nil || len(targets) != 0 {
		t.Fatalf("Prunable = %+v, %v; want no target: neither tmp is a cleanup candidate", targets, err)
	}
	if removed, err := Prune(dir, "requests-", now.Add(-30*24*time.Hour), now); err != nil || len(removed) != 0 {
		t.Fatalf("Prune removed %+v, %v; want nothing", removed, err)
	}
	captureLogs(t)
	triggeredGuard(dir).guard(filepath.Join(dir, "requests-20261122.log"))

	for _, tmp := range []string{live, orphan} {
		if _, err := os.Stat(tmp); err != nil {
			t.Errorf("%s did not survive sweep, Prune and the guard: %v", filepath.Base(tmp), err)
		}
	}
}

// TestInFlightArchiveNoteSizes pins the accounting itself — count and byte
// total — which the guard-level cases above can only see as "0.0GiB": the
// guard's scale is the volume's, not a test fixture's.
func TestInFlightArchiveNoteSizes(t *testing.T) {
	if got := inFlightArchiveNote(nil); got != "" {
		t.Errorf("note with nothing in flight = %q, want empty", got)
	}
	inFlight := []inFlightArchive{
		{Name: "requests-20261101.log.zst.tmp", Size: 1 << 30},
		{Name: "requests-20261102.log.zst.tmp", Size: 1 << 29},
	}
	want := "; 2 unfinished archive(s) hold 1.5GiB in .zst.tmp (never deleted)"
	if got := inFlightArchiveNote(inFlight); got != want {
		t.Errorf("note = %q, want %q", got, want)
	}
}

// TestRotatedFilesSizesMatchTheFilesOnDisk: both lists carry the size of the
// file they name. The guard's accounting is only as good as these numbers,
// and no guard-level case can see them — a report in GiB renders every test
// fixture as 0.0GiB, so sizes recorded as 0 would pass all of those.
func TestRotatedFilesSizesMatchTheFilesOnDisk(t *testing.T) {
	dir := t.TempDir()
	writeGuardFile(t, dir, "requests-20261101.log.zst", 200)     // deletable
	writeGuardFile(t, dir, "requests-20261102.log", 100)         // deletable
	writeGuardFile(t, dir, "requests-20261102.log.zst.tmp", 300) // in flight
	writeGuardFile(t, dir, "requests-20261115.log", 50)          // active: in neither list
	if err := os.Mkdir(filepath.Join(dir, "requests-20261103.log.zst.tmp"), 0o700); err != nil {
		t.Fatal(err) // a directory wearing an archive's name is not one
	}

	deletable, inFlight, err := rotatedFiles(dir, "requests-", "requests-20261115.log")
	if err != nil {
		t.Fatal(err)
	}
	want := []PruneTarget{
		{Name: "requests-20261101.log.zst", Size: 200},
		{Name: "requests-20261102.log", Size: 100},
	}
	if !slices.Equal(deletable, want) {
		t.Errorf("deletable = %+v, want %+v (oldest first, active file and tmp left out)", deletable, want)
	}
	if len(inFlight) != 1 || inFlight[0].Name != "requests-20261102.log.zst.tmp" || inFlight[0].Size != 300 {
		t.Errorf("in flight = %+v, want exactly the one tmp with its 300 bytes on disk", inFlight)
	}
}
