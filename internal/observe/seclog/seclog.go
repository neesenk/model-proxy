// Package seclog is the security audit log leaf package. It owns the audit
// Record schema and the queryable SQLite store (security.db — exact-match
// hits, drift, medium/high verdicts and fail-open records; low verdicts are
// the ignored tier and stay out of the query surface); the full-fidelity
// JSONL trail (queue, size/day rotation, retention sweeps, owner-only
// permissions, EVERY record including lows) is delegated to the shared
// observe/logfile sink — the same pattern as the request log. Its only
// model-proxy imports are the logx level-filtering leaf and observe/logfile;
// it depends on nothing else in the repo.
//
// Red line: Record.Names carries pattern-type or path-category names only.
// Secret values never enter a Record — this package never inspects payloads,
// and producers must not smuggle matched content into Reason/Evidence/Detail
// either (the adjudication channel scrubs those before they land here).
package seclog

import (
	"encoding/json"
	"fmt"
	"model-proxy/internal/observe/logfile"
	"model-proxy/internal/observe/logx"
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

// Record is one audit event. It is written to the full JSONL trail (every
// record) and, unless it is an ignored-tier low verdict, to the queryable
// SQLite store. Ts is unix milliseconds; a zero Ts is stamped by the writer
// at persistence time.
type Record struct {
	Ts        int64    `json:"ts"`
	Kind      string   `json:"kind"`
	RequestID string   `json:"request_id,omitempty"`
	SessionID string   `json:"session_id,omitempty"`
	Agent     string   `json:"agent,omitempty"`
	Protocol  string   `json:"protocol,omitempty"`
	Exposed   string   `json:"exposed,omitempty"`
	Names     []string `json:"names,omitempty"`
	Action    string   `json:"action,omitempty"`
	// Verdict is the AI second-opinion outcome for records from (or
	// fail-opened out of) guard.adjudicate: "high" | "medium" | "error" |
	// "skipped" ("low" exists in the JSONL trail only). Empty = the classic
	// immediate record, including exact-match interception hits. Like every
	// Record field it carries classification labels and scrubbed model text
	// only, never matched content.
	Verdict string `json:"verdict,omitempty"`
	// Reason is the scrubbed judgment logic of an LLM verdict; Evidence the
	// scrubbed factual basis the model cited. Both are capped by the
	// adjudication channel before they reach a Record.
	Reason   string `json:"reason,omitempty"`
	Evidence string `json:"evidence,omitempty"`
	// Model is the judging model id of an LLM verdict.
	Model  string `json:"model,omitempty"`
	Detail string `json:"detail,omitempty"`
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

// Logger owns the non-blocking queue, its single writer goroutine and the
// SQLite store handle. Every enqueued record lands in the JSONL trail; the
// same write also inserts into security.db unless the record is an
// ignored-tier low verdict (the insert runs on the logfile writer goroutine,
// so one connection serializes all writes). A store that fails to open
// degrades to JSONL-only — the guard and its raw trail must not stop because
// the queryable half is unavailable — with a warning log. The application
// lifecycle must stop producers before calling Shutdown.
type Logger struct {
	sink  *logfile.Logger
	store *store
}

// New returns a logger for dir. Run must be started exactly once before
// Shutdown.
func New(dir string, opts Options) (*Logger, error) {
	if dir == "" {
		return nil, fmt.Errorf("seclog: empty directory")
	}
	opts = opts.normalized()
	l := &Logger{
		sink: logfile.New(logfile.Options{
			Directory:  dir,
			FilePrefix: filePrefix,
			MaxBytes:   opts.MaxBytes,
			Retention:  opts.Retention,
			Tag:        "seclog",
		}),
	}
	if st, err := openStore(dir); err != nil {
		logx.Warnf("seclog: queryable store unavailable, JSONL trail only: %v", err)
	} else {
		l.store = st
	}
	return l, nil
}

// Enqueue offers a record without blocking the request path; a full queue
// drops the record (both halves) and counts it in Dropped. The SQLite insert
// happens inside the encode hook on the writer goroutine, after the record's
// Ts is stamped and before the JSONL line is written; an insert failure is
// logged and leaves the JSONL line intact — the full trail is the source of
// record, the store is a queryable projection.
func (l *Logger) Enqueue(record *Record) {
	if l == nil || record == nil {
		return
	}
	l.sink.Enqueue(func(now time.Time, _ []byte) ([]byte, error) {
		if record.Ts == 0 {
			record.Ts = now.UnixMilli()
		}
		line, err := json.Marshal(record)
		if err != nil {
			return nil, err
		}
		if err := l.store.insert(record); err != nil {
			logx.Warnf("%v", err)
		}
		return line, nil
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

// Shutdown drains accepted records and waits for Run to close the file and
// the store handle (inserts happen on the writer goroutine, so closing after
// the sink drained is race-free).
func (l *Logger) Shutdown() {
	if l == nil {
		return
	}
	l.sink.Shutdown()
	if err := l.store.Close(); err != nil {
		logx.Warnf("seclog: close store: %v", err)
	}
}
