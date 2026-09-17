package targetexec

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const contextOverflowPeek = 64 << 10

func isSSE(header http.Header) bool {
	for _, contentType := range header.Values("content-type") {
		if strings.Contains(contentType, "text/event-stream") {
			return true
		}
	}
	return false
}

func peekResponseBody(response *http.Response, limit int) []byte {
	peek, _ := io.ReadAll(io.LimitReader(response.Body, int64(limit)))
	response.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(peek), response.Body), response.Body}
	return peek
}

func sniffSSEFraming(response *http.Response) bool {
	const window = 16
	buf := make([]byte, window)
	n, err := response.Body.Read(buf)
	for n < window && err == nil && sniffUndecided(buf[:n]) {
		m, readErr := response.Body.Read(buf[n:])
		n += m
		err = readErr
		if m == 0 && readErr == nil {
			break
		}
	}
	peek := buf[:n]
	response.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(peek), response.Body), response.Body}
	peek = bytes.TrimSpace(peek)
	if bytes.HasPrefix(peek, []byte(":")) {
		return true
	}
	for _, marker := range [][]byte{[]byte("event:"), []byte("data:"), []byte("id:"), []byte("retry:")} {
		if bytes.HasPrefix(peek, marker) {
			return true
		}
	}
	return false
}

func sniffUndecided(peek []byte) bool {
	peek = bytes.TrimSpace(peek)
	if len(peek) == 0 {
		return true
	}
	for _, marker := range [][]byte{[]byte("event:"), []byte("data:"), []byte("id:"), []byte("retry:")} {
		if len(peek) < len(marker) && bytes.HasPrefix(marker, peek) {
			return true
		}
	}
	return false
}

func copyHeaderWhitelist(dst, src http.Header, keys ...string) {
	for _, key := range keys {
		// Values + Add, not Get + Set: whitelisted headers like anthropic-beta
		// are legal multi-valued, and collapsing them to the first value
		// silently drops beta-capability declarations.
		for _, value := range src.Values(key) {
			if value != "" {
				dst.Add(key, value)
			}
		}
	}
}

type streamEnd int

const (
	streamEOF streamEnd = iota
	streamClientGone
	streamUpstreamErr
)

type countingReadCloser struct {
	io.ReadCloser
	Count int64
}

func (reader *countingReadCloser) Read(buffer []byte) (int, error) {
	count, err := reader.ReadCloser.Read(buffer)
	reader.Count += int64(count)
	return count, err
}

// flushBufPool recycles the 32KB streaming copy buffer: one fresh allocation
// per forwarded response was the proxy's largest flat allocation source under
// the forward benchmarks.
var flushBufPool = sync.Pool{New: func() any {
	buf := make([]byte, 32<<10)
	return &buf
}}

func flushCopy(writer http.ResponseWriter, body io.ReadCloser) streamEnd {
	flusher, _ := writer.(http.Flusher)
	bufp := flushBufPool.Get().(*[]byte)
	defer flushBufPool.Put(bufp)
	buf := *bufp
	for {
		n, err := body.Read(buf)
		if n > 0 {
			if _, writeErr := writer.Write(buf[:n]); writeErr != nil {
				return streamClientGone
			}
			if flusher != nil {
				flusher.Flush()
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

// heartbeatFrame is an SSE comment line: a protocol-level no-op that every
// compliant SSE parser (openai / anthropic / responses SDKs) skips. Written
// only into client-facing event streams to reset client/edge idle timers
// while a slow-TTFT upstream is still silent.
var heartbeatFrame = []byte(": ping\n\n")

// flushCopyHeartbeat is flushCopy plus a keepalive ticker: whenever the
// upstream body stays silent for interval, a heartbeat comment frame goes to
// the client. Body reads run on a helper goroutine so the ticker can fire
// during a blocked Read; only the calling goroutine writes to the client.
// Heartbeats bypass the recording chain (they are written to the client,
// never read from the body) and the ttft marker (via WriteHeartbeat).
func flushCopyHeartbeat(writer http.ResponseWriter, body io.ReadCloser, interval time.Duration) streamEnd {
	flusher, _ := writer.(http.Flusher)
	heartbeatWriter, hasHeartbeat := writer.(interface {
		WriteHeartbeat([]byte) (int, error)
	})
	writeHeartbeat := func() error {
		if hasHeartbeat {
			_, err := heartbeatWriter.WriteHeartbeat(heartbeatFrame)
			return err
		}
		_, err := writer.Write(heartbeatFrame)
		return err
	}
	bufp := flushBufPool.Get().(*[]byte)
	buf := *bufp
	type readResult struct {
		n   int
		err error
	}
	readReq := make(chan struct{})
	readRes := make(chan readResult, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for range readReq {
			n, err := body.Read(buf)
			readRes <- readResult{n, err}
			if err != nil {
				return
			}
		}
	}()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	recycle := true
	defer func() {
		close(readReq)
		if recycle {
			<-readerDone
			flushBufPool.Put(bufp)
		}
	}()
	// At most one read request is outstanding: the reader goroutine parks in
	// body.Read until the upstream produces bytes, and re-sending the request
	// before that read completes would deadlock the loop.
	readReq <- struct{}{}
	for {
		select {
		case result := <-readRes:
			if result.n > 0 {
				if _, err := writer.Write(buf[:result.n]); err != nil {
					return streamClientGone
				}
				if flusher != nil {
					flusher.Flush()
				}
				ticker.Reset(interval)
			}
			if result.err != nil {
				if result.err == io.EOF {
					return streamEOF
				}
				return streamUpstreamErr
			}
			readReq <- struct{}{}
		case <-ticker.C:
			if err := writeHeartbeat(); err != nil {
				// The reader goroutine may still be parked in body.Read holding
				// buf; the caller's body.Close unblocks it, but the buffer must
				// NOT return to the pool before that — abandon it to the GC.
				recycle = false
				return streamClientGone
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
}

type timingResponseWriter struct {
	http.ResponseWriter
	firstByte    time.Time
	hasFirstByte bool
	flusher      http.Flusher
}

func newTimingResponseWriter(writer http.ResponseWriter) *timingResponseWriter {
	flusher, _ := writer.(http.Flusher)
	return &timingResponseWriter{ResponseWriter: writer, flusher: flusher}
}

func (writer *timingResponseWriter) Write(body []byte) (int, error) {
	if !writer.hasFirstByte {
		writer.hasFirstByte = true
		writer.firstByte = time.Now()
	}
	return writer.ResponseWriter.Write(body)
}

// WriteHeartbeat writes a client-facing keepalive frame WITHOUT recording the
// first byte: TTFT measures the upstream's first real byte, not proxy padding.
func (writer *timingResponseWriter) WriteHeartbeat(frame []byte) (int, error) {
	return writer.ResponseWriter.Write(frame)
}

func (writer *timingResponseWriter) Flush() {
	if writer.flusher != nil {
		writer.flusher.Flush()
	}
}
