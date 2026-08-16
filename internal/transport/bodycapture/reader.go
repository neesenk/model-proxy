// Package bodycapture provides bounded, pass-through response body capture.
package bodycapture

import (
	"bytes"
	"io"
	"sync"
)

// Callback receives the captured prefix, full byte count, and truncation flag
// when a Reader is closed for the first time.
//
// The captured slice is only valid for the duration of the callback. Callers
// that retain it must copy it.
type Callback func(captured []byte, total int64, truncated bool)

// maxPooledCaptureBuffers bounds how many capture buffers the free-list
// retains; larger captures fall back to per-request allocation.
const maxPooledCaptureBuffers = 8

// captureBuffers is a small bounded free-list of capture buffers. Unlike
// sync.Pool it survives GC cycles: large captures generate enough garbage
// that GC would clear a sync.Pool between requests, forcing the buffer to
// re-grow (and re-allocate ~2x its size) on every request.
var captureBuffers struct {
	mu      sync.Mutex
	buffers []*bytes.Buffer
}

func getCaptureBuffer() *bytes.Buffer {
	captureBuffers.mu.Lock()
	defer captureBuffers.mu.Unlock()
	if n := len(captureBuffers.buffers); n > 0 {
		buffer := captureBuffers.buffers[n-1]
		captureBuffers.buffers[n-1] = nil
		captureBuffers.buffers = captureBuffers.buffers[:n-1]
		return buffer
	}
	return new(bytes.Buffer)
}

func putCaptureBuffer(buffer *bytes.Buffer) {
	buffer.Reset()
	captureBuffers.mu.Lock()
	defer captureBuffers.mu.Unlock()
	if len(captureBuffers.buffers) < maxPooledCaptureBuffers {
		captureBuffers.buffers = append(captureBuffers.buffers, buffer)
	}
}

// Reader passes bytes through unchanged while retaining at most max bytes for
// observation. It is intended for one reader goroutine; Close is idempotent
// and invokes the callback and underlying Close at most once.
type Reader struct {
	source    io.ReadCloser
	buffer    *bytes.Buffer // nil when max == 0
	total     int64
	max       int
	truncated bool
	onClose   Callback
	closeOnce sync.Once
}

// New wraps source with bounded capture. The reader preserves the source's
// Read return values exactly; capture never changes bytes delivered downstream.
func New(source io.ReadCloser, max int, onClose Callback) *Reader {
	if max < 0 {
		max = 0
	}
	r := &Reader{source: source, max: max, onClose: onClose}
	if max > 0 {
		r.buffer = getCaptureBuffer()
	}
	return r
}

// capturedLen reports the retained prefix length.
func (r *Reader) capturedLen() int {
	if r.buffer == nil {
		return 0
	}
	return r.buffer.Len()
}

// capturedBytes returns the retained prefix, valid until the callback returns.
func (r *Reader) capturedBytes() []byte {
	if r.buffer == nil {
		return nil
	}
	return r.buffer.Bytes()
}

// Read forwards to the source while retaining a prefix of the returned bytes.
func (r *Reader) Read(buffer []byte) (int, error) {
	n, err := r.source.Read(buffer)
	if n > 0 {
		r.total += int64(n)
		room := r.max - r.capturedLen()
		if room > n {
			room = n
		}
		if room > 0 {
			_, _ = r.buffer.Write(buffer[:room])
		}
		if n > room {
			r.truncated = true
		}
	}
	return n, err
}

// Close invokes the callback once, returns the capture buffer to the pool,
// then closes the source once. The first Close result is returned; subsequent
// calls return nil.
func (r *Reader) Close() error {
	var firstErr error
	r.closeOnce.Do(func() {
		if r.onClose != nil {
			r.onClose(r.capturedBytes(), r.total, r.truncated)
		}
		if r.buffer != nil {
			putCaptureBuffer(r.buffer)
			r.buffer = nil
		}
		firstErr = r.source.Close()
	})
	return firstErr
}
