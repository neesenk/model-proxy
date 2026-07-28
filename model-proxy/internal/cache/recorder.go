package cache

import (
	"io"
	"net/http"
)

// Recorder tees a response stream into a bounded buffer. A response is
// complete only after a clean EOF and while remaining within the size cap.
type Recorder struct {
	source    io.ReadCloser
	body      []byte
	max       int
	truncated bool
	sawEOF    bool
}

// NewRecorder wraps source with a capture limit.
func NewRecorder(source io.ReadCloser, max int) *Recorder {
	return &Recorder{source: source, max: max}
}

func (r *Recorder) Read(buffer []byte) (int, error) {
	n, err := r.source.Read(buffer)
	if err == io.EOF {
		r.sawEOF = true
	}
	if n > 0 && !r.truncated {
		if len(r.body)+n > r.max {
			fit := r.max - len(r.body)
			if fit > 0 {
				r.body = append(r.body, buffer[:fit]...)
			}
			r.truncated = true
		} else {
			r.body = append(r.body, buffer[:n]...)
		}
	}
	return n, err
}

func (r *Recorder) Close() error {
	return r.source.Close()
}

// Complete reports whether the source reached a clean EOF within the cap.
func (r *Recorder) Complete() bool {
	return r != nil && r.sawEOF && !r.truncated
}

// Body returns the captured prefix. Store.Put detaches it before retention.
func (r *Recorder) Body() []byte {
	if r == nil {
		return nil
	}
	return r.body
}

// HeaderForCapturedBody returns headers describing the client-facing bytes.
func HeaderForCapturedBody(header http.Header, converted, modeMismatch, clientWantsStream bool) http.Header {
	result := header.Clone()
	if converted {
		result.Del("Content-Length")
		result.Del("Transfer-Encoding")
	}
	if modeMismatch {
		if clientWantsStream {
			result.Set("Content-Type", "text/event-stream")
		} else {
			result.Set("Content-Type", "application/json")
		}
	}
	return result
}
