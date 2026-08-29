// Package seclog is the security audit log leaf package. It owns the audit
// Record schema, JSONL persistence with owner-only permissions (files 0600,
// directory 0700), size/day rotation, retention sweeps and offline top-K
// queries. It is a pure leaf: standard library only, no model-proxy imports.
//
// Red line: Record.Names carries pattern-type or path-category names only.
// Secret values never enter a Record — this package never inspects payloads,
// and producers must not smuggle matched content into Detail either.
package seclog

import (
	"fmt"
	"model-proxy/internal/observe/logx"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	queueCapacity = 512
	sweepInterval = time.Hour
	dirMode       = 0o700

	// DefaultMaxBytes is the built-in per-file size cap.
	DefaultMaxBytes = 16 << 20 // 16 MiB
	// DefaultRetention is the built-in retention window for rotated files.
	DefaultRetention = 30 * 24 * time.Hour
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
	Detail    string   `json:"detail,omitempty"`
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

// Logger owns the non-blocking queue and its single JSONL writer goroutine.
// The application lifecycle must stop producers before calling Shutdown.
type Logger struct {
	dir         string
	maxBytes    int64
	retention   time.Duration
	records     chan *Record
	done        chan struct{}
	closed      chan struct{}
	dropped     uint64
	writeErrors uint64
	stopOnce    sync.Once
	// writeRecord is a test seam replacing the file writer; nil in production.
	writeRecord func(*Record, time.Time) error
}

// New returns a logger for dir. Run must be started exactly once before
// Shutdown.
func New(dir string, opts Options) (*Logger, error) {
	if dir == "" {
		return nil, fmt.Errorf("seclog: empty directory")
	}
	opts = opts.normalized()
	return &Logger{
		dir:       dir,
		maxBytes:  opts.MaxBytes,
		retention: opts.Retention,
		records:   make(chan *Record, queueCapacity),
		done:      make(chan struct{}),
		closed:    make(chan struct{}),
	}, nil
}

// Enqueue offers a record without blocking the request path; a full queue
// drops the record and counts it in Dropped.
func (l *Logger) Enqueue(record *Record) {
	if l == nil || record == nil {
		return
	}
	select {
	case l.records <- record:
	default:
		n := atomic.AddUint64(&l.dropped, 1)
		if n == 1 || n%1000 == 0 {
			logx.Warnf("[seclog] queue full, dropped %d records total", n)
		}
	}
}

// Dropped returns how many records a full queue has discarded.
func (l *Logger) Dropped() uint64 {
	if l == nil {
		return 0
	}
	return atomic.LoadUint64(&l.dropped)
}

// Directory returns the JSONL directory.
func (l *Logger) Directory() string {
	if l == nil {
		return ""
	}
	return l.dir
}

// Run drains queued records until Shutdown asks it to finish. It narrows both
// newly-created and pre-existing storage objects to owner-only permissions.
func (l *Logger) Run() {
	if l == nil {
		return
	}
	defer close(l.closed)
	if err := os.MkdirAll(l.dir, dirMode); err != nil {
		logx.Warnf("[seclog] mkdir %s: %v - logging disabled", l.dir, err)
		return
	}
	if err := os.Chmod(l.dir, dirMode); err != nil {
		logx.Warnf("[seclog] chmod %s: %v - logging disabled", l.dir, err)
		return
	}
	writer := &fileWriter{dir: l.dir, maxSize: l.maxBytes}
	defer writer.close()
	writer.open(time.Now())
	ticker := time.NewTicker(sweepInterval)
	defer ticker.Stop()
	l.sweep(time.Now(), writer.path)
	for {
		select {
		case record := <-l.records:
			l.write(writer, record, time.Now())
		case <-ticker.C:
			l.sweep(time.Now(), writer.path)
		case <-l.done:
			for {
				select {
				case record := <-l.records:
					l.write(writer, record, time.Now())
				default:
					l.sweep(time.Now(), writer.path)
					return
				}
			}
		}
	}
}

func (l *Logger) write(writer *fileWriter, record *Record, now time.Time) {
	var err error
	if l.writeRecord != nil {
		err = l.writeRecord(record, now)
	} else {
		err = writer.write(record, now)
	}
	if err != nil {
		n := atomic.AddUint64(&l.writeErrors, 1)
		if n == 1 || n%1000 == 0 {
			logx.Warnf("[seclog] write failed (lost %d records total): %v", n, err)
		}
	}
}

// sweep removes rotated files older than the retention window. The active
// file is never removed. Retention == 0 disables sweeping.
func (l *Logger) sweep(now time.Time, activePath string) {
	if l.retention <= 0 {
		return
	}
	cutoff := now.Add(-l.retention)
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		logx.Warnf("[seclog] sweep readdir %s: %v", l.dir, err)
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !isLogFile(entry.Name()) {
			continue
		}
		path := filepath.Join(l.dir, entry.Name())
		if activePath != "" && path == activePath {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(path); err != nil {
				logx.Warnf("[seclog] sweep remove %s: %v", entry.Name(), err)
			}
		}
	}
}

// Shutdown drains accepted records and waits for Run to close the file.
func (l *Logger) Shutdown() {
	if l == nil {
		return
	}
	l.stopOnce.Do(func() { close(l.done) })
	<-l.closed
}

func isLogFile(name string) bool {
	return strings.HasPrefix(name, filePrefix) && strings.HasSuffix(name, fileSuffix)
}
