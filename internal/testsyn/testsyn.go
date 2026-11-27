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
	"net"
	"net/http"
	"runtime"
	"sync"
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

// SilentPeerListener is a listener whose connections are unbuffered
// in-memory pipes: every byte the server writes blocks until the peer reads
// it — the fixture for "the peer stops reading" tests (pattern P9). A client
// that sends its request and then reads nothing is a stalled reader
// deterministically, from the first response byte. Kernel sockets cannot give
// that: how many bytes a non-reading peer absorbs before Write blocks
// (socket buffers, autotuning, zero-window probe backoff) is platform state a
// short test budget races — observed on macOS loopback, where each 32 KiB
// write still went through every few tens of milliseconds.
type SilentPeerListener struct {
	// conns is the accept backlog: like a real listener, a connection made
	// before the server Accepts waits here instead of blocking the dialer.
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

// silentPeerBacklog bounds connections made ahead of their Accept.
const silentPeerBacklog = 64

// NewSilentPeerListener returns a listener no server is listening on yet;
// hand it to the server (httptest.Server.Listener) and dial with Connect.
func NewSilentPeerListener() *SilentPeerListener {
	return &SilentPeerListener{conns: make(chan net.Conn, silentPeerBacklog), closed: make(chan struct{})}
}

// Connect returns the client end of a fresh connection to the listener; the
// server receives the other end from its next Accept. It fails with
// net.ErrClosed once the listener is closed.
func (l *SilentPeerListener) Connect() (net.Conn, error) {
	select {
	case <-l.closed:
		return nil, net.ErrClosed
	default:
	}
	serverEnd, clientEnd := net.Pipe()
	select {
	case l.conns <- serverEnd:
		return clientEnd, nil
	case <-l.closed:
		_ = serverEnd.Close()
		_ = clientEnd.Close()
		return nil, net.ErrClosed
	}
}

// Accept implements net.Listener.
func (l *SilentPeerListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.conns:
		return conn, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

// Close implements net.Listener.
func (l *SilentPeerListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

// Addr implements net.Listener.
func (l *SilentPeerListener) Addr() net.Addr { return silentPeerAddr{} }

type silentPeerAddr struct{}

func (silentPeerAddr) Network() string { return "pipe" }
func (silentPeerAddr) String() string  { return "pipe" }

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
