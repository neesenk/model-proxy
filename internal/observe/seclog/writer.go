package seclog

import (
	"encoding/json"
	"fmt"
	"model-proxy/internal/observe/logx"
	"os"
	"path/filepath"
	"time"
)

const (
	filePrefix     = "security-"
	fileSuffix     = ".log"
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
}

func (w *fileWriter) open(now time.Time) {
	w.startedAt = now
	w.day = now.Format("2006-01-02")
	w.path = filepath.Join(w.dir, fmt.Sprintf("%s%s%s", filePrefix, now.Format(fileTimeLayout), fileSuffix))
	file, err := os.OpenFile(w.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, logFileMode)
	if err != nil {
		logx.Warnf("[seclog] open %s: %v", w.path, err)
		w.file = nil
		return
	}
	// OpenFile does not narrow permissions on an existing same-second file.
	if err := file.Chmod(logFileMode); err != nil {
		logx.Warnf("[seclog] chmod %s: %v", w.path, err)
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
				"%s%s--%s-%d%s",
				filePrefix,
				w.startedAt.Format(fileTimeLayout),
				now.Format(fileTimeLayout),
				w.rotateSeq,
				fileSuffix,
			))
			if err := os.Rename(w.path, archive); err != nil {
				logx.Warnf("[seclog] rename %s -> %s: %v", w.path, archive, err)
			}
		} else if err := os.Remove(w.path); err != nil && !os.IsNotExist(err) {
			logx.Warnf("[seclog] remove empty %s: %v", w.path, err)
		}
	}
	w.open(now)
}

func (w *fileWriter) write(rec *Record, now time.Time) error {
	if w.file == nil {
		w.open(now)
		if w.file == nil {
			return fmt.Errorf("seclog: no open file")
		}
	}
	if rec.Ts == 0 {
		rec.Ts = now.UnixMilli()
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("seclog: encode record: %w", err)
	}
	line = append(line, '\n')
	dayChanged := now.Format("2006-01-02") != w.day
	if (w.size > 0 && w.size+int64(len(line)) > w.maxSize) || dayChanged {
		w.rotate(now)
		if w.file == nil {
			return fmt.Errorf("seclog: no file after rotate")
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
