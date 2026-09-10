package events

import (
	"bytes"
	"io"
	"time"
)

// ProgressMeta carries the stable identity fields for progress events.
type ProgressMeta struct {
	RequestID     string
	Agent         string
	Protocol      string
	Exposed       string
	Provider      string
	UpstreamModel string
}

// PublishProgress is called by ProgressReader when a new progress event should
// be emitted. The caller builds and publishes the Event.
type PublishProgress func(text string, receivedBytes int64)

// Default progress throttle/cap constants. They are exported so tests can
// construct readers with smaller values.
const (
	// ProgressMaxTextBytes is the head-capped prefix kept for the UI.
	ProgressMaxTextBytes = 32 << 10
	// ProgressThrottleInterval is the minimum time between progress events.
	ProgressThrottleInterval = 250 * time.Millisecond
	// ProgressThrottleBytes is the minimum new bytes between progress events.
	ProgressThrottleBytes = 16 << 10
)

// ProgressReader passes response bytes through while accumulating a bounded
// head prefix and throttling progress callbacks. It is intended for one reader
// goroutine; Read and Close are driven by the same executor goroutine, so no
// internal mutex is needed.
type ProgressReader struct {
	source        io.ReadCloser
	meta          ProgressMeta
	publish       PublishProgress
	maxTextBytes  int
	throttleBytes int64
	throttleEvery time.Duration

	buf                bytes.Buffer
	receivedBytes      int64
	lastPublishedBytes int64
	lastPublishedAt    time.Time
	closed             bool
}

// NewProgressReader wraps source with a bounded, throttled progress tap.
// publish is invoked with the accumulated head prefix and total bytes seen.
// Passing zero values for maxTextBytes/throttleBytes/throttleEvery selects the
// package defaults.
func NewProgressReader(source io.ReadCloser, meta ProgressMeta, publish PublishProgress) *ProgressReader {
	return NewProgressReaderWithLimits(source, meta, publish, ProgressMaxTextBytes, ProgressThrottleBytes, ProgressThrottleInterval)
}

// NewProgressReaderWithLimits is the testable constructor.
func NewProgressReaderWithLimits(source io.ReadCloser, meta ProgressMeta, publish PublishProgress, maxTextBytes int, throttleBytes int64, throttleEvery time.Duration) *ProgressReader {
	if maxTextBytes <= 0 {
		maxTextBytes = ProgressMaxTextBytes
	}
	if throttleBytes <= 0 {
		throttleBytes = ProgressThrottleBytes
	}
	if throttleEvery <= 0 {
		throttleEvery = ProgressThrottleInterval
	}
	return &ProgressReader{
		source:        source,
		meta:          meta,
		publish:       publish,
		maxTextBytes:  maxTextBytes,
		throttleBytes: throttleBytes,
		throttleEvery: throttleEvery,
	}
}

// Read forwards bytes and accumulates a bounded head prefix for progress events.
func (r *ProgressReader) Read(p []byte) (int, error) {
	n, err := r.source.Read(p)
	if n > 0 {
		r.receivedBytes += int64(n)
		r.writeHead(p[:n])
		r.maybePublish()
	}
	return n, err
}

// Close flushes any pending progress event and closes the source once.
func (r *ProgressReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	r.flush()
	return r.source.Close()
}

func (r *ProgressReader) writeHead(p []byte) {
	room := r.maxTextBytes - r.buf.Len()
	if room <= 0 {
		return
	}
	if room > len(p) {
		room = len(p)
	}
	if room > 0 {
		r.buf.Write(p[:room])
	}
}

func (r *ProgressReader) maybePublish() {
	if r.publish == nil {
		return
	}
	first := r.lastPublishedAt.IsZero()
	byBytes := r.receivedBytes-r.lastPublishedBytes >= r.throttleBytes
	byTime := !first && time.Since(r.lastPublishedAt) >= r.throttleEvery
	if first || byBytes || byTime {
		r.publishLocked()
	}
}

func (r *ProgressReader) flush() {
	if r.publish == nil {
		return
	}
	if r.receivedBytes == r.lastPublishedBytes {
		return
	}
	r.publishLocked()
}

func (r *ProgressReader) publishLocked() {
	r.publish(r.buf.String(), r.receivedBytes)
	r.lastPublishedBytes = r.receivedBytes
	r.lastPublishedAt = time.Now()
}
