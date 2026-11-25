package logfile

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"model-proxy/internal/testsyn"
)

// TestRunStopsRefillingWhileArchivesKeepFailing: a systematic failure — a
// full disk, nothing left for the guard to delete — must not cost the whole
// backlog once per sweep. Every failure frees a slot and so asks for a
// refill; from archiveFailBudget failures in a row on, the writer goroutine
// hands over no more parked candidates until a success or the next sweep. A
// sweep then tries exactly one queue's worth plus one more per failure below
// the budget, and warns once per attempt.
//
// The count is fixed, not sampled: every attempt parks before it fails, and
// the test releases it only once Run has handled every signal sent so far.
// The worker then never runs more than one failure ahead of what Run has
// seen.
func TestRunStopsRefillingWhileArchivesKeepFailing(t *testing.T) {
	now := time.Now()
	plain := writeColdDayFiles(t, now, 3*archiveQueueDepth, now.Add(-2*time.Hour))
	logs := captureLogs(t)
	l := New(Options{Directory: filepath.Dir(plain[0]), FilePrefix: "requests-", Tag: "t", HotRetention: time.Hour})
	// The probe is an offer queued after the sweep's attempts ran out. It is
	// taken only once the last of them is over, which anchors the final
	// settle; it is not one of the sweep's attempts.
	const probe = "probe: queued by the test after the budget was spent"
	var attempts atomic.Int32
	parkedAttempt := make(chan chan struct{}, 1)
	l.archiveFn = func(p string) error {
		if p != probe {
			attempts.Add(1)
		}
		release := make(chan struct{})
		parkedAttempt <- release
		<-release
		return errors.New("no space left on device")
	}
	written := make(chan struct{}, 1)
	l.writeLine = func([]byte, time.Time) error {
		written <- struct{}{}
		return nil
	}
	// settled returns once Run has handled every signal the worker has sent
	// and is back at its loop; nothing can change until the parked attempt is
	// released.
	settled := func() {
		testsyn.WaitUntil(t, "Run to take every refill signal so far", func() bool { return len(l.archiveRefill) == 0 })
		l.Enqueue(func(_ time.Time, buf []byte) ([]byte, error) { return append(buf[:0], `{}`...), nil })
		<-written
	}
	// nextAttempt returns the attempt the worker has started and is parked in.
	// The loop below says when one must start; the bound only turns a wrong
	// prediction into a failure instead of a hang.
	nextAttempt := func() chan struct{} {
		testsyn.WaitUntil(t, "the worker to start an attempt", func() bool { return len(parkedAttempt) > 0 })
		return <-parkedAttempt
	}
	goroutines := testsyn.Goroutines()
	go l.Run()
	var fire func() // releases the attempt under way, at most once
	defer func() {
		// A failed wait must not leave the worker parked in an attempt: the
		// final flush would then wait on it for good.
		if fire != nil {
			fire()
		}
		l.Shutdown()
	}()

	for {
		release := nextAttempt() // an attempt is under way, parked before it fails
		var once sync.Once
		fire = func() { once.Do(func() { close(release) }) }
		settled()
		queued, parked, streak := len(l.archive), len(l.archivePending), l.archiveFailStreak.Load()
		fire()
		// This attempt fails. Another one starts only if an offer is already
		// queued or Run hands over exactly one more for this failure.
		if queued == 0 && !(parked > 0 && streak+1 < archiveFailBudget) {
			break
		}
	}
	// The last attempt's warning and signal come after its release, so the
	// settle inside the loop cannot speak for them: the probe anchors those.
	select {
	case l.archive <- probe:
	default:
		t.Fatalf("%d offer(s) still queued once the sweep's attempts ran out", len(l.archive))
	}
	probeRelease := nextAttempt()
	var probeOnce sync.Once
	fire = func() { probeOnce.Do(func() { close(probeRelease) }) }
	settled()

	want := int32(archiveQueueDepth + archiveFailBudget - 1)
	if got := attempts.Load(); got != want {
		t.Fatalf("%d attempt(s) in the sweep, want exactly %d: one queue's worth plus one per failure below the budget", got, want)
	}
	if got := len(parkedAttempt); got != 0 {
		t.Fatalf("%d attempt(s) started once the budget was spent", got)
	}
	if got, want := len(l.archivePending), len(plain)-int(want); got != want {
		t.Fatalf("%d candidate(s) still parked, want the %d the sweep did not reach", got, want)
	}
	if n := strings.Count(logs.String(), "failed (kept plain; retried next sweep)"); n != int(want) {
		t.Fatalf("%d failure warning(s), want one per attempt (%d):\n%s", n, want, logs.String())
	}
	for _, p := range plain {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s did not survive the failed archive: %v", filepath.Base(p), err)
		}
	}
	fire()
	l.Shutdown()
	testsyn.LeakCheck(t, goroutines, "failure budget")
}

// TestRunFailureBudgetDoesNotStarveTheBacklog: the budget counts failures in
// a row. Bad files must not cost the good ones their sweep as long as
// successes keep resetting the streak — one bad file, or several that are
// never the budget's worth in a row. A file the worker failed is not offered
// again within the sweep, so it costs one attempt and one warning.
func TestRunFailureBudgetDoesNotStarveTheBacklog(t *testing.T) {
	const good = 4*archiveQueueDepth + 8
	cases := []struct {
		name string
		bad  []int // positions in scan order of the files whose archive always fails
	}{
		{"one bad file", []int{7}},
		// Three failures in one sweep — the budget's worth — but never in a
		// row: a budget that counted failures without the reset, or in total
		// rather than consecutively, would stop refilling at the third.
		{"bad files never the budget's worth in a row", []int{5, 5 + archiveQueueDepth, 5 + 2*archiveQueueDepth}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			plain := writeColdDayFiles(t, now, good, now.Add(-2*time.Hour))
			bad := make(map[string]bool, len(tc.bad))
			for _, i := range tc.bad {
				bad[plain[i]] = true
			}
			logs := captureLogs(t)
			l := New(Options{
				Directory: filepath.Dir(plain[0]), FilePrefix: "requests-", Tag: "t",
				HotRetention: time.Hour, Retention: 365 * 24 * time.Hour,
			})
			var badAttempts atomic.Int32
			l.archiveFn = func(p string) error {
				if bad[p] {
					badAttempts.Add(1)
					return errors.New("boom")
				}
				return archiveZstd(p)
			}
			goroutines := testsyn.Goroutines()
			go l.Run()
			defer l.Shutdown() // a failed wait must not leave Run behind
			testsyn.WaitUntil(t, "every good file to be archived", func() bool {
				archived := 0
				for _, p := range plain {
					if !bad[p] {
						if _, err := os.Stat(p + zstdSuffix); err == nil {
							archived++
						}
					}
				}
				return archived == good-len(bad)
			})
			l.Shutdown()

			for _, p := range plain {
				_, plainErr := os.Stat(p)
				_, archiveErr := os.Stat(p + zstdSuffix)
				if bad[p] {
					if plainErr != nil || !os.IsNotExist(archiveErr) {
						t.Errorf("bad file %s = plain %v, archive %v; want kept plain, no archive", filepath.Base(p), plainErr, archiveErr)
					}
					continue
				}
				if !os.IsNotExist(plainErr) || archiveErr != nil {
					t.Errorf("good file %s = plain %v, archive %v; want archived away", filepath.Base(p), plainErr, archiveErr)
				}
			}
			if got := badAttempts.Load(); got != int32(len(bad)) {
				t.Errorf("bad file(s) attempted %d time(s) in the sweep, want %d: a failure is not retried before the next one", got, len(bad))
			}
			if n := strings.Count(logs.String(), "failed (kept plain; retried next sweep)"); n != len(bad) {
				t.Errorf("%d failure warning(s), want one per bad file (%d):\n%s", n, len(bad), logs.String())
			}
			testsyn.LeakCheck(t, goroutines, "failure budget")
		})
	}
}

// TestOfferPendingArchivesThrottlesOnTheFailureStreak: the rule itself, with
// the worker's outcomes replayed by hand. A clean streak fills every free
// slot; a streak below the budget hands over one candidate per call and
// waits for its outcome; the budget spent hands over nothing — until a
// success resets the streak. A new dispatch pass fills the queue whatever
// the streak says: the next sweep gets its turn.
func TestOfferPendingArchivesThrottlesOnTheFailureStreak(t *testing.T) {
	now := time.Now()
	plain := writeColdDayFiles(t, now, 3*archiveQueueDepth, now.Add(-2*time.Hour))
	captureLogs(t)
	l := New(Options{Directory: filepath.Dir(plain[0]), FilePrefix: "requests-", Tag: "t", HotRetention: time.Hour})
	l.dispatchArchive(now, "")
	drainOffers(l)
	parked := len(l.archivePending)

	for streak := int32(1); streak < archiveFailBudget; streak++ {
		l.archiveFailStreak.Store(streak)
		l.offerPendingArchives()
		if got := len(l.archivePending); got != parked-int(streak) {
			t.Fatalf("with a streak of %d the refill handed over %d candidate(s), want exactly one", streak, parked-got)
		}
	}

	l.archiveFailStreak.Store(archiveFailBudget)
	l.offerPendingArchives()
	if got := len(l.archive); got != archiveFailBudget-1 {
		t.Fatalf("queue holds %d offer(s) with the budget spent, want the %d handed over below it and no more", got, archiveFailBudget-1)
	}

	l.archiveFailStreak.Store(0) // a success
	l.offerPendingArchives()
	if got := len(l.archive); got != archiveQueueDepth {
		t.Fatalf("queue holds %d offer(s) after a success, want topped up to %d", got, archiveQueueDepth)
	}

	drainOffers(l)
	l.archiveFailStreak.Store(archiveFailBudget)
	l.dispatchArchive(now, "") // the next sweep: every file is still plain and cold
	if got := len(l.archive); got != archiveQueueDepth {
		t.Fatalf("the sweep after a spent budget queued %d file(s), want a full queue of %d", got, archiveQueueDepth)
	}
	l.offerPendingArchives()
	if got := len(l.archive); got != archiveQueueDepth {
		t.Fatalf("queue grew to %d on a refill with the budget still spent, want the sweep's %d and no more", got, archiveQueueDepth)
	}
}
