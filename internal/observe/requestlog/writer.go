package requestlog

import (
	"fmt"
	"model-proxy/internal/observe/logx"
	"os"
	"path/filepath"
	"time"
)

const (
	fileTimeLayout = "20060102-150405"
	logFileMode    = 0o600
)

// fileWriter is single-goroutine-owned and implements size/day rotation.
type fileWriter struct {
	dir       string
	maxSize   int64
	file      *os.File
	path      string
	startedAt time.Time
	day       string
	size      int64
	rotateSeq uint64
	// line is the reusable encode buffer for one JSONL row; being
	// single-goroutine-owned it needs no pool.
	line []byte
}

func (w *fileWriter) open(now time.Time) {
	w.startedAt = now
	w.day = now.Format("2006-01-02")
	w.path = filepath.Join(w.dir, fmt.Sprintf("requests-%s.log", now.Format(fileTimeLayout)))
	file, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, logFileMode)
	if err != nil {
		logx.Warnf("[request_log] open %s: %v", w.path, err)
		w.file = nil
		return
	}
	// OpenFile does not narrow permissions on an existing same-second file.
	if err := file.Chmod(logFileMode); err != nil {
		logx.Warnf("[request_log] chmod %s: %v", w.path, err)
		_ = file.Close()
		w.file = nil
		return
	}
	w.file = file
	if info, err := file.Stat(); err == nil {
		w.size = info.Size()
	} else {
		w.size = 0
	}
}

func (w *fileWriter) rotate(now time.Time) {
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
		if w.size > 0 {
			w.rotateSeq++
			archive := filepath.Join(w.dir, fmt.Sprintf(
				"requests-%s--%s-%d.log",
				w.startedAt.Format(fileTimeLayout),
				now.Format(fileTimeLayout),
				w.rotateSeq,
			))
			if err := os.Rename(w.path, archive); err != nil {
				logx.Warnf("[request_log] rename %s -> %s: %v", w.path, archive, err)
			}
		} else if err := os.Remove(w.path); err != nil && !os.IsNotExist(err) {
			logx.Warnf("[request_log] remove empty %s: %v", w.path, err)
		}
	}
	w.open(now)
}

func (w *fileWriter) write(rec *Record, now time.Time) error {
	if w.file == nil {
		w.open(now)
		if w.file == nil {
			return fmt.Errorf("request_log: no open file")
		}
	}
	// Encode onto the retained line buffer: after the first large record its
	// capacity is steady-state, so encoding adds no per-record allocation.
	w.line = appendRecordLine(w.line[:0], rec)
	line := w.line
	dayChanged := now.Format("2006-01-02") != w.day
	if (w.size > 0 && w.size+int64(len(line)) > w.maxSize) || dayChanged {
		w.rotate(now)
		if w.file == nil {
			return fmt.Errorf("request_log: no file after rotate")
		}
	}
	n, err := w.file.Write(line)
	w.size += int64(n)
	return err
}

func (w *fileWriter) close() {
	if w.file != nil {
		_ = w.file.Close()
		w.file = nil
	}
}
