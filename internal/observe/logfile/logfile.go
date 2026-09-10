// Package logfile is the shared rotating-JSONL log sink behind the observe
// leaf loggers (request log, security audit log). It owns the common
// "request-log pattern": a non-blocking record queue drained by a single
// writer goroutine, a size/day-rotating JSONL file writer with owner-only
// permissions (files 0600, directory 0700), hourly retention sweeps, and a
// synchronous single-line append for callers outside the daemon lifecycle.
//
// File naming is per day: the active file is <prefix>YYYYMMDD.log, so a
// process restart on the same day appends to the existing file instead of
// accumulating one file per run, and the writer opens lazily on the first
// line so a run that never logs leaves no (empty) file behind. Size rotation
// renames the active file to <prefix>YYYYMMDD--HHMMSS-<seq>.log; a day change
// simply switches to the new day's name (no rename).
//
// Producers keep their Record schema and encoding; this package never
// inspects payload content. Its only model-proxy import is the logx
// level-filtering leaf.
package logfile

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
	dayFileLayout  = "20060102"
	rotTimeLayout  = "150405"
	fileExtension  = ".log"
	logFileMode    = 0o600
	dirMode        = 0o700
	dayCheckLayout = "2006-01-02"

	// DefaultQueueCapacity is the built-in queue depth.
	DefaultQueueCapacity = 2048
	// DefaultMaxBytes is the built-in per-file size cap.
	DefaultMaxBytes = 16 << 20 // 16 MiB
	// SweepInterval is how often the retention sweep runs.
	SweepInterval = time.Hour
)

// EncodeFunc renders one record as a JSONL line without the trailing newline.
// now is the persistence time (records may stamp persistence-time fields from
// it); buf is a scratch buffer owned by the writer goroutine that encoders may
// reuse via buf[:0]. EncodeFunc runs on the writer goroutine only.
type EncodeFunc func(now time.Time, buf []byte) ([]byte, error)

// Options configures a Logger. MaxBytes <= 0 selects DefaultMaxBytes;
// QueueCapacity <= 0 selects DefaultQueueCapacity; Retention <= 0 disables
// sweeping (files are kept forever).
type Options struct {
	Directory     string
	FilePrefix    string
	MaxBytes      int64
	Retention     time.Duration
	QueueCapacity int
	// Tag names the component in logx warnings (e.g. "seclog").
	Tag string
}

// Logger owns the non-blocking queue and its single JSONL writer goroutine.
// The application lifecycle must stop producers before calling Shutdown.
type Logger struct {
	opts        Options
	encodes     chan EncodeFunc
	done        chan struct{}
	closed      chan struct{}
	dropped     uint64
	writeErrors uint64
	stopOnce    sync.Once
	// writeLine is a test seam replacing the file writer; nil in production.
	writeLine func(line []byte, now time.Time) error
	// line is the reusable encode buffer; owned by the writer goroutine.
	line []byte
}

// New returns a logger. Run must be started exactly once before Shutdown.
func New(opts Options) *Logger {
	if opts.QueueCapacity <= 0 {
		opts.QueueCapacity = DefaultQueueCapacity
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	return &Logger{
		opts:    opts,
		encodes: make(chan EncodeFunc, opts.QueueCapacity),
		done:    make(chan struct{}),
		closed:  make(chan struct{}),
	}
}

// Enqueue offers one record (as its encoder) without blocking the request
// path; a full queue drops the record and counts it in Dropped.
func (l *Logger) Enqueue(encode EncodeFunc) {
	if l == nil || encode == nil {
		return
	}
	select {
	case l.encodes <- encode:
	default:
		n := atomic.AddUint64(&l.dropped, 1)
		if n == 1 || n%1000 == 0 {
			logx.Warnf("[%s] queue full, dropped %d records total", l.opts.Tag, n)
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

// WriteErrors returns how many records failed to persist.
func (l *Logger) WriteErrors() uint64 {
	if l == nil {
		return 0
	}
	return atomic.LoadUint64(&l.writeErrors)
}

// Directory returns the JSONL directory.
func (l *Logger) Directory() string {
	if l == nil {
		return ""
	}
	return l.opts.Directory
}

// Run drains queued records until Shutdown asks it to finish. It narrows the
// storage directory to owner-only permissions; if that fails, logging is
// disabled (records are dropped and counted).
func (l *Logger) Run() {
	if l == nil {
		return
	}
	defer close(l.closed)
	if err := EnsureDir(l.opts.Directory); err != nil {
		logx.Warnf("[%s] mkdir %s: %v - logging disabled", l.opts.Tag, l.opts.Directory, err)
		return
	}
	writer := NewWriter(l.opts.Directory, l.opts.FilePrefix, l.opts.MaxBytes)
	defer writer.Close()
	ticker := time.NewTicker(SweepInterval)
	defer ticker.Stop()
	l.sweep(time.Now(), writer.Path())
	for {
		select {
		case encode := <-l.encodes:
			l.write(writer, encode, time.Now())
		case <-ticker.C:
			l.sweep(time.Now(), writer.Path())
		case <-l.done:
			for {
				select {
				case encode := <-l.encodes:
					l.write(writer, encode, time.Now())
				default:
					l.sweep(time.Now(), writer.Path())
					return
				}
			}
		}
	}
}

func (l *Logger) write(writer *Writer, encode EncodeFunc, now time.Time) {
	var err error
	line, encErr := encode(now, l.line)
	l.line = line
	if encErr != nil {
		err = encErr
	} else if l.writeLine != nil {
		err = l.writeLine(line, now)
	} else {
		err = writer.WriteLine(line, now)
	}
	if err != nil {
		n := atomic.AddUint64(&l.writeErrors, 1)
		if n == 1 || n%1000 == 0 {
			logx.Warnf("[%s] write failed (lost %d records total): %v", l.opts.Tag, n, err)
		}
	}
}

// sweep removes rotated files older than the retention window. The active
// file is never removed. Retention <= 0 disables sweeping.
func (l *Logger) sweep(now time.Time, activePath string) {
	if l.opts.Retention <= 0 {
		return
	}
	cutoff := now.Add(-l.opts.Retention)
	entries, err := os.ReadDir(l.opts.Directory)
	if err != nil {
		logx.Warnf("[%s] sweep readdir %s: %v", l.opts.Tag, l.opts.Directory, err)
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !isLogFile(entry.Name(), l.opts.FilePrefix) {
			continue
		}
		path := filepath.Join(l.opts.Directory, entry.Name())
		if activePath != "" && path == activePath {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(path); err != nil {
				logx.Warnf("[%s] sweep remove %s: %v", l.opts.Tag, entry.Name(), err)
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

func isLogFile(name, prefix string) bool {
	return strings.HasPrefix(name, prefix) && strings.HasSuffix(name, fileExtension)
}

// EnsureDir creates dir with owner-only permissions and narrows a
// pre-existing directory to the same mode.
func EnsureDir(dir string) error {
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return err
	}
	return os.Chmod(dir, dirMode)
}

// Writer is a single-goroutine-owned rotating JSONL file sink: lazy open on
// the first line, per-day active file <prefix>YYYYMMDD.log, size rotation to
// <prefix>YYYYMMDD--HHMMSS-<seq>.log, day change without rename, and
// owner-only permissions narrowed on both new and pre-existing files.
type Writer struct {
	dir       string
	prefix    string
	maxSize   int64
	file      *os.File
	path      string
	day       string // YYYYMMDD stamp of the active file name
	dayKey    string // calendar-day key compared for day rotation
	size      int64
	rotateSeq uint64
}

// NewWriter returns a writer for dir. maxSize <= 0 selects DefaultMaxBytes.
func NewWriter(dir, prefix string, maxSize int64) *Writer {
	if maxSize <= 0 {
		maxSize = DefaultMaxBytes
	}
	return &Writer{dir: dir, prefix: prefix, maxSize: maxSize}
}

// Path returns the active file path, empty before the first open.
func (w *Writer) Path() string {
	return w.path
}

func (w *Writer) open(now time.Time) error {
	w.day = now.Format(dayFileLayout)
	w.dayKey = now.Format(dayCheckLayout)
	w.path = filepath.Join(w.dir, w.prefix+w.day+fileExtension)
	file, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, logFileMode)
	if err != nil {
		return fmt.Errorf("logfile: open %s: %w", w.path, err)
	}
	// OpenFile does not narrow permissions on a pre-existing file.
	if err := file.Chmod(logFileMode); err != nil {
		_ = file.Close()
		return fmt.Errorf("logfile: chmod %s: %w", w.path, err)
	}
	w.file = file
	if info, err := file.Stat(); err == nil {
		w.size = info.Size()
	} else {
		w.size = 0
	}
	return nil
}

// WriteLine appends one encoded line (the trailing newline is added here).
// It opens the file lazily and rotates on day change or size overflow.
func (w *Writer) WriteLine(line []byte, now time.Time) error {
	if w.file == nil {
		if err := w.open(now); err != nil {
			return err
		}
	}
	if now.Format(dayCheckLayout) != w.dayKey {
		w.rotateDay(now)
	} else if w.size > 0 && w.size+int64(len(line))+1 > w.maxSize {
		w.rotateSize(now)
	}
	if w.file == nil {
		return fmt.Errorf("logfile: no file after rotate")
	}
	n, err := w.file.Write(append(line, '\n'))
	w.size += int64(n)
	return err
}

func (w *Writer) rotateDay(now time.Time) {
	if w.file != nil {
		// A day's file that never received a line is removed, not archived.
		if w.size == 0 {
			if err := os.Remove(w.path); err != nil && !os.IsNotExist(err) {
				logx.Warnf("logfile: remove empty %s: %v", w.path, err)
			}
		}
		_ = w.file.Close()
		w.file = nil
	}
	if err := w.open(now); err != nil {
		logx.Warnf("logfile: open after day rotate: %v", err)
	}
}

func (w *Writer) rotateSize(now time.Time) {
	_ = w.file.Close()
	w.file = nil
	w.rotateSeq++
	archive := filepath.Join(w.dir, fmt.Sprintf(
		"%s%s--%s-%d%s",
		w.prefix,
		w.day,
		now.Format(rotTimeLayout),
		w.rotateSeq,
		fileExtension,
	))
	if err := os.Rename(w.path, archive); err != nil {
		// The file stays under its active name; writes continue appended.
		logx.Warnf("logfile: rename %s -> %s: %v", w.path, archive, err)
	}
	if err := w.open(now); err != nil {
		logx.Warnf("logfile: open after size rotate: %v", err)
	}
}

// Close closes the active file if open.
func (w *Writer) Close() {
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
}

// AppendLine appends one pre-encoded line (newline added) synchronously to
// the per-day <prefix>YYYYMMDD.log file in dir, for callers that run outside
// the daemon lifecycle. The directory and file are created/narrowed to
// owner-only permissions. Coexistence with a running Logger in the same
// directory is safe: both append single lines with O_APPEND.
func AppendLine(dir, prefix string, line []byte, now time.Time) error {
	if err := EnsureDir(dir); err != nil {
		return fmt.Errorf("logfile: mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, prefix+now.Format(dayFileLayout)+fileExtension)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, logFileMode)
	if err != nil {
		return fmt.Errorf("logfile: open %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	if err := file.Chmod(logFileMode); err != nil {
		return fmt.Errorf("logfile: chmod %s: %w", path, err)
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("logfile: append %s: %w", path, err)
	}
	return nil
}
