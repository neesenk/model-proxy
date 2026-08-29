package requestlog

import (
	"model-proxy/internal/observe/logx"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	queueCapacity = 2048
	sweepInterval = time.Hour
)

// Options contains already-resolved request-log policy values.
type Options struct {
	Directory    string
	MaxFileSize  int64
	MaxBodyBytes int
	Retention    time.Duration
}

// Logger owns the non-blocking queue and its single JSONL writer goroutine.
// The application lifecycle must stop producers before calling Shutdown.
type Logger struct {
	dir          string
	maxFileSize  int64
	maxBodyBytes int
	retention    time.Duration
	records      chan *Record
	done         chan struct{}
	closed       chan struct{}
	dropped      uint64
	writeErrors  uint64
	dead         uint32
	stopOnce     sync.Once
	writeRecord  func(*Record, time.Time) error
}

// New returns a logger. Run must be started exactly once before Shutdown.
func New(options Options) *Logger {
	return &Logger{
		dir:          options.Directory,
		maxFileSize:  options.MaxFileSize,
		maxBodyBytes: options.MaxBodyBytes,
		retention:    options.Retention,
		records:      make(chan *Record, queueCapacity),
		done:         make(chan struct{}),
		closed:       make(chan struct{}),
	}
}

// Enqueue offers a record without blocking the request path.
func (l *Logger) Enqueue(record *Record) {
	if l == nil {
		return
	}
	select {
	case l.records <- record:
	default:
		n := atomic.AddUint64(&l.dropped, 1)
		if n == 1 || n%1000 == 0 {
			if atomic.LoadUint32(&l.dead) == 1 {
				logx.Warnf("[request_log] logger is dead (setup failed); dropped %d records total", n)
			} else {
				logx.Warnf("[request_log] channel full, dropped %d records total", n)
			}
		}
	}
}

// Run drains queued records until Shutdown asks it to finish. It narrows both
// newly-created and pre-existing storage objects to owner-only permissions.
func (l *Logger) Run() {
	if l == nil {
		return
	}
	defer close(l.closed)
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		logx.Warnf("[request_log] mkdir %s: %v - logging disabled", l.dir, err)
		atomic.StoreUint32(&l.dead, 1)
		return
	}
	if err := os.Chmod(l.dir, 0o700); err != nil {
		logx.Warnf("[request_log] chmod %s: %v - logging disabled", l.dir, err)
		atomic.StoreUint32(&l.dead, 1)
		return
	}
	writer := &fileWriter{dir: l.dir, maxSize: l.maxFileSize}
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
	if record == nil {
		return
	}
	var err error
	if l.writeRecord != nil {
		err = l.writeRecord(record, now)
	} else {
		err = writer.write(record, now)
	}
	if err != nil {
		n := atomic.AddUint64(&l.writeErrors, 1)
		if n == 1 || n%1000 == 0 {
			logx.Warnf("[request_log] write failed (lost %d records total): %v", n, err)
		}
	}
}

func (l *Logger) sweep(now time.Time, activePath string) {
	if l.retention <= 0 {
		return
	}
	cutoff := now.Add(-l.retention)
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		logx.Warnf("[request_log] sweep readdir %s: %v", l.dir, err)
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, "requests-") || !strings.HasSuffix(name, ".log") {
			continue
		}
		path := filepath.Join(l.dir, name)
		if activePath != "" && path == activePath {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(path); err != nil {
				logx.Warnf("[request_log] sweep remove %s: %v", name, err)
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

// Directory returns the JSONL directory.
func (l *Logger) Directory() string {
	if l == nil {
		return ""
	}
	return l.dir
}

// MaxBodyBytes returns the configured capture limit.
func (l *Logger) MaxBodyBytes() int {
	if l == nil {
		return 0
	}
	return l.maxBodyBytes
}
