// Package seclog is the security audit log leaf package. It owns the audit
// Record schema and offline top-K queries; JSONL persistence (queue, size/day
// rotation, retention sweeps, owner-only permissions) is delegated to the
// shared observe/logfile sink — the same pattern as the request log. Its only
// model-proxy imports are the logx level-filtering leaf and observe/logfile;
// it depends on nothing else in the repo.
//
// Red line: Record.Names carries pattern-type or path-category names only.
// Secret values never enter a Record — this package never inspects payloads,
// and producers must not smuggle matched content into Detail either.
package seclog

import (
	"encoding/json"
	"fmt"
	"model-proxy/internal/observe/logfile"
	"time"
)

const (
	// DefaultMaxBytes is the built-in per-file size cap.
	DefaultMaxBytes = 16 << 20 // 16 MiB
	// DefaultRetention is the built-in retention window for rotated files.
	DefaultRetention = 30 * 24 * time.Hour

	filePrefix = "security-"
)

// Audit event kinds.
const (
	KindSecret = "secret"
	KindPath   = "path"
	KindDrift  = "drift"
)

// Record is one line in the JSONL security audit log. Ts is unix
// milliseconds; a zero Ts is stamped by the writer at persistence time.
type Record struct {
	Ts        int64    `json:"ts"`
	Kind      string   `json:"kind"`
	RequestID string   `json:"request_id,omitempty"`
	Agent     string   `json:"agent,omitempty"`
	Protocol  string   `json:"protocol,omitempty"`
	Exposed   string   `json:"exposed,omitempty"`
	Names     []string `json:"names,omitempty"`
	Action    string   `json:"action,omitempty"`
	// Verdict is the AI second-opinion outcome for records from (or
	// fail-opened out of) guard.adjudicate: "high" | "error" | "skipped".
	// Empty = the classic immediate record. Like every Record field it
	// carries classification labels only, never matched content.
	Verdict string `json:"verdict,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// Options carries resolved persistence policy. MaxBytes <= 0 selects
// DefaultMaxBytes. Retention == 0 keeps rotated files forever; Retention < 0
// selects DefaultRetention.
type Options struct {
	MaxBytes  int64
	Retention time.Duration
}

func (o Options) normalized() Options {
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultMaxBytes
	}
	if o.Retention < 0 {
		o.Retention = DefaultRetention
	}
	return o
}

// Logger owns the non-blocking queue and its single JSONL writer goroutine,
// backed by the shared logfile sink. The application lifecycle must stop
// producers before calling Shutdown.
type Logger struct {
	sink *logfile.Logger
}

// New returns a logger for dir. Run must be started exactly once before
// Shutdown.
func New(dir string, opts Options) (*Logger, error) {
	if dir == "" {
		return nil, fmt.Errorf("seclog: empty directory")
	}
	opts = opts.normalized()
	return &Logger{
		sink: logfile.New(logfile.Options{
			Directory:  dir,
			FilePrefix: filePrefix,
			MaxBytes:   opts.MaxBytes,
			Retention:  opts.Retention,
			Tag:        "seclog",
		}),
	}, nil
}

// Enqueue offers a record without blocking the request path; a full queue
// drops the record and counts it in Dropped.
func (l *Logger) Enqueue(record *Record) {
	if l == nil || record == nil {
		return
	}
	l.sink.Enqueue(func(now time.Time, _ []byte) ([]byte, error) {
		if record.Ts == 0 {
			record.Ts = now.UnixMilli()
		}
		return json.Marshal(record)
	})
}

// Dropped returns how many records a full queue has discarded.
func (l *Logger) Dropped() uint64 {
	if l == nil {
		return 0
	}
	return l.sink.Dropped()
}

// Directory returns the JSONL directory.
func (l *Logger) Directory() string {
	if l == nil {
		return ""
	}
	return l.sink.Directory()
}

// Run drains queued records until Shutdown asks it to finish.
func (l *Logger) Run() {
	if l == nil {
		return
	}
	l.sink.Run()
}

// Shutdown drains accepted records and waits for Run to close the file.
func (l *Logger) Shutdown() {
	if l == nil {
		return
	}
	l.sink.Shutdown()
}
