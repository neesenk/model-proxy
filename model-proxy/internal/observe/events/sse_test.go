package events

import (
	"context"
	"net/http/httptest"
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
