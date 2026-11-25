package logfile

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

// compressThenLoseSource puts an archive where a deleter leaves it: the
// compression finished — its tmp is complete — and the plain file is gone,
// removed while the archive was being written. The paths it returns are the
// plain file, the tmp and the archive.
func compressThenLoseSource(t *testing.T, body string) (src, tmp, dst string) {
	t.Helper()
	src = filepath.Join(t.TempDir(), "requests-20260101.log")
	tmp, dst = src+zstdTmpSuffix, src+zstdSuffix
	if err := os.WriteFile(src, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := compressZstd(src, tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(src); err != nil {
		t.Fatal(err)
	}
	return src, tmp, dst
}

// revivedArchiveNote is what the archive tail says when it publishes an
// archive whose plain file a deleter (retention sweep, disk guard) removed
// mid-compression. It is spelled out here rather than shared with the
// production code: the test asserts the operator-facing text itself.
const revivedArchiveNote = ": the plain file was removed while it was being compressed (retention sweep or disk guard); the archive stays until a sweep or guard pass collects it"

// TestPublishArchiveKeepsRevivedArchiveAndReportsIt: when the plain file is
// deleted while its archive is being written, publishing still renames the
// archive into place — a .zst that outlives the deletion that raced it. The
// behaviour is kept (idempotent, nothing taken back; the sweep collects the
// archive by its day stamp); what was missing is the report: the source
// surfacing as plain not-exist was swallowed as an expected duplicate offer.
// The tail now marks the error and leaves the talking to the worker, which
// owns the stream's tag.
func TestPublishArchiveKeepsRevivedArchiveAndReportsIt(t *testing.T) {
	body := strings.Repeat("{\"ts\":\"2026-01-01T00:00:00Z\"}\n", 300)
	src, tmp, dst := compressThenLoseSource(t, body)
	logs := captureLogs(t)
	pinLogLevel(t, "info")

	err := publishArchive(src, tmp, dst)

	// Still a not-exist for anything that treats one generically, and marked
	// as the revived archive for the worker that reports it.
	if !errors.Is(err, os.ErrNotExist) || !errors.Is(err, errSourceRemovedMidArchive) {
		t.Fatalf("publishArchive error = %v, want the source's not-exist marked as removed mid-archive", err)
	}
	f, err := os.Open(dst)
	if err != nil {
		t.Fatalf("the revived archive is gone: %v", err)
	}
	defer f.Close()
	dec, err := zstd.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer dec.Close()
	got, err := io.ReadAll(dec)
	if err != nil || string(got) != body {
		t.Fatalf("revived archive content = %d bytes, %v; want the plain file's %d", len(got), err, len(body))
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatalf("tmp left behind by the published archive: %v", err)
	}
	if got := logs.String(); got != "" {
		t.Fatalf("the archive tail logged %q: the worker reports a revived archive, once, with the tag", got)
	}
}

// TestArchiveWorkerReportsRevivedArchiveAtInfoOnly: through the worker, a
// revived archive costs one info line and nothing else — no failure warning
// (the file is archived, not kept plain), no OnChange (the indexer does not
// track a .zst; the deletion itself already signalled on the writer
// goroutine).
func TestArchiveWorkerReportsRevivedArchiveAtInfoOnly(t *testing.T) {
	src, tmp, dst := compressThenLoseSource(t, "{\"n\":1}\n")
	logs := captureLogs(t)
	pinLogLevel(t, "info")
	l := New(Options{Directory: filepath.Dir(src), Tag: "t"})
	l.archiveFn = func(string) error { return publishArchive(src, tmp, dst) }
	changes := make(chan struct{}, 1)
	l.opts.OnChange = func() { changes <- struct{}{} }
	go func() { l.archive <- src; close(l.archive) }()
	l.archiveWorker() // returns when the queue closes

	// Tagged like every other line of the archive path, and naming the
	// archive that outlived its plain file.
	want := "[t] archive " + filepath.Base(dst) + revivedArchiveNote
	if n := strings.Count(logs.String(), revivedArchiveNote); n != 1 {
		t.Errorf("%d revived-archive line(s), want exactly one:\n%s", n, logs.String())
	}
	if got := logs.String(); !strings.Contains(got, want) {
		t.Errorf("worker log = %q, want the revived archive reported: %q", got, want)
	}
	if got := logs.String(); strings.Contains(got, "failed") || strings.Contains(got, "retried") {
		t.Errorf("a revived archive is not a failed one, log says otherwise: %q", got)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Errorf("revived archive missing after the worker ran: %v", err)
	}
	select {
	case <-changes:
		t.Error("OnChange fired for a revived archive")
	default:
	}
}

// TestArchiveZstdIdempotentRerunWithSourceGoneStaysSilent: the other way a
// source surfaces as not-exist is the common one — a duplicate offer after
// the archive already removed the plain file (every sweep re-offers what was
// still queued). Nothing is produced, so nothing is logged: the revived-
// archive line must not turn into one message per duplicate.
func TestArchiveZstdIdempotentRerunWithSourceGoneStaysSilent(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "requests-20260101.log")
	if err := os.WriteFile(src+zstdSuffix, []byte("already compressed"), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := captureLogs(t)
	pinLogLevel(t, "info")

	err := archiveZstd(src) // the plain file is already gone

	if !errors.Is(err, os.ErrNotExist) || errors.Is(err, errSourceRemovedMidArchive) {
		t.Fatalf("archiveZstd error = %v, want a plain not-exist: nothing was produced, so nothing was revived", err)
	}
	if got := logs.String(); got != "" {
		t.Fatalf("a duplicate offer must stay silent, got %q", got)
	}
	if data, err := os.ReadFile(src + zstdSuffix); err != nil || string(data) != "already compressed" {
		t.Fatalf("existing archive = %q, %v; want untouched", data, err)
	}
}

// TestArchiveWorkerStaysSilentOnADuplicateOffer: the same rerun through the
// worker, with the production archive function and no seam replaced. The
// revived-archive report keys on an error the worker receives; a duplicate
// offer must not trip it.
func TestArchiveWorkerStaysSilentOnADuplicateOffer(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "requests-20260101.log")
	if err := os.WriteFile(src+zstdSuffix, []byte("already compressed"), 0o600); err != nil {
		t.Fatal(err)
	}
	logs := captureLogs(t)
	pinLogLevel(t, "info")
	l := New(Options{Directory: dir, Tag: "t"}) // archiveFn is the production archiveZstd
	changes := make(chan struct{}, 1)
	l.opts.OnChange = func() { changes <- struct{}{} }
	go func() { l.archive <- src; close(l.archive) }()
	l.archiveWorker() // returns when the queue closes

	if got := logs.String(); got != "" {
		t.Fatalf("a duplicate offer must stay silent through the worker, got %q", got)
	}
	select {
	case <-changes:
		t.Fatal("OnChange fired for a duplicate offer")
	default:
	}
}
