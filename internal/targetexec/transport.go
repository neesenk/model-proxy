package targetexec

import (
	"bytes"
	"io"
	"net/http"
	"strings"
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

func flushCopy(writer http.ResponseWriter, body io.ReadCloser) streamEnd {
	flusher, _ := writer.(http.Flusher)
	buf := make([]byte, 32<<10)
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

func (writer *timingResponseWriter) Flush() {
	if writer.flusher != nil {
		writer.flusher.Flush()
	}
}
