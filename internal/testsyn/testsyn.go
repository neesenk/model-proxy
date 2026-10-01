// Package testsyn provides the repository's shared synchronization primitives
// for concurrency TESTS. The policy authority is docs/engineering/testing.md
// §「并发测试模式目录」; this package is the canonical provider for the
// polling, adversarial-latency, and goroutine-accounting patterns so packages
// stop hand-rolling divergent copies (12+ wait helpers existed at extraction
// time, with 2-50ms backoffs and inconsistent failure messages).
//
// The package is test-only infrastructure (like internal/cli/clitest): it
// imports "testing" and is imported exclusively from _test.go files.
package testsyn

import (
	"fmt"
	"net/http"
	"runtime"
	"testing"
	"time"
)

// pollBackoff is the re-check interval inside WaitUntil: short enough that
// tests settle fast on quick machines, long enough not to spin.
const pollBackoff = 5 * time.Millisecond

// defaultTimeout bounds every WaitUntil that does not name its own.
const defaultTimeout = 5 * time.Second

// WaitUntil polls cond until it holds, failing the test with what (and the
// elapsed time) when it never does. Ordering comes from the polled predicate
// — never from a fixed sleep (docs §并发测试模式目录, pattern P1).
func WaitUntil(t testing.TB, what string, cond func() bool) {
	t.Helper()
	WaitUntilFor(t, what, defaultTimeout, cond)
}

// WaitUntilFor is WaitUntil with an explicit timeout budget.
func WaitUntilFor(t testing.TB, what string, timeout time.Duration, cond func() bool) {
	t.Helper()
	if err := waitUntil(what, timeout, pollBackoff, cond); err != nil {
		t.Fatal(err)
	}
}

func waitUntil(what string, timeout, backoff time.Duration, cond func() bool) error {
	deadline := time.Now().Add(timeout)
	started := time.Now()
	for !cond() {
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %v waiting for %s", time.Since(started).Round(time.Millisecond), what)
		}
		time.Sleep(backoff)
	}
	return nil
}

// SlowRound wraps a handler function with a fixed per-request latency — the
// adversarial injector for window-class concurrency tests (pattern P8): a
// test that pins behavior across a timing window proves the behavior is
// independent of in-flight round duration by running once with a delay that
// dwarfs any plausible scheduling margin (e.g. 300ms against a 25ms window).
func SlowRound(d time.Duration, h func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(d)
		h(w, r)
	}
}

// Goroutines snapshots the goroutine count for a later LeakCheck. Take the
// baseline BEFORE constructing the unit under test, run the scenario, Close
// everything, then LeakCheck: tests must not leave goroutines behind
// (AGENTS.md testing rules). Only meaningful in non-parallel tests (a
// parallel sibling inflates the count); the pattern directory says so.
func Goroutines() int {
	return runtime.NumGoroutine()
}

// LeakCheck waits (bounded by the default 5s) for the goroutine count to
// settle back to the baseline taken before the test's setup, then fails with
// the surplus if it never does. Background owners need their Close/Stop/wait
// called first — this helper verifies the drain, it does not replace it.
func LeakCheck(t testing.TB, baseline int, what string) {
	t.Helper()
	LeakCheckFor(t, baseline, defaultTimeout, what)
}

// LeakCheckFor is LeakCheck with an explicit settle budget.
func LeakCheckFor(t testing.TB, baseline int, timeout time.Duration, what string) {
	t.Helper()
	if err := settleGoroutines(baseline, timeout, pollBackoff); err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func settleGoroutines(baseline int, timeout, backoff time.Duration) error {
	err := waitUntil("goroutine count to return to baseline", timeout, backoff, func() bool {
		return runtime.NumGoroutine() <= baseline
	})
	if err != nil {
		return fmt.Errorf("goroutine leak: %d before setup, %d after Close/drain still running (missing Stop/Wait/close on a background owner?)",
			baseline, runtime.NumGoroutine())
	}
	return nil
}
