package logfile

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"model-proxy/internal/testsyn"
)

// writeColdDayFiles creates n per-day rotated files of the "requests-"
// stream, one per day ending three days before now, each with the given
// mtime, and returns their paths in name order — the order the dispatch pass
// lists them in, oldest first — starting with the oldest.
func writeColdDayFiles(t *testing.T, now time.Time, n int, mtime time.Time) []string {
	t.Helper()
	dir := t.TempDir()
	paths := make([]string, n)
	for i := range paths {
		day := now.AddDate(0, 0, -3-(n-1-i)).Format(dayFileLayout)
		p := filepath.Join(dir, "requests-"+day+".log")
		if err := os.WriteFile(p, []byte("{\"n\":1}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		paths[i] = p
	}
	return paths
}

// archivedCount reports how many of the plain files have their .zst in place.
func archivedCount(plain []string) int {
	n := 0
	for _, p := range plain {
		if _, err := os.Stat(p + zstdSuffix); err == nil {
			n++
		}
	}
	return n
}

// TestRunDrainsArchiveBacklogWithoutWaitingForTheNextSweep: more cold files
// than the archive queue holds must all be archived by one sweep. The startup
// scan can offer only archiveQueueDepth of them; the rest used to wait for
// the next hourly tick, capping throughput at 16 files an hour — the worker
// idling next to a directory of cold files.
//
// Order comes from the polled observable, never from a sleep: the deadline
// only ever delays a failure.
func TestRunDrainsArchiveBacklogWithoutWaitingForTheNextSweep(t *testing.T) {
	// Two full queues and then some: no single scan can hand these over.
	now := time.Now()
	plain := writeColdDayFiles(t, now, 2*archiveQueueDepth+8, now.Add(-2*time.Hour))
	dir := filepath.Dir(plain[0])

	l := New(Options{
		Directory: dir, FilePrefix: "requests-", Tag: "t",
		HotRetention: time.Hour,
		// An archive is aged by the day in its name, so retention has to
		// outlast the 40-day span of these names: the shutdown sweep must
		// not collect what the backlog just produced.
		Retention: 365 * 24 * time.Hour,
	})
	logs := captureLogs(t)
	goroutines := testsyn.Goroutines()
	go l.Run()
	defer l.Shutdown() // a failed wait must not leave Run behind
	defer func() {
		if archived := archivedCount(plain); t.Failed() && archived != len(plain) {
			t.Logf("%d of %d cold file(s) archived when the wait gave up", archived, len(plain))
		}
	}()
	testsyn.WaitUntil(t, "every cold file to be archived", func() bool {
		return archivedCount(plain) == len(plain)
	})
	l.Shutdown()

	// One dispatch pass, one backlog warning. The refills that drained the
	// rest say nothing, and the figure is whatever the scan found: the worker
	// may already have freed a slot while it ran.
	if n := strings.Count(logs.String(), "more file(s) waiting"); n != 1 {
		t.Fatalf("%d queue-full warning(s) for one dispatch pass, want exactly one:\n%s", n, logs.String())
	}
	for _, p := range plain {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("plain file after Run = %v, want removed by the archive: %s", err, filepath.Base(p))
		}
		if info, err := os.Stat(p + zstdSuffix); err != nil || info.Size() == 0 {
			t.Fatalf("archive after Run = %v, %v; want a non-empty .zst: %s", info, err, filepath.Base(p))
		}
	}
	if names := logFileNames(t, dir); len(names) != len(plain) {
		t.Fatalf("directory holds %d entries after Run, want the %d archives: %v", len(names), len(plain), names)
	}
	testsyn.LeakCheck(t, goroutines, "archive backlog")
}

// TestDispatchArchiveReportsHowManyCandidatesWaitForTheWorker: a dispatch
// pass that cannot offer everything says how much is left over. Without the
// count the backlog has no observable signal at all — the warning named one
// refused file while thousands could be waiting.
func TestDispatchArchiveReportsHowManyCandidatesWaitForTheWorker(t *testing.T) {
	now := time.Now()
	plain := writeColdDayFiles(t, now, archiveQueueDepth+5, now.Add(-2*time.Hour))
	logs := captureLogs(t)

	l := New(Options{Directory: filepath.Dir(plain[0]), FilePrefix: "requests-", Tag: "t", HotRetention: time.Hour})
	l.dispatchArchive(now, "")

	// The prefix stays as it was, so anything matching a full queue still
	// finds the line; the backlog and the next in line follow it.
	want := fmt.Sprintf("[t] archive queue full; 5 more file(s) waiting, %s next", filepath.Base(plain[archiveQueueDepth]))
	if got := logs.String(); !strings.Contains(got, want) || strings.Count(got, "archive queue full") != 1 {
		t.Fatalf("dispatch log = %q, want exactly one queue-full line naming the backlog: %q", got, want)
	}
	// The oldest archiveQueueDepth candidates sit in the queue, in name order.
	for i, p := range plain[:archiveQueueDepth] {
		select {
		case got := <-l.archive:
			if got != p {
				t.Fatalf("offer %d = %s, want %s", i, got, p)
			}
		default:
			t.Fatalf("only %d of %d candidates offered", i, archiveQueueDepth)
		}
	}
	select {
	case extra := <-l.archive:
		t.Fatalf("queued beyond its depth: %s", extra)
	default:
	}
}

// drainOffers returns everything queued for the archive worker, in order.
func drainOffers(l *Logger) []string {
	var queued []string
	for {
		select {
		case p := <-l.archive:
			queued = append(queued, p)
		default:
			return queued
		}
	}
}

// TestOfferPendingArchivesHandsOverOnlyWhatTheScanParked: the refill a worker
// signal triggers works from the parked list alone. It tops the queue up in
// scan order and stops when the queue is full or the list is empty — and it
// never looks at the directory again. That is also what keeps a file the
// worker failed from being retried on every freed slot: it is as plain and
// as cold as before, but it is no longer parked.
func TestOfferPendingArchivesHandsOverOnlyWhatTheScanParked(t *testing.T) {
	now := time.Now()
	plain := writeColdDayFiles(t, now, 2*archiveQueueDepth, now.Add(-2*time.Hour))
	captureLogs(t) // the scan's backlog warning is not what this test is about
	l := New(Options{Directory: filepath.Dir(plain[0]), FilePrefix: "requests-", Tag: "t", HotRetention: time.Hour})
	l.dispatchArchive(now, "")
	// The worker takes the oldest offer: one slot is free, the rest of the
	// first pass still queued.
	if got := <-l.archive; got != plain[0] {
		t.Fatalf("first offer = %s, want the oldest file %s", got, plain[0])
	}

	// Exactly the next parked file takes the freed slot: the queue is full
	// again, the parked file behind the offers that were already in it.
	l.offerPendingArchives()
	if queued := drainOffers(l); !slices.Equal(queued, plain[1:archiveQueueDepth+1]) {
		t.Fatalf("after one freed slot the queue holds %d offer(s), want the %d still queued followed by the next parked file %s",
			len(queued), archiveQueueDepth-1, plain[archiveQueueDepth])
	}

	// The worker drained that whole queue before Run woke up again: one
	// wake-up hands over everything that is left.
	l.offerPendingArchives()
	if queued := drainOffers(l); !slices.Equal(queued, plain[archiveQueueDepth+1:]) {
		t.Fatalf("refill queued %d file(s), want the remaining %d parked ones in scan order",
			len(queued), len(plain)-archiveQueueDepth-1)
	}

	// Nothing is parked any more, although the directory still holds every
	// one of these files, plain and cold: a further wake-up must not find
	// them.
	l.offerPendingArchives()
	if queued := drainOffers(l); len(queued) != 0 {
		t.Fatalf("refill with an empty parked list queued %v: it rescanned the directory", queued)
	}
	if len(l.archivePending) != 0 {
		t.Fatalf("%d file(s) still parked after the list was handed over", len(l.archivePending))
	}
}

// TestRunRefillSignalWithNothingParkedScansNoDirectory: in steady state — a
// file falls out of the hot window, is archived, the worker reports its
// freed slot — the signal must not cost a directory scan. Order comes from
// the writer goroutine's own record writes: a line seen written after the
// signal was taken proves Run has handled the signal and is back at records.
func TestRunRefillSignalWithNothingParkedScansNoDirectory(t *testing.T) {
	now := time.Now()
	dir := t.TempDir()
	// The only plain file sits inside the hot window: the startup scan finds
	// nothing to offer, so nothing is parked. Its tmp is live while the
	// source stands.
	hot := filepath.Join(dir, "requests-"+now.AddDate(0, 0, -1).Format(dayFileLayout)+".log")
	if err := os.WriteFile(hot, []byte("{\"n\":1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(hot, now, now); err != nil {
		t.Fatal(err)
	}
	tmp := hot + zstdTmpSuffix
	if err := os.WriteFile(tmp, []byte("z"), 0o600); err != nil {
		t.Fatal(err)
	}

	l := New(Options{Directory: dir, FilePrefix: "requests-", Tag: "t", HotRetention: time.Hour})
	written := make(chan struct{}, 1)
	l.writeLine = func([]byte, time.Time) error {
		written <- struct{}{}
		return nil
	}
	line := func() {
		l.Enqueue(func(_ time.Time, buf []byte) ([]byte, error) { return append(buf[:0], `{}`...), nil })
	}
	goroutines := testsyn.Goroutines()
	go l.Run()
	defer l.Shutdown() // a failed wait must not leave Run behind
	line()
	<-written // Run is in its loop: the startup sweep, scan included, is over

	// What only a scan would act on: an orphan tmp is reaped, a cold file is
	// offered. Neither existed when the startup scan ran.
	if err := os.Remove(hot); err != nil {
		t.Fatal(err)
	}
	cold := filepath.Join(dir, "requests-"+now.AddDate(0, 0, -40).Format(dayFileLayout)+".log")
	if err := os.WriteFile(cold, []byte("{\"n\":1}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(cold, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}

	select {
	case l.archiveRefill <- struct{}{}: // the worker's signal, with nothing parked
	default:
		t.Fatal("a refill signal was already pending: no archive has run")
	}
	testsyn.WaitUntil(t, "Run to take the refill signal", func() bool { return len(l.archiveRefill) == 0 })
	line()
	<-written

	if _, err := os.Stat(tmp); err != nil {
		t.Fatalf("the refill signal scanned the directory: the orphan tmp is gone (%v)", err)
	}
	if queued := drainOffers(l); len(queued) != 0 {
		t.Fatalf("the refill signal scanned the directory: queued %v", queued)
	}
	if _, err := os.Stat(cold + zstdSuffix); err == nil {
		t.Fatal("the cold file created after the startup scan was archived: the refill signal scanned the directory")
	}
	l.Shutdown()

	// Still there once Run has stopped: the exit path sweeps retention only
	// and never dispatches.
	for _, p := range []string{tmp, cold} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s gone after Run: %v", filepath.Base(p), err)
		}
	}
	testsyn.LeakCheck(t, goroutines, "refill signal")
}

// TestArchiveWorkerRefillSignalNeverWaitsForRun: the refill signal is
// fire-and-forget and coalesces. Run is parked inside a record write, so it
// reads no signal; the worker must still work through every offer it holds —
// a blocking send would stop it behind the writer goroutine, and at shutdown
// nobody reads the signal at all. Three freed slots leave one signal.
//
// Ordered by gates, never by timing: the first archive waits until Run is
// parked, so no signal exists before Run stops reading them. Left to race, Run
// may take the first signal before it parks, and "one signal pending" then
// depends on whether the worker finished before or after that.
func TestArchiveWorkerRefillSignalNeverWaitsForRun(t *testing.T) {
	now := time.Now()
	plain := writeColdDayFiles(t, now, 3, now.Add(-2*time.Hour))
	l := New(Options{Directory: filepath.Dir(plain[0]), FilePrefix: "requests-", Tag: "t", HotRetention: time.Hour})
	done := make(chan string, len(plain))
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var first, releaseFirstOnce sync.Once
	l.archiveFn = func(p string) error {
		first.Do(func() {
			close(firstStarted)
			<-releaseFirst
		})
		done <- p
		return nil
	}
	stalled := make(chan struct{})
	release := make(chan struct{})
	var stallOnce sync.Once
	l.writeLine = func([]byte, time.Time) error {
		stallOnce.Do(func() { close(stalled) })
		<-release
		return nil
	}
	goroutines := testsyn.Goroutines()
	go l.Run()
	// shutdown unblocks Run's write and ends the logger, at most once. It
	// bounds the wait: under a blocking signal a worker stuck in its send
	// never reaches the stop check, Shutdown never returns — that is the
	// failure to report, not a test to hang on.
	shutdownReturned := make(chan struct{})
	var shutdownOnce sync.Once
	shutdown := func() {
		shutdownOnce.Do(func() {
			close(release)
			go func() {
				l.Shutdown()
				close(shutdownReturned)
			}()
		})
		testsyn.WaitUntil(t, "Shutdown to return", func() bool {
			select {
			case <-shutdownReturned:
				return true
			default:
				return false
			}
		})
	}
	defer func() {
		// A failed wait must not leave either goroutine parked.
		releaseFirstOnce.Do(func() { close(releaseFirst) })
		shutdown()
	}()
	l.Enqueue(func(_ time.Time, buf []byte) ([]byte, error) { return append(buf[:0], `{}`...), nil })
	<-firstStarted // the worker is inside its first archive: nothing has completed, no signal exists
	<-stalled      // Run is inside the record write, off its loop
	releaseFirstOnce.Do(func() { close(releaseFirst) })

	testsyn.WaitUntil(t, "the worker to finish every offer it holds", func() bool { return len(done) == len(plain) })
	if got := len(l.archiveRefill); got != 1 {
		t.Fatalf("%d refill signal(s) pending after %d freed slots, want the single one a parked Run has not taken",
			got, len(plain))
	}
	shutdown()
	testsyn.LeakCheck(t, goroutines, "refill signal")
}

// TestDispatchArchiveKeepsParkedCandidatesWhenTheScanFails: a dispatch pass
// that cannot read the directory leaves the parked list as it was. The
// candidates are still cold and still waiting; dropping them would silence a
// backlog until the next sweep.
func TestDispatchArchiveKeepsParkedCandidatesWhenTheScanFails(t *testing.T) {
	now := time.Now()
	plain := writeColdDayFiles(t, now, archiveQueueDepth+2, now.Add(-2*time.Hour))
	dir := filepath.Dir(plain[0])
	logs := captureLogs(t)
	l := New(Options{Directory: dir, FilePrefix: "requests-", Tag: "t", HotRetention: time.Hour})
	l.dispatchArchive(now, "")
	drainOffers(l)

	// The directory is gone altogether: the next pass cannot even list it.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	l.dispatchArchive(now, "")
	if !strings.Contains(logs.String(), "[t] archive readdir ") {
		t.Fatalf("scan log = %q, want the readdir failure reported", logs.String())
	}

	if queued := drainOffers(l); len(queued) != 0 {
		t.Fatalf("a failed scan offered %v", queued)
	}
	l.offerPendingArchives()
	if queued := drainOffers(l); !slices.Equal(queued, plain[archiveQueueDepth:]) {
		t.Fatalf("after the failed scan the refill queued %v, want the %d candidates the last good scan parked",
			queued, len(plain)-archiveQueueDepth)
	}
}
