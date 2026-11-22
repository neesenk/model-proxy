package logfile

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
)

func TestArchiveZstdRoundTripAtomic(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "requests-20260101.log")
	body := strings.Repeat("{\"ts\":\"2026-01-01T00:00:00Z\"}\n", 1000)
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := archiveZstd(src); err != nil {
		t.Fatalf("archiveZstd: %v", err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("plain file still present: %v", err)
	}
	if _, err := os.Stat(src + ".zst.tmp"); !os.IsNotExist(err) {
		t.Fatal("tmp leftover after success")
	}
	f, err := os.Open(src + ".zst")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("archive mode = %v, %v; want 0600", info, err)
	}
	dec, err := zstd.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	got, err := io.ReadAll(dec)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("round trip mismatch: %d bytes vs %d", len(got), len(body))
	}
}

func TestArchiveZstdIdempotentAfterCrashBetweenRenameAndRemove(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "requests-20260101.log")
	if err := os.WriteFile(src, []byte("plain\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src+".zst", []byte("already compressed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := archiveZstd(src); err != nil {
		t.Fatalf("archiveZstd: %v", err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("plain file kept after idempotent rerun")
	}
	b, err := os.ReadFile(src + ".zst")
	if err != nil || string(b) != "already compressed" {
		t.Fatalf("existing archive must be untouched: %q, %v", b, err)
	}
}

func TestDispatchArchiveOffersOnlyColdRotatedFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	write := func(name string, age time.Duration) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
		return p
	}
	active := write("requests-20261122.log", 0)
	cold := write("requests-20261115.log", 8*24*time.Hour)
	write("requests-20261121.log", 24*time.Hour) // inside hot window
	if err := os.WriteFile(filepath.Join(dir, "requests-20261114.log.zst"), []byte("z"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.db"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	l := New(Options{Directory: dir, FilePrefix: "requests-", HotRetention: 7 * 24 * time.Hour})
	l.dispatchArchive(now, active)
	select {
	case got := <-l.archive:
		if got != cold {
			t.Fatalf("dispatched %s, want %s", got, cold)
		}
	default:
		t.Fatal("expected the cold rotated file to be dispatched")
	}
	select {
	case extra := <-l.archive:
		t.Fatalf("dispatched extra %s", extra)
	default:
	}
}

func TestDispatchArchiveDisabledWhenHotRetentionZero(t *testing.T) {
	l := New(Options{Directory: t.TempDir(), HotRetention: 0})
	l.dispatchArchive(time.Now(), "")
	select {
	case p := <-l.archive:
		t.Fatalf("dispatched %s with archiving disabled", p)
	default:
	}
}

func TestArchiveWorkerFiresOnChangePerSuccessAndSurvivesErrors(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.log")
	b := filepath.Join(dir, "b.log")
	if err := os.WriteFile(a, []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := New(Options{Directory: dir, Tag: "t"})
	var calls atomic.Int32
	l.archiveFn = func(p string) error {
		if calls.Add(1) == 1 {
			return errors.New("boom")
		}
		return os.Remove(p)
	}
	changes := make(chan struct{}, 2)
	l.opts.OnChange = func() { changes <- struct{}{} }
	go func() {
		l.archive <- a
		l.archive <- b
		close(l.archive)
	}()
	l.archiveWorker() // returns when the queue closes: worker drained both
	if _, err := os.Stat(a); err != nil {
		t.Fatalf("failed archive must keep the plain file: %v", err)
	}
	if _, err := os.Stat(b); !os.IsNotExist(err) {
		t.Fatalf("successful archive must remove the plain file: %v", err)
	}
	select {
	case <-changes:
	default:
		t.Fatal("OnChange not fired for the successful archive")
	}
}

func TestRunShutdownWaitsForInFlightArchive(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "requests-20260101.log")
	if err := os.WriteFile(src, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := New(Options{Directory: dir, FilePrefix: "requests-", Retention: time.Hour, HotRetention: time.Hour})
	started := make(chan struct{})
	release := make(chan struct{})
	l.archiveFn = func(p string) error {
		close(started)
		<-release
		return os.Rename(p, p+".done")
	}
	go l.Run()
	l.archive <- src
	<-started // the worker is mid-archive
	shutdownReturned := make(chan struct{})
	go func() {
		l.Shutdown()
		close(shutdownReturned)
	}()
	close(release) // the in-flight archive may now finish
	<-shutdownReturned
	// Shutdown must have waited for the final flush: the rename is visible
	// by the time it returns.
	if _, err := os.Stat(src + ".done"); err != nil {
		t.Fatalf("Shutdown returned before the in-flight archive finished: %v", err)
	}
}

// TestIsZstdFileMatchesOnlyThisStreamsArchives pins the archive matcher the
// retention sweep, the disk guard and Prune delete by: this stream's prefix
// plus the exact .zst suffix. An archive still being written is not one yet,
// and another stream's archive is never this stream's.
func TestIsZstdFileMatchesOnlyThisStreamsArchives(t *testing.T) {
	const prefix = "requests-"
	cases := []struct {
		name  string
		match bool
	}{
		{"requests-20261101.log.zst", true},           // per-day file, archived
		{"requests-20261101--010000-1.log.zst", true}, // size-rotated file, archived
		{"requests-20261101.log.zst.tmp", false},      // still being written
		{"requests-20261101.log", false},              // plain file
		{"security-20261101.log.zst", false},          // another stream's archive
		{"requests.log.zst", false},                   // prefix is requests-, not requests
	}
	for _, tc := range cases {
		if got := isZstdFile(tc.name, prefix); got != tc.match {
			t.Errorf("isZstdFile(%q) = %v, want %v", tc.name, got, tc.match)
		}
	}
}

// TestArchiveDayReadsTheNameStamp pins where an archive's age comes from:
// the YYYYMMDD day stamp its name carries right after the prefix (a .zst's
// mtime is compression time). Anything else — too short, not a date, an
// impossible date — reports no day rather than a guess.
func TestArchiveDayReadsTheNameStamp(t *testing.T) {
	const prefix = "requests-"
	november1 := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		want time.Time
		ok   bool
	}{
		{"requests-20261101.log.zst", november1, true},
		{"requests-20261101--010000-1.log.zst", november1, true},
		{"req", time.Time{}, false},                       // shorter than the prefix
		{"requests-.zst", time.Time{}, false},             // 4 chars after the prefix
		{"requests-202.zst", time.Time{}, false},          // 7 chars after the prefix
		{"requests-archive.log.zst", time.Time{}, false},  // not a date stamp
		{"requests-20261301.log.zst", time.Time{}, false}, // month out of range
		{"requests-20261132.log.zst", time.Time{}, false}, // day out of range
	}
	for _, tc := range cases {
		got, ok := archiveDay(tc.name, prefix)
		if ok != tc.ok || !got.Equal(tc.want) {
			t.Errorf("archiveDay(%q) = %v, %v; want %v, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}

// TestArchiveZstdReleasesSourceWhenTmpIsBlocked: a failure after the source
// was opened (its tmp path is taken by a directory) must report the error,
// leave the plain file and no archive behind, and release the source handle.
// The handle matters on Windows, where an open file cannot be removed: one
// leaked by a failed attempt would still be open when a later attempt in the
// same process removes the source.
func TestArchiveZstdReleasesSourceWhenTmpIsBlocked(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "requests-20260101.log")
	if err := os.WriteFile(src, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tmp := src + ".zst.tmp"
	if err := os.Mkdir(tmp, 0o700); err != nil { // OpenFile on a directory fails
		t.Fatal(err)
	}
	before := openDescriptorCount(t)

	err := archiveZstd(src)

	var pathErr *os.PathError
	if !errors.As(err, &pathErr) || pathErr.Path != tmp {
		t.Fatalf("archiveZstd error = %v, want one naming the blocked tmp %s", err, tmp)
	}
	if got := openDescriptorCount(t); got != before {
		t.Fatalf("open descriptors = %d after the failed archive, want %d: the source handle leaked", got, before)
	}
	data, err := os.ReadFile(src)
	if err != nil || string(data) != "{}\n" {
		t.Fatalf("plain file = %q, %v; want kept untouched", data, err)
	}
	if _, err := os.Stat(src + ".zst"); !os.IsNotExist(err) {
		t.Fatalf("archive created despite the failure: %v", err)
	}
}

// openDescriptorCount reports how many file descriptors this process holds
// open, so a test can prove a handle was released. It reads the process's
// descriptor directory (Unix), which lists the handle used to read it, so
// equal conditions give equal counts.
func openDescriptorCount(t *testing.T) int {
	t.Helper()
	for _, dir := range []string{"/proc/self/fd", "/dev/fd"} {
		entries, err := os.ReadDir(dir)
		if err == nil {
			return len(entries)
		}
	}
	t.Skip("no descriptor directory (/proc/self/fd, /dev/fd): a released handle is not observable here")
	return 0
}

// TestLoggerHotRetentionAccessor: the hot window is reported as configured;
// 0 means archiving is disabled, which is the default.
func TestLoggerHotRetentionAccessor(t *testing.T) {
	if got := New(Options{Directory: t.TempDir()}).HotRetention(); got != 0 {
		t.Fatalf("default HotRetention() = %v, want 0 (archiving disabled)", got)
	}
	if got := New(Options{Directory: t.TempDir(), HotRetention: 48 * time.Hour}).HotRetention(); got != 48*time.Hour {
		t.Fatalf("HotRetention() = %v, want 48h", got)
	}
	var stopped *Logger
	if got := stopped.HotRetention(); got != 0 {
		t.Fatalf("nil HotRetention() = %v, want 0", got)
	}
}

// TestShutdownAbandonsQueuedButFinishesInFlight: stop is the final flush and
// nothing more. Shutdown must wait for the archive in flight and must not
// start the offers still queued — those files stay plain on disk, for the
// next run's sweep to re-offer.
//
// Ordered by channels only: the gate holds the first archive until Run's
// exit path has closed the stop channel, so the worker cannot reach a queued
// offer before stop is visible. The scenario is repeated because the worker's
// re-check of stop is reached only when select hands it a queued offer
// although stop is already closed — a fair coin per lifecycle. Run once, a
// deleted re-check would fail this test only every other run.
func TestShutdownAbandonsQueuedButFinishesInFlight(t *testing.T) {
	const rounds = 32
	for round := 0; round < rounds; round++ {
		dir := t.TempDir()
		var paths []string
		for _, name := range []string{"requests-20260101.log", "requests-20260102.log", "requests-20260103.log"} {
			p := filepath.Join(dir, name)
			if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			paths = append(paths, p)
		}
		first, queued := paths[0], paths[1:]
		l := New(Options{Directory: dir, FilePrefix: "requests-", HotRetention: time.Hour})
		started := make(chan struct{})
		release := make(chan struct{})
		var calls atomic.Int32
		l.archiveFn = func(p string) error {
			if calls.Add(1) == 1 {
				close(started)
				<-release
			}
			return os.Rename(p, p+".done")
		}
		for _, p := range paths {
			l.archive <- p
		}
		go l.Run()
		<-started // the worker is inside the first archive
		shutdownReturned := make(chan struct{})
		go func() {
			l.Shutdown()
			close(shutdownReturned)
		}()
		<-l.archiveStop // Run's exit path has asked the worker to stop
		close(release)  // the in-flight archive may now finish
		<-shutdownReturned
		if got := calls.Load(); got != 1 {
			t.Fatalf("round %d: archived %d file(s) at shutdown, want only the one in flight", round, got)
		}
		if _, err := os.Stat(first + ".done"); err != nil {
			t.Fatalf("round %d: in-flight archive not finished before Shutdown returned: %v", round, err)
		}
		for _, p := range queued {
			if _, err := os.Stat(p); err != nil {
				t.Fatalf("round %d: queued offer %s was started at shutdown, want it left plain: %v", round, filepath.Base(p), err)
			}
		}
	}
}

// TestSweepDeletesExpiredZstdByNameDateNotMtime pins the retention rule for
// archives: an expired .zst is deleted on the day stamp its name carries, and
// a .zst inside the window survives however old its mtime looks — compression
// time says nothing about content age.
func TestSweepDeletesExpiredZstdByNameDateNotMtime(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	// Name-day old, mtime fresh (compressed recently): must be deleted.
	expired := filepath.Join(dir, "requests-20261020.log.zst")
	if err := os.WriteFile(expired, []byte("z"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(expired, now, now); err != nil {
		t.Fatal(err)
	}
	// Name-day young, mtime ancient: must be kept.
	fresh := filepath.Join(dir, "requests-20261121.log.zst")
	if err := os.WriteFile(fresh, []byte("z"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(fresh, now.Add(-100*24*time.Hour), now.Add(-100*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	l := New(Options{Directory: dir, FilePrefix: "requests-", Retention: 30 * 24 * time.Hour})
	l.sweep(now, "")
	if _, err := os.Stat(expired); !os.IsNotExist(err) {
		t.Fatalf("expired .zst kept: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh .zst deleted: %v", err)
	}
}

// TestGuardDeletesZstdOldestFirst pins that archives belong to the disk
// guard's deletable set and share its oldest-first-by-name order: the oldest
// entry goes whether it is a .zst or a .log, and the pass stops once the
// floor is met.
func TestGuardDeletesZstdOldestFirst(t *testing.T) {
	dir := t.TempDir()
	young := filepath.Join(dir, "requests-20261121.log")
	oldZ := filepath.Join(dir, "requests-20261101.log.zst")
	for _, p := range []string{young, oldZ} {
		if err := os.WriteFile(p, []byte("data"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	l := New(Options{
		Directory: dir, FilePrefix: "requests-",
		MinFreeBytes: 4, MaxUsedPercent: 0,
	})
	// free=0 < 4 triggers; the guard deletes oldest-first by name, so the
	// 20261101 .zst goes before the 20261121 .log, and the 4 freed bytes
	// project free to 4 — no longer below the floor — stopping the pass.
	l.freeSpace = func(string) (free, total int64, err error) {
		return 0, 0, nil
	}
	l.guard(filepath.Join(dir, "requests-20261122.log"))
	if _, err := os.Stat(oldZ); !os.IsNotExist(err) {
		t.Fatalf("guard did not delete the oldest .zst: %v", err)
	}
	if _, err := os.Stat(young); err != nil {
		t.Fatalf("guard deleted past the trigger: %v", err)
	}
}

// TestPrunableListsZstdByArchiveDay pins that the one-shot cleanup lists an
// expired archive — by its name's day stamp, not by a fresh compression mtime
// — and nothing inside the window.
func TestPrunableListsZstdByArchiveDay(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	z := filepath.Join(dir, "requests-20261020.log.zst")
	if err := os.WriteFile(z, []byte("z"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(z, now, now); err != nil { // fresh mtime, old name-day
		t.Fatal(err)
	}
	logf := filepath.Join(dir, "requests-20261121.log")
	if err := os.WriteFile(logf, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	targets, err := Prunable(dir, "requests-", now.Add(-30*24*time.Hour), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].Name != "requests-20261020.log.zst" {
		t.Fatalf("targets = %+v, want only the old .zst", targets)
	}
}
