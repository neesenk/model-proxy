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

	// A goroutine that outlives the (shortened) settle budget: the leak must
	// be REPORTED, then released so the test itself does not leak.
	leaked := make(chan struct{})
	defer close(leaked)
	go func() { <-leaked }()

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
	// A goroutine that is RUNNING (synchronized via started, so the counter
	// already includes it) and outlives the settle budget.
	leaked := make(chan struct{})
	defer close(leaked)
	started := make(chan struct{})
	go func() {
		close(started)
		<-leaked
	}()
	<-started
	baseline := Goroutines() - 1
	func() {
		defer func() { _ = recover() }()
		LeakCheckFor(stub, baseline, 30*time.Millisecond, "pilot scenario")
	}()
	if !stub.failed || !strings.Contains(stub.message, "goroutine leak") {
		t.Fatalf("Fatal path wrong: failed=%v message=%q", stub.failed, stub.message)
	}
}
