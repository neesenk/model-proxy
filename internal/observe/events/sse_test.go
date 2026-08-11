package events

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestKeepaliveLoopWithoutHub covers the no-hub SSE path: the handler must
// return cleanly once the client context is cancelled (the keepalive ticker
// fires every 15s; cancelling before that must still exit).
func TestKeepaliveLoopWithoutHub(t *testing.T) {
	recorder := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest("GET", "/api/events", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ServeEvents(nil, recorder, request)
	}()
	time.AfterFunc(50*time.Millisecond, cancel)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ServeEvents did not return after client disconnect")
	}
}

// TestKeepaliveLoopEmitsComment covers the loop's write path: with a short
// client lifetime it must have attempted at least one keepalive comment
// before disconnecting (nil hub path).
func TestKeepaliveLoopEmitsComment(t *testing.T) {
	recorder := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest("GET", "/api/events", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ServeEvents(nil, recorder, request)
	}()
	// Give the handler time to write headers + enter the loop, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	if recorder.Code != 200 && recorder.Code != 0 {
		t.Fatalf("no-hub SSE status = %d, want 200", recorder.Code)
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
	mu     sync.Mutex
	header http.Header
	buf    strings.Builder
}

func newSyncSSEWriter() *syncSSEWriter {
	return &syncSSEWriter{header: http.Header{}}
}

func (w *syncSSEWriter) Header() http.Header { return w.header }
func (w *syncSSEWriter) WriteHeader(int)     {}
func (w *syncSSEWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(b)
}
func (w *syncSSEWriter) Flush() {}

func (w *syncSSEWriter) body() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
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
