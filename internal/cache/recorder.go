package cache

import (
	"io"
	"net/http"
	"strings"
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
// Hop-by-hop headers (RFC 9110 §7.6.1) and every header NAMED by Connection
// are stripped exactly like the live forward path does — a cached replay must
// not resurrect connection-scoped headers the live response never delivered.
func HeaderForCapturedBody(header http.Header, converted, modeMismatch, clientWantsStream bool) http.Header {
	result := header.Clone()
	connectionNamed := map[string]bool{}
	for _, connectionValue := range result.Values("Connection") {
		for _, token := range strings.Split(connectionValue, ",") {
			if named := strings.TrimSpace(token); named != "" {
				connectionNamed[strings.ToLower(named)] = true
			}
		}
	}
	for name := range result {
		lower := strings.ToLower(name)
		if hopByHopHeaders[lower] || connectionNamed[lower] {
			result.Del(name)
		}
	}
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

// hopByHopHeaders mirrors the live forward path's connection-scoped set
// (internal/targetexec). Kept as a copy because the cache package is a
// repository leaf and must not import it.
var hopByHopHeaders = map[string]bool{
	"connection":          true,
	"keep-alive":          true,
	"proxy-authenticate":  true,
	"proxy-authorization": true,
	"te":                  true,
	"trailer":             true,
	"upgrade":             true,
}
