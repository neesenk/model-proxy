package events

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestServeEventsWithoutHubCancelled covers the exported production wrapper:
// even an already-cancelled client receives the SSE response metadata and
// initial flush before the handler exits.
func TestServeEventsWithoutHubCancelled(t *testing.T) {
	writer := newSyncSSEWriter()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest("GET", "/api/events", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ServeEvents(nil, writer, request)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeEvents did not return after client disconnect")
	}
	select {
	case <-writer.flushed:
	default:
		t.Fatal("ServeEvents did not perform the initial SSE flush")
	}
	assertSSEResponseMetadata(t, writer.statusCode(), writer.Header())
	if got := writer.body(); got != "" {
		t.Fatalf("cancelled no-hub SSE body = %q, want empty", got)
	}
}

// TestServeEventsWithoutHubEmitsComment drives the complete SSE entry core with
// one controlled tick and requires an initial flush followed by one exact
// keepalive comment and a second flush before cancellation.
func TestServeEventsWithoutHubEmitsComment(t *testing.T) {
	writer := newSyncSSEWriter()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest("GET", "/api/events", nil).WithContext(ctx)
	ticks := make(chan time.Time, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveEventsWithTicks(nil, writer, request, ticks)
	}()
	select {
	case <-writer.flushed:
	case <-time.After(time.Second):
		t.Fatal("ServeEvents did not perform the initial SSE flush")
	}
	assertSSEResponseMetadata(t, writer.statusCode(), writer.Header())

	ticks <- time.Now()
	select {
	case <-writer.flushed:
	case <-time.After(time.Second):
		t.Fatalf("keepalive was not written and flushed: %q", writer.body())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("keepalive loop did not stop after cancellation")
	}
	if got := writer.body(); got != ": keepalive\n\n" {
		t.Fatalf("keepalive body = %q, want exact SSE comment", got)
	}
}

func TestServeEventsWithoutHubStopsOnWriteError(t *testing.T) {
	wantErr := errors.New("client gone")
	writer := &keepaliveErrorWriter{
		header:   http.Header{},
		writeErr: wantErr,
		writes:   make(chan string, 1),
		flushes:  make(chan struct{}, 1),
	}
	ticks := make(chan time.Time, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveEventsWithTicks(nil, writer, httptest.NewRequest("GET", "/api/events", nil), ticks)
	}()
	select {
	case <-writer.flushes:
	case <-time.After(time.Second):
		t.Fatal("ServeEvents did not perform the initial SSE flush")
	}
	assertSSEResponseMetadata(t, writer.status, writer.Header())
	ticks <- time.Now()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("keepalive loop did not stop after write error")
	}
	select {
	case got := <-writer.writes:
		if got != ": keepalive\n\n" {
			t.Fatalf("attempted write = %q, want exact SSE comment", got)
		}
	default:
		t.Fatal("keepalive loop returned without attempting a write")
	}
	select {
	case <-writer.flushes:
		t.Fatal("keepalive loop flushed after the write failed")
	default:
	}
}

type keepaliveErrorWriter struct {
	header   http.Header
	status   int
	writeErr error
	writes   chan string
	flushes  chan struct{}
}

func (w *keepaliveErrorWriter) Header() http.Header { return w.header }
func (w *keepaliveErrorWriter) WriteHeader(status int) {
	w.status = status
}
func (w *keepaliveErrorWriter) Write(body []byte) (int, error) {
	w.writes <- string(body)
	return 0, w.writeErr
}
func (w *keepaliveErrorWriter) Flush() {
	select {
	case w.flushes <- struct{}{}:
	default:
	}
}

// TestServeEventsReplaysRecentThenStreams covers the hub path: the handler
// must first flush the recent ring, then stream new published events, and
// exit cleanly on client disconnect.
func TestServeEventsReplaysRecentThenStreams(t *testing.T) {
	hub := NewHub()
	hub.Publish(Event{Type: "end", RequestID: "old", Ts: 1})

	writer := newSyncSSEWriter()
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest("GET", "/api/events", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ServeEvents(hub, writer, request)
	}()

	// Wait for the replay to land, then publish a live event.
	if !writer.waitFor(`"request_id":"old"`, 2*time.Second) {
		t.Fatalf("recent event never replayed: %q", writer.body())
	}
	hub.Publish(Event{Type: "start", RequestID: "live", Ts: 2})
	if !writer.waitFor(`"request_id":"live"`, 2*time.Second) {
		t.Fatalf("live event never streamed: %q", writer.body())
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeEvents did not return after client disconnect")
	}

	// A non-flusher writer must be rejected rather than panicking.
	ServeEvents(hub, &noFlusher{header: http.Header{}}, httptest.NewRequest("GET", "/api/events", nil))
}

// syncSSEWriter is a mutex-guarded ResponseWriter/Flusher so the test can poll
// the written body without racing the handler goroutine.
type syncSSEWriter struct {
	mu      sync.Mutex
	header  http.Header
	status  int
	buf     strings.Builder
	flushed chan struct{}
}

func newSyncSSEWriter() *syncSSEWriter {
	return &syncSSEWriter{header: http.Header{}, flushed: make(chan struct{}, 1)}
}

func (w *syncSSEWriter) Header() http.Header { return w.header }
func (w *syncSSEWriter) WriteHeader(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status = status
}
func (w *syncSSEWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(b)
}
func (w *syncSSEWriter) Flush() {
	select {
	case w.flushed <- struct{}{}:
	default:
	}
}

func (w *syncSSEWriter) body() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func (w *syncSSEWriter) statusCode() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status
}

func (w *syncSSEWriter) waitFor(substr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if strings.Contains(w.body(), substr) {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

type noFlusher struct{ header http.Header }

func (n *noFlusher) Header() http.Header         { return n.header }
func (n *noFlusher) Write(b []byte) (int, error) { return len(b), nil }
func (n *noFlusher) WriteHeader(int)             {}

func assertSSEResponseMetadata(t *testing.T, status int, header http.Header) {
	t.Helper()
	if status != http.StatusOK {
		t.Errorf("SSE status = %d, want %d", status, http.StatusOK)
	}
	for key, want := range map[string]string{
		"content-type":  "text/event-stream",
		"cache-control": "no-cache",
		"connection":    "keep-alive",
	} {
		if got := header.Get(key); got != want {
			t.Errorf("SSE header %s = %q, want %q", key, got, want)
		}
	}
}
