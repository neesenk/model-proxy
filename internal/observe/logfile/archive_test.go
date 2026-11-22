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
