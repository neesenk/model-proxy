package logfile

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"model-proxy/internal/observe/logx"
)

// zstdSuffix marks archived rotated files: <prefix>*.log.zst. Every query
// path matches the exact .log suffix, so archives are invisible to the
// indexer, the scan fallback and the queries by construction — offline-only
// data (zstd -d | jq), never an online read path.
const zstdSuffix = ".zst"

// archiveQueueDepth bounds how many cold files one sweep tick may hand to
// the worker; a fuller queue retries on the next sweep.
const archiveQueueDepth = 16

func isZstdFile(name, prefix string) bool {
	return strings.HasPrefix(name, prefix) && strings.HasSuffix(name, zstdSuffix)
}

// archiveDay extracts the day stamp (YYYYMMDD) a managed file's name
// carries. A .zst's mtime is compression time, not content age, so every
// age check on archives reads the name instead.
func archiveDay(name, prefix string) (time.Time, bool) {
	if len(name) < len(prefix) {
		return time.Time{}, false
	}
	rest := name[len(prefix):]
	if len(rest) < 8 {
		return time.Time{}, false
	}
	day, err := time.Parse(dayFileLayout, rest[:8])
	if err != nil {
		return time.Time{}, false
	}
	return day, true
}

// archiveZstd compresses src (a rotated .log) to src+".zst" atomically:
// write src+".zst.tmp" → rename to ".zst" → remove src. It is idempotent
// across crashes: when the ".zst" already exists (a crash between rename
// and remove), only src is removed; a leftover ".tmp" is replaced
// (O_TRUNC) by the next run.
func archiveZstd(src string) error {
	dst := src + zstdSuffix
	if _, statErr := os.Stat(dst); statErr == nil {
		return os.Remove(src)
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	tmp := dst + ".tmp"
	if err := compressZstd(src, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return err
	}
	return os.Remove(src)
}

// compressZstd writes src's content zstd-compressed to tmp and returns only
// once both file handles are closed, on every path. That is what lets the
// caller rename tmp and remove src afterwards: Go opens files on Windows
// without FILE_SHARE_DELETE, so a read handle still open on src would make
// that remove fail.
func compressZstd(src, tmp string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	// The success path closes in explicitly below; the deferred close covers
	// every error path in between.
	defer in.Close()
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, logFileMode)
	if err != nil {
		return err
	}
	enc, err := zstd.NewWriter(out)
	if err != nil {
		_ = out.Close()
		return err
	}
	if _, err := io.Copy(enc, in); err != nil {
		_ = enc.Close()
		_ = out.Close()
		return err
	}
	// Last read done: src is released from here on, whatever happens next.
	_ = in.Close()
	if err := enc.Close(); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// dispatchArchive offers rotated .log files whose mtime fell out of the hot
// window to the archive worker — non-blocking; a full queue retries on the
// next sweep. It runs on the writer goroutine AFTER the retention sweep, so
// files already past total retention are deleted rather than compressed.
// The active file and non-.log entries are never offered.
func (l *Logger) dispatchArchive(now time.Time, activePath string) {
	if l.opts.HotRetention <= 0 {
		return
	}
	cutoff := now.Add(-l.opts.HotRetention)
	entries, err := os.ReadDir(l.opts.Directory)
	if err != nil {
		logx.Warnf("[%s] archive readdir %s: %v", l.opts.Tag, l.opts.Directory, err)
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !isLogFile(name, l.opts.FilePrefix) {
			continue
		}
		path := filepath.Join(l.opts.Directory, name)
		if activePath != "" && path == activePath {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		select {
		case l.archive <- path:
		default:
			logx.Warnf("[%s] archive queue full; %s retried next sweep", l.opts.Tag, name)
			return
		}
	}
}

// archiveWorker drains the archive queue off the writer goroutine —
// compressing a rotated file takes seconds and must never stall record
// writes. Each finished archive fires OnChange so the tailing index sees
// the vanished plain file on its next pass.
//
// Stop is the final flush and nothing more: closing archiveStop ends the
// loop, finishing at most the offer already in flight. The offers still
// queued are abandoned, not drained — their files stay plain on disk and the
// next run's sweep re-offers them — so a shutdown never waits on a backlog.
func (l *Logger) archiveWorker() {
	defer close(l.archiveDone)
	for {
		select {
		case <-l.archiveStop:
			return
		case src, ok := <-l.archive:
			if !ok {
				return // a closed queue (how tests end the worker) ends the loop
			}
			// The select above may pick a queued offer although stop is
			// already closed: that one must not be started.
			select {
			case <-l.archiveStop:
				return
			default:
			}
			if err := l.archiveFn(src); err != nil {
				logx.Warnf("[%s] archive %s failed (kept plain; retried next sweep): %v",
					l.opts.Tag, filepath.Base(src), err)
				continue
			}
			l.changed()
		}
	}
}

// HotRetention returns the configured hot window (0 = archiving disabled).
func (l *Logger) HotRetention() time.Duration {
	if l == nil {
		return 0
	}
	return l.opts.HotRetention
}
