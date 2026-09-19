package requestlog

import (
	"model-proxy/internal/observe/logfile"
	"time"
)

const (
	// filePrefix names both the active per-day file and its size-rotated
	// archives inside Directory for the default (LLM) stream.
	filePrefix = "requests-"
	// MCPFilePrefix names the split MCP gateway stream's files (mcp-).
	MCPFilePrefix = "mcp-"
)

// Options contains already-resolved request-log policy values. FilePrefix
// selects the stream's file naming ("" = requests-; requestlog.MCPFilePrefix
// for the split MCP stream) — the query side must scan with the same prefix.
type Options struct {
	Directory    string
	FilePrefix   string
	MaxFileSize  int64
	MaxBodyBytes int
	Retention    time.Duration
}

// Logger owns the non-blocking queue and its single JSONL writer goroutine.
// Persistence (queue, rotation, retention, permissions) is delegated to the
// shared observe/logfile sink; this wrapper only binds the request-log record
// encoding and accessors. The application lifecycle must stop producers
// before calling Shutdown.
type Logger struct {
	sink         *logfile.Logger
	maxBodyBytes int
}

// New returns a logger. Run must be started exactly once before Shutdown.
func New(options Options) *Logger {
	prefix := options.FilePrefix
	if prefix == "" {
		prefix = filePrefix
	}
	return &Logger{
		sink: logfile.New(logfile.Options{
			Directory:  options.Directory,
			FilePrefix: prefix,
			MaxBytes:   options.MaxFileSize,
			Retention:  options.Retention,
			Tag:        "request_log",
		}),
		maxBodyBytes: options.MaxBodyBytes,
	}
}

// Enqueue offers a record without blocking the request path; a full queue
// drops the record and counts it in Dropped.
func (l *Logger) Enqueue(record *Record) {
	if l == nil || record == nil {
		return
	}
	l.sink.Enqueue(func(_ time.Time, buf []byte) ([]byte, error) {
		return appendRecordLine(buf[:0], record), nil
	})
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

// Dropped returns how many records a full queue has discarded.
func (l *Logger) Dropped() uint64 {
	if l == nil {
		return 0
	}
	return l.sink.Dropped()
}

// WriteErrors returns how many records failed to persist.
func (l *Logger) WriteErrors() uint64 {
	if l == nil {
		return 0
	}
	return l.sink.WriteErrors()
}

// Directory returns the JSONL directory.
func (l *Logger) Directory() string {
	if l == nil {
		return ""
	}
	return l.sink.Directory()
}

// MaxBodyBytes returns the configured capture limit.
func (l *Logger) MaxBodyBytes() int {
	if l == nil {
		return 0
	}
	return l.maxBodyBytes
}
