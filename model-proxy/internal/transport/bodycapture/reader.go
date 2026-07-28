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

// Reader passes bytes through unchanged while retaining at most max bytes for
// observation. It is intended for one reader goroutine; Close is idempotent
// and invokes the callback and underlying Close at most once.
type Reader struct {
	source    io.ReadCloser
	buffer    bytes.Buffer
	total     int64
	max       int
	truncated bool
	onClose   Callback
	closeOnce sync.Once
}

// New wraps source with bounded capture. The reader preserves the source's
// Read return values exactly; capture never changes bytes delivered downstream.
func New(source io.ReadCloser, max int, onClose Callback) *Reader {
	return &Reader{source: source, max: max, onClose: onClose}
}

// Read forwards to the source while retaining a prefix of the returned bytes.
func (r *Reader) Read(buffer []byte) (int, error) {
	n, err := r.source.Read(buffer)
	if n > 0 {
		r.total += int64(n)
		if r.buffer.Len() < r.max {
			room := r.max - r.buffer.Len()
			if n <= room {
				r.buffer.Write(buffer[:n])
			} else {
				r.buffer.Write(buffer[:room])
				r.truncated = true
			}
		}
	}
	return n, err
}

// Close invokes the callback once, then closes the source once. The first
// Close result is returned; subsequent calls return nil.
func (r *Reader) Close() error {
	var firstErr error
	r.closeOnce.Do(func() {
		if r.onClose != nil {
			r.onClose(r.buffer.Bytes(), r.total, r.truncated)
		}
		firstErr = r.source.Close()
	})
	return firstErr
}
