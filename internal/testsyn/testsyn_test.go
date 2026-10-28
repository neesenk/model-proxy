package testsyn

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWaitUntilImmediate(t *testing.T) {
	if err := waitUntil("already-true condition", time.Second, pollBackoff, func() bool { return true }); err != nil {
		t.Fatalf("immediate condition failed: %v", err)
	}
}

func TestWaitUntilDelayed(t *testing.T) {
	flips := time.Now().Add(20 * time.Millisecond)
	if err := waitUntil("condition flip", time.Second, pollBackoff, func() bool {
		return time.Now().After(flips)
	}); err != nil {
		t.Fatalf("delayed condition failed: %v", err)
	}
}

func TestWaitUntilTimeoutNamesWhat(t *testing.T) {
	err := waitUntil("the shadow upstream hit", 30*time.Millisecond, pollBackoff, func() bool { return false })
	if err == nil {
		t.Fatal("never-true condition unexpectedly succeeded")
	}
	// The failure must name what was awaited — the diagnostic value of the
	// shared helper over the twelve hand-rolled copies it replaces.
	for _, want := range []string{"the shadow upstream hit", "timed out"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
}

func TestSlowRoundInjectsLatencyAndForwards(t *testing.T) {
	called := false
	h := SlowRound(50*time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusTeapot)
	})
	started := time.Now()
	rec := httptest.NewRecorder()
	h(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if elapsed := time.Since(started); elapsed < 50*time.Millisecond {
		t.Fatalf("elapsed %v, want ≥ the injected 50ms", elapsed)
	}
	if !called || rec.Code != http.StatusTeapot {
		t.Fatalf("handler not forwarded: called=%v code=%d", called, rec.Code)
	}
}

func TestSettleGoroutinesDetectsLeak(t *testing.T) {
	baseline := Goroutines()
	// A short-lived goroutine that rejoins before the check: settle must pass.
	done := make(chan struct{})
	go func() { <-done }()
	close(done)

	// Goroutines that outlive the (shortened) settle budget: the leak must
	// be REPORTED, then released so the test itself does not leak. Their
	// starts are synchronized (both are counted before settle runs), and
	// there are TWO of them: under -shuffle a sibling test's deferred
	// goroutine release (e.g. TestLeakCheckFatalPath's) can still be exiting
	// when this test captures its baseline, and one such transient inside
	// the baseline masks exactly one surplus when it exits inside the settle
	// window — the count comparison cannot tell "leak gone" from
	// "unrelated exit". Masking two live leaks needs two unrelated exits
	// racing the same 50ms budget, which is not a plausible scheduling
	// outcome.
	leaked := make(chan struct{})
	defer close(leaked)
	var started [2]chan struct{}
	for i := range started {
		started[i] = make(chan struct{})
		go func(started chan struct{}) { close(started); <-leaked }(started[i])
	}
	for _, s := range started {
		<-s
	}

	if err := settleGoroutines(baseline, 50*time.Millisecond, pollBackoff); err == nil {
		t.Fatal("leaked goroutine not detected")
	}
}

func TestSettleGoroutinesPassesAfterReap(t *testing.T) {
	baseline := Goroutines()
	reap := make(chan struct{})
	go func() { <-reap }()
	close(reap)
	if err := settleGoroutines(baseline, defaultTimeout, pollBackoff); err != nil {
		t.Fatalf("reaped goroutine reported as leak: %v", err)
	}
}

// stubTB redirects Fatal into a recoverable panic so the wrappers' failure
// paths are covered without failing the test binary (Fatal on a real T is a
// Goexit that cannot be recovered).
type stubTB struct {
	*testing.T
	failed  bool
	message string
}

type stubFatal struct{ msg string }

func (s *stubTB) Fatal(args ...any) {
	s.failed = true
	s.message = fmt.Sprint(args...)
	panic(stubFatal{msg: s.message})
}

func (s *stubTB) Fatalf(format string, args ...any) {
	s.Fatal(fmt.Sprintf(format, args...))
}

func TestWaitUntilForFatalPath(t *testing.T) {
	stub := &stubTB{T: t}
	func() {
		defer func() { _ = recover() }()
		WaitUntilFor(stub, "never-true condition", time.Millisecond, func() bool { return false })
	}()
	if !stub.failed || !strings.Contains(stub.message, "never-true condition") {
		t.Fatalf("Fatal path wrong: failed=%v message=%q", stub.failed, stub.message)
	}
}

func TestLeakCheckFatalPath(t *testing.T) {
	stub := &stubTB{T: t}
	// TWO goroutines that are RUNNING (synchronized via started, so the counter
	// already includes them) and outlive the settle budget. The doubled leak
	// keeps the check decisive even with ±1 foreign goroutine churn under
	// shuffle: a leftover from an earlier test exiting inside the settle
	// window still leaves the count above baseline (same hardening as
	// TestLeakCheck's dual-leak self-test, 09e85d9).
	leaked := make(chan struct{})
	defer close(leaked)
	started := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		go func() {
			started <- struct{}{}
			<-leaked
		}()
	}
	<-started
	<-started
	baseline := Goroutines() - 2
	func() {
		defer func() { _ = recover() }()
		LeakCheckFor(stub, baseline, 30*time.Millisecond, "pilot scenario")
	}()
	if !stub.failed || !strings.Contains(stub.message, "goroutine leak") {
		t.Fatalf("Fatal path wrong: failed=%v message=%q", stub.failed, stub.message)
	}
}
