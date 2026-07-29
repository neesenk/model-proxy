package main

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"time"
)

// isSSE reports whether the response is an SSE stream (content-type
// text/event-stream). Only such responses are wrapped by the usage scanner —
// plain JSON / chunked-but-not-SSE bodies pay no scanner overhead.
func isSSE(h http.Header) bool {
	for _, ct := range h.Values("content-type") {
		if strings.Contains(ct, "text/event-stream") {
			return true
		}
	}
	return false
}

// countingReadCloser counts the bytes streamed through it, for the empty-200
// postmortem in tryTarget (zero bytes on a committed 2xx → model-level failure).
type countingReadCloser struct {
	rc io.ReadCloser
	n  int64
}

func (c *countingReadCloser) Read(p []byte) (int, error) {
	n, err := c.rc.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countingReadCloser) Close() error { return c.rc.Close() }

// flushCopy reads, writes, and flushes per chunk, supporting SSE streaming.
// Stops immediately if the client disconnects (write error), so the proxy
// doesn't keep pulling the upstream stream after the client is gone.
// streamEnd reports how flushCopy terminated (used by the empty-200 postmortem:
// only a clean EOF with zero bytes proves an empty upstream body).
type streamEnd int

const (
	streamEOF         streamEnd = iota // upstream body read to a clean EOF
	streamClientGone                   // client write failed (disconnect) — upstream unread
	streamUpstreamErr                  // upstream read error (not EOF)
)

// flushCopy reads, writes, and flushes per chunk, supporting SSE streaming.
// Stops immediately if the client disconnects (write error), so the proxy
// doesn't keep pulling the upstream stream after the client is gone.
func flushCopy(w http.ResponseWriter, rc io.ReadCloser) streamEnd {
	fl, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := rc.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				// Client disconnected — stop reading upstream.
				return streamClientGone
			}
			if fl != nil {
				fl.Flush()
			}
		}
		if err != nil {
			if err == io.EOF {
				return streamEOF
			}
			return streamUpstreamErr
		}
	}
}

// contextOverflowPeek caps how far into a 4xx body tryTarget reads when
// sniffing for a context-overflow error. 64 KiB is far beyond any error JSON.
const contextOverflowPeek = 64 << 10

// peekResponseBody reads up to n bytes from resp.Body and returns them, then
// RESTORES resp.Body so the commit path re-reads the full body transparently
// (MultiReader: peeked prefix + remaining stream; Close still reaches the
// original body). Used to sniff a 4xx body for a context-overflow error without
// consuming it; a read error is best-effort (peeked holds what was read, and no
// byte is lost either way).
func peekResponseBody(resp *http.Response, n int) []byte {
	peeked, _ := io.ReadAll(io.LimitReader(resp.Body, int64(n)))
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(peeked), resp.Body), resp.Body}
	return peeked
}

// sniffSSEFraming peeks at the first bytes of a <300 response whose
// content-type did not declare SSE and reports whether the body starts with
// SSE framing. Besides event:/data:, the SSE grammar permits comment heartbeats
// (": ping") and id:/retry: fields before the first data event. Like
// peekResponseBody it restores resp.Body, so the commit path re-reads every byte
// transparently.
//
// It reads ONCE before deciding to wait: a single Read returns as soon as any
// bytes are buffered, whereas io.ReadAll(LimitReader(16)) would BLOCK until
// the 16-byte window is full or EOF. Only when that first chunk leaves the
// verdict undecided — empty/whitespace, or a proper prefix of a known SSE field
// marker (a short TCP segment can split inside "event:") — does it keep reading
// to the window. A comment heartbeat is a decisive SSE marker, so it returns
// immediately instead of waiting for the next event and delaying the stream.
func sniffSSEFraming(resp *http.Response) bool {
	const window = 16
	buf := make([]byte, window)
	n, readErr := resp.Body.Read(buf)
	total := n
	for total < window && readErr == nil && sniffUndecided(buf[:total]) {
		n, readErr = resp.Body.Read(buf[total:])
		total += n
		if n == 0 && readErr == nil {
			break // defensive: an io.Reader should not return no progress
		}
	}
	peeked := buf[:total]
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(peeked), resp.Body), resp.Body}
	peek := bytes.TrimSpace(peeked)
	return hasSSEPrefix(peek)
}

// sniffUndecided reports whether a first peek could still turn out to be SSE
// framing once more bytes arrive: nothing but whitespace so far, or a proper
// prefix of a known field marker.
var sseFieldPrefixes = [][]byte{
	[]byte("event:"),
	[]byte("data:"),
	[]byte("id:"),
	[]byte("retry:"),
}

func sniffUndecided(peeked []byte) bool {
	peek := bytes.TrimSpace(peeked)
	if len(peek) == 0 {
		return true
	}
	for _, marker := range sseFieldPrefixes {
		if len(peek) < len(marker) && bytes.HasPrefix(marker, peek) {
			return true
		}
	}
	return false
}

func hasSSEPrefix(peek []byte) bool {
	if bytes.HasPrefix(peek, []byte(":")) {
		return true
	}
	for _, marker := range sseFieldPrefixes {
		if bytes.HasPrefix(peek, marker) {
			return true
		}
	}
	return false
}

// timingResponseWriter wraps the client ResponseWriter to capture time-to-first-
// token: the instant of the first response body byte written to the client
// (the SSE first event, or the first byte of a buffered body). It delegates
// Write/WriteHeader/Header unchanged and implements http.Flusher so SSE
// flushing (flushCopy's w.(http.Flusher)) keeps working through the wrapper.
// firstByte is zero until the first Write; callers treat a zero value as "no
// byte was written" and fall back to the total latency as the TTFT.
type timingResponseWriter struct {
	http.ResponseWriter
	firstByte    time.Time
	hasFirstByte bool
	flusher      http.Flusher // nil if the underlying writer isn't a Flusher
}

func newTimingResponseWriter(w http.ResponseWriter) *timingResponseWriter {
	fl, _ := w.(http.Flusher)
	return &timingResponseWriter{ResponseWriter: w, flusher: fl}
}

func (t *timingResponseWriter) Write(p []byte) (int, error) {
	if !t.hasFirstByte {
		t.hasFirstByte = true
		t.firstByte = time.Now()
	}
	return t.ResponseWriter.Write(p)
}

// Flush delegates to the underlying writer's Flush so SSE chunk flushing through
// the wrapper is a no-op change (flushCopy asserts http.Flusher; without this
// the assertion would fail and SSE would not flush until the buffer filled).
func (t *timingResponseWriter) Flush() {
	if t.flusher != nil {
		t.flusher.Flush()
	}
}

func copyHeaderWhitelist(dst, src http.Header, keys ...string) {
	for _, k := range keys {
		if v := src.Get(k); v != "" {
			dst.Set(k, v)
		}
	}
}
