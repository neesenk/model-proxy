package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// hangingServer never answers: the handler parks until the client goes away
// OR the test releases it. The explicit release is required for Close(): a
// client that aborts before the server wrote any response byte is not always
// observable server-side, so r.Context().Done() alone can leave the handler
// parked and httptest.Server.Close would block forever.
func hangingServer() (*httptest.Server, func()) {
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-done:
		case <-r.Context().Done():
		}
	}))
	return srv, func() { close(done) }
}

// TestOneRequestStreamMeasuresTTFT pins the streaming gauge: time to the
// FIRST body byte. The server holds the body until released, so TTFT must
// cover the hold gap (the sleep creates the measured phenomenon, it is not a
// completion wait).
func TestOneRequestStreamMeasuresTTFT(t *testing.T) {
	var enteredOnce sync.Once
	entered := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		enteredOnce.Do(func() { close(entered) })
		<-release
		_, _ = io.WriteString(w, "data")
	}))
	defer srv.Close()

	s := scenario{
		name:   "stream",
		stream: true,
		build:  func(model string, i int) string { return `{}` },
	}
	done := make(chan result, 1)
	go func() {
		done <- oneRequest(context.Background(), newClient(1), srv.URL, s, "m", 0)
	}()

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("request never reached the server")
	}
	const hold = 60 * time.Millisecond
	time.Sleep(hold)
	close(release)

	select {
	case r := <-done:
		if !r.ok || r.status != http.StatusOK {
			t.Fatalf("result = ok:%v status:%d, want 200 ok", r.ok, r.status)
		}
		// The first byte could not arrive before the release, so TTFT is at
		// least the hold (with timer slop).
		if r.ttftMS < float64(hold.Milliseconds())/2 {
			t.Fatalf("ttftMS = %.1f, want >= %d (hold was %s)", r.ttftMS, hold.Milliseconds()/2, hold)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("oneRequest did not return after release")
	}
}

// TestOneRequestCancelAfterCountsOK pins the cancel-verdict branch: when the
// scenario's OWN per-request timeout fires (parent ctx still alive), the
// client-side abort is the expected outcome of the cancel scenario — ok=true,
// not aborted, not a proxy failure.
func TestOneRequestCancelAfterCountsOK(t *testing.T) {
	srv, release := hangingServer()
	defer srv.Close() // runs after release (LIFO)
	defer release()

	s := scenario{
		name:        "cancel",
		stream:      true,
		cancelAfter: 100 * time.Millisecond,
		build:       func(model string, i int) string { return `{}` },
	}
	r := oneRequest(context.Background(), newClient(1), srv.URL, s, "m", 0)
	if !r.ok {
		t.Fatalf("cancel-scenario result = ok:%v aborted:%v (the per-request timeout firing must count as ok)", r.ok, r.aborted)
	}
	if r.aborted {
		t.Fatal("our own cancelAfter must not mark the request aborted")
	}
	if r.status != 0 {
		t.Fatalf("status = %d, want 0 (no HTTP exchange completed)", r.status)
	}
}

// TestRunScenarioExcludesAbortedAtDeadline pins the aborted-exclusion rule:
// requests still in flight when the SCENARIO deadline expires carry no signal
// about the proxy and must be dropped from the results — neither fake ok nor
// fake err rows may appear.
func TestRunScenarioExcludesAbortedAtDeadline(t *testing.T) {
	srv, release := hangingServer()
	defer srv.Close() // runs after release (LIFO)
	defer release()

	s := scenario{
		name:  "short",
		build: func(model string, i int) string { return `{}` },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	results := runScenario(ctx, newClient(2), srv.URL, "m", s.name, s, 2)

	if len(results) != 0 {
		t.Fatalf("results = %d (%v), want 0: deadline-aborted requests must be excluded", len(results), results)
	}
}
