package forward

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// TestCountingResponseWriterKeepsConnectionReachable: the executor bounds its
// client writes with a deadline it sets through this wrapper (a peer that
// stops reading must not park a handler past its upstream timeout — serve
// audit 2026-11-26). http.ResponseController only crosses a wrapper that
// Unwraps, and only a FlushError carries a timed-out write back; losing
// either would silently disable the bound on the production path, where
// every response passes through this wrapper.
func TestCountingResponseWriterKeepsConnectionReachable(t *testing.T) {
	const frame = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n"
	type flushed struct {
		counted int64
		err     error
	}
	result := make(chan flushed, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counting := &countingResponseWriter{ResponseWriter: w}
		controller := http.NewResponseController(counting)
		if _, err := counting.Write([]byte(frame)); err != nil {
			result <- flushed{err: err}
			return
		}
		// Control: without a deadline the flush is clean and reports nothing.
		if err := controller.Flush(); err != nil {
			result <- flushed{counted: counting.bytes, err: err}
			return
		}
		if err := controller.SetWriteDeadline(time.Now().Add(-time.Second)); err != nil {
			result <- flushed{counted: counting.bytes, err: err}
			return
		}
		// The small frame lands in the response buffer; the flush is where a
		// past deadline fails the write.
		if _, err := counting.Write([]byte(frame)); err != nil {
			result <- flushed{counted: counting.bytes, err: err}
			return
		}
		result <- flushed{counted: counting.bytes, err: controller.Flush()}
	}))
	t.Cleanup(server.Close)

	resp, err := http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	// The timed-out response never gets its chunked terminator, so the
	// client sees the cut as an unexpected EOF after the delivered prefix.
	delivered, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
	select {
	case got := <-result:
		if !errors.Is(got.err, os.ErrDeadlineExceeded) {
			t.Fatalf("flush error = %v, want the connection's write deadline; the writer no longer reaches the connection", got.err)
		}
		if got.counted != int64(2*len(frame)) {
			t.Fatalf("counted %d bytes, want both writes (%d)", got.counted, 2*len(frame))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the handler")
	}
	if string(delivered) != frame {
		t.Fatalf("client received %q, want exactly the frame flushed before the deadline", delivered)
	}
}
