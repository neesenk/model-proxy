package logfile

import (
	"errors"
	"fmt"
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

// archiveQueueDepth bounds how many cold files may wait for the archive
// worker at once. A dispatch pass hands over one queue's worth and parks the
// rest for the refills the worker's progress triggers (see
// offerPendingArchives).
const archiveQueueDepth = 16

// archiveFailBudget is how many archives the worker may fail in a row before
// the writer goroutine stops handing over parked candidates, until a success
// or the next sweep. Every failure frees a slot and so asks for a refill: on
// a full disk, with nothing left for the guard to delete, an unthrottled
// refill would try the whole backlog once per sweep, a warning each.
const archiveFailBudget = 3

func isZstdFile(name, prefix string) bool {
	return strings.HasPrefix(name, prefix) && strings.HasSuffix(name, zstdSuffix)
}

// isZstdTmpFile matches this stream's archive still being written — the
// orphan reap of the dispatch pass and the disk guard's accounting read it.
func isZstdTmpFile(name, prefix string) bool {
	return strings.HasPrefix(name, prefix) && strings.HasSuffix(name, zstdTmpSuffix)
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
	return publishArchive(src, tmp, dst)
}

// errSourceRemovedMidArchive marks the error publishArchive returns when the
// plain file was deleted while its archive was being written — by the
// retention sweep or the disk guard, which run on the writer goroutine. It
// still reads as not-exist; the worker tells it apart from a duplicate offer
// (the same not-exist, nothing produced) to report the archive that outlived
// the deletion.
var errSourceRemovedMidArchive = errors.New("plain file removed while its archive was being written")

// publishArchive finishes an archive whose tmp is fully written: rename it
// to dst, then remove src. When src is already gone the archive just
// published stands — nothing takes it back (a source that was expired
// carries a day stamp the next sweep collects it by) — and the error says so.
func publishArchive(src, tmp, dst string) error {
	if err := os.Rename(tmp, dst); err != nil {
		// Same orphan risk as a failed compression; same best-effort takeback.
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Remove(src); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: %w", errSourceRemovedMidArchive, err)
		}
		return err
	}
	return nil
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
// window to the archive worker — non-blocking. It runs on the writer
// goroutine AFTER the retention sweep, so files already past total retention
// are deleted rather than compressed. The active file and non-.log entries
// are never offered.
//
// The tmp sweep is garbage collection, not archiving: it runs with or without
// a hot window. A tmp left by a crash while archiving was on has no other
// owner once archiving is off — sweep, disk guard and prune all pass .zst.tmp
// over — so this pass is its only way off the disk.
//
// This pass is the only time the directory is scanned for candidates. What
// the queue has no room for is parked in archivePending and counted in one
// warning; the worker's refill signal hands the parked files over as slots
// free up, so a backlog drains without waiting for the next sweep, and
// nothing rescans the directory in between.
//
// A sweep that finds the previous one's backlog still in progress counts
// files it handed over before: offers still queued, and the one in flight,
// are plain until their archive completes, so the scan lists them again —
// offered where a slot is free, parked otherwise. The parked list, and the
// figure in the warning, therefore include up to one queue's worth plus one
// file that were already handed over; the worker skips each duplicate once
// the first offer is done, or gives a failed one its retry.
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
	// Only a successful scan replaces the parked list: when the read above
	// failed, what was waiting still is.
	l.archivePending = nil
	// One sweep hands the worker one queue's worth directly, however many
	// slots it frees while this scan runs (a failing worker frees them fast).
	// What a sweep costs while archives keep failing then does not depend on
	// how long the scan takes; refills after it carry the failure budget.
	offered := 0
	for _, entry := range entries {
		name := entry.Name()
		// Reap leftover tmp archives — garbage from a dead run (crash
		// mid-write or mid-rename). While archiving is on, a tmp with its
		// source present belongs to a live or retryable compression (the
		// next attempt O_TRUNCs it) and stays; with archiving off no worker
		// exists, so every tmp of this stream is a leftover.
		if isZstdTmpFile(name, l.opts.FilePrefix) {
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
		if offered < archiveQueueDepth && l.offerArchive(path) {
			offered++
			continue
		}
		l.archivePending = append(l.archivePending, path)
	}
	if len(l.archivePending) > 0 {
		logx.Warnf("[%s] archive queue full; %d more file(s) waiting, %s next",
			l.opts.Tag, len(l.archivePending), filepath.Base(l.archivePending[0]))
	}
}

// offerArchive hands path to the archive worker without blocking. It reports
// whether the queue took it; a full queue does not.
func (l *Logger) offerArchive(path string) bool {
	select {
	case l.archive <- path:
		return true
	default:
		return false
	}
}

// offerPendingArchives hands the worker the parked candidates, in scan order,
// until the queue is full again or nothing is left. It is what the worker's
// refill signal triggers on the writer goroutine, and it reads no directory:
// with nothing parked it is a no-op, and a file the worker failed — plain and
// cold as ever — is not offered again before the next dispatch pass.
//
// The worker's failure streak throttles it (see archiveFailBudget). From
// archiveFailBudget failures in a row on, nothing is handed over until a
// success or the next sweep. Below that, a nonzero streak hands over one
// candidate per call and waits for its outcome: the queue's own offers are
// still worked through, so a sweep tries at most one queue's worth plus one
// more per failure below the budget.
func (l *Logger) offerPendingArchives() {
	for len(l.archivePending) > 0 {
		streak := l.archiveFailStreak.Load()
		if streak >= archiveFailBudget {
			return
		}
		select {
		case l.archive <- l.archivePending[0]:
			l.archivePending = l.archivePending[1:]
		default:
			return
		}
		if streak > 0 {
			return
		}
	}
}

// signalArchiveRefill tells Run that the worker freed a queue slot. The
// signal coalesces (many freed slots are one wake-up) and never blocks: the
// worker must not wait on the writer goroutine — not behind a stalled record
// write, and not at shutdown, when nobody reads the signal any more.
func (l *Logger) signalArchiveRefill() {
	select {
	case l.archiveRefill <- struct{}{}:
	default:
	}
}

// archiveWorker drains the archive queue off the writer goroutine —
// compressing a rotated file takes seconds and must never stall record
// writes. Each finished archive fires OnChange so the tailing index sees
// the vanished plain file on its next pass, and every processed offer frees
// its slot back to Run (see archiveOffered), which tops the queue up from
// what the last dispatch pass parked.
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
			l.archiveOffered(src)
		}
	}
}

// archiveOffered compresses one file the queue handed over and, whatever came
// of it — archived, failed, or already gone — reports the freed slot to Run.
func (l *Logger) archiveOffered(src string) {
	defer l.signalArchiveRefill()
	if err := l.archiveFn(src); err != nil {
		if errors.Is(err, errSourceRemovedMidArchive) {
			// Published, but a deletion raced the compression and took the
			// plain file. The archive stands where the file was: said once
			// here, where the stream's tag is, so the revived .zst leaves
			// a trace without reading as a failure.
			logx.Infof("[%s] archive %s: the plain file was removed while it was being compressed "+
				"(retention sweep or disk guard); the archive stays until a sweep or guard pass collects it",
				l.opts.Tag, filepath.Base(src)+zstdSuffix)
			l.archiveFailStreak.Store(0) // the archive was written: the disk took it
			return
		}
		if os.IsNotExist(err) {
			// The source vanished under us — a duplicate offer
			// after a finished archive removed it, or the
			// retention sweep / disk guard deleted it
			// mid-compression. Expected, not a failure, and
			// no attempt on the disk: it leaves the streak alone.
			return
		}
		// Counted before the freed slot is reported, so the refill that
		// follows always sees this failure.
		l.archiveFailStreak.Add(1)
		logx.Warnf("[%s] archive %s failed (kept plain; retried next sweep): %v",
			l.opts.Tag, filepath.Base(src), err)
		return
	}
	l.archiveFailStreak.Store(0)
	l.changed()
}

// HotRetention returns the configured hot window (0 = archiving disabled).
func (l *Logger) HotRetention() time.Duration {
	if l == nil {
		return 0
	}
	return l.opts.HotRetention
}
