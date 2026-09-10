package app

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
	observeevents "model-proxy/internal/observe/events"
	"model-proxy/internal/protocol"
	"model-proxy/internal/targetexec"
)

// TestCaptureResponseProgressWhenSubscribed verifies that CaptureResponse wraps
// the body with a progress tap when the event hub has subscribers, and that
// reading the body emits progress events for the right request id.
func TestCaptureResponseProgressWhenSubscribed(t *testing.T) {
	p := &Proxy{processServices: processServices{events: observeevents.NewHub()}}
	effects := targetExecutionEffects{proxy: p, generation: 1}

	ch, _, cancel := p.events.Subscribe()
	defer cancel()

	bodyText := bytes.Repeat([]byte("a"), 100)
	src := io.NopCloser(bytes.NewReader(bodyText))
	wrapped := effects.CaptureResponse(src, sampleAttempt("req-progress"))

	// Drain the wrapped body.
	got, err := io.ReadAll(wrapped)
	if err != nil {
		t.Fatalf("read wrapped body: %v", err)
	}
	if !bytes.Equal(got, bodyText) {
		t.Fatalf("wrapped body changed: got %d bytes, want %d", len(got), len(bodyText))
	}
	if err := wrapped.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	var progress []observeevents.Event
	collectDone := time.After(50 * time.Millisecond)
loop:
	for {
		select {
		case e := <-ch:
			if e.Type == "progress" {
				progress = append(progress, e)
			}
		case <-collectDone:
			break loop
		}
	}

	if len(progress) == 0 {
		t.Fatal("expected progress events, got none")
	}
	first := progress[0]
	if first.RequestID != "req-progress" {
		t.Fatalf("progress request_id = %q, want req-progress", first.RequestID)
	}
	last := progress[len(progress)-1]
	if last.ReceivedBytes != int64(len(bodyText)) {
		t.Fatalf("final received_bytes = %d, want %d", last.ReceivedBytes, len(bodyText))
	}
}

// TestCaptureResponseProgressNoSubscriberFastPath verifies that CaptureResponse
// returns the original body unchanged when no one is subscribed to the events hub.
func TestCaptureResponseProgressNoSubscriberFastPath(t *testing.T) {
	p := &Proxy{processServices: processServices{events: observeevents.NewHub()}}
	effects := targetExecutionEffects{proxy: p, generation: 1}

	src := io.NopCloser(bytes.NewReader([]byte("hello")))
	wrapped := effects.CaptureResponse(src, sampleAttempt("req-no-sub"))
	if wrapped != src {
		t.Fatal("CaptureResponse wrapped body when no subscribers exist")
	}
}

func sampleAttempt(requestID string) targetexec.AttemptDTO {
	return targetexec.AttemptDTO{
		Target:   configdomain.RouteTarget{Provider: "teststatic", Model: "test-model"},
		Protocol: protocol.OpenAI,
		Request:  httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil),
		Response: &http.Response{StatusCode: 200, Header: make(http.Header)},
		Scope: targetexec.Scope{
			Agent: "curl",
			Log: targetexec.LogContext{
				RequestID: requestID,
				Exposed:   "test-model",
			},
		},
	}
}
