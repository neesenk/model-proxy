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

// zstdTmpSuffix marks an archive still being written. Nothing lists a tmp as
// deletable, so the dispatch pass reaps those whose source is already gone.
const zstdTmpSuffix = ".zst.tmp"

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
// (O_TRUNC) by the next run. A failed compression or rename takes its
// ".tmp" back; a crashed run's tmp whose source later vanished is reaped
// by the dispatch pass.
func archiveZstd(src string) error {
	dst := src + zstdSuffix
	if _, statErr := os.Stat(dst); statErr == nil {
		return os.Remove(src)
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	tmp := dst + ".tmp"
	if err := compressZstd(src, tmp); err != nil {
		// Take the unfinished tmp back. Nothing lists a ".zst.tmp" as
		// deletable — sweep, disk guard and Prune all pass over it as an
		// archive still being written — so once its source is gone (a full
		// disk fails the compression, then has the guard delete the plain
		// file) the partial blob would be orphaned for good. Best effort:
		// the compression error is the one to report.
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		// Same orphan risk as a failed compression; same best-effort takeback.
		_ = os.Remove(tmp)
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
//
// The tmp sweep is garbage collection, not archiving: it runs with or without
// a hot window. A tmp left by a crash while archiving was on has no other
// owner once archiving is off — sweep, disk guard and prune all pass .zst.tmp
// over — so this pass is its only way off the disk.
func (l *Logger) dispatchArchive(now time.Time, activePath string) {
	archiving := l.opts.HotRetention > 0
	entries, err := os.ReadDir(l.opts.Directory)
	if err != nil {
		logx.Warnf("[%s] archive readdir %s: %v", l.opts.Tag, l.opts.Directory, err)
		return
	}
	var cutoff time.Time
	if archiving {
		cutoff = now.Add(-l.opts.HotRetention)
	}
	for _, entry := range entries {
		name := entry.Name()
		// Reap leftover tmp archives — garbage from a dead run (crash
		// mid-write or mid-rename). While archiving is on, a tmp with its
		// source present belongs to a live or retryable compression (the
		// next attempt O_TRUNCs it) and stays; with archiving off no worker
		// exists, so every tmp of this stream is a leftover.
		if strings.HasPrefix(name, l.opts.FilePrefix) && strings.HasSuffix(name, zstdTmpSuffix) {
			if archiving {
				srcName := strings.TrimSuffix(strings.TrimSuffix(name, ".tmp"), zstdSuffix)
				if _, statErr := os.Stat(filepath.Join(l.opts.Directory, srcName)); !os.IsNotExist(statErr) {
					continue
				}
			}
			_ = os.Remove(filepath.Join(l.opts.Directory, name))
			continue
		}
		if !archiving {
			continue
		}
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
				if os.IsNotExist(err) {
					// The source vanished under us — a duplicate offer
					// after a finished archive removed it, or the
					// retention sweep / disk guard deleted it
					// mid-compression. Expected, not a failure.
					continue
				}
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
