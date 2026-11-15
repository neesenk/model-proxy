// logrotate.go owns the serve-log size rotator. The serve log is written by
// the supervisor and the worker through ONE fd inherited across exec (plus a
// second O_APPEND open in foreground mirror mode), so rotation must keep the
// file's inode: a rename-based rotation would strand every writer's appended
// bytes on the archived inode. Copy+truncate keeps the inode — the snapshot
// lands in a stem-stamped archive, os.Truncate rewinds the shared inode, and
// O_APPEND writers resume at the new end. Lines written between the copy and
// the truncate are lost (accepted copytruncate semantics for the human-facing
// run log; the request log keeps its own lossless rotation).
package serve

import (
	"fmt"
	"io"
	"model-proxy/internal/observe/logx"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultLogRotateInterval is how often the rotator checks the log's size.
	DefaultLogRotateInterval = time.Minute

	archiveStampLayout = "20060102-150405"
	archiveExtension   = ".log"
)

// RunLogRotatorLoop is the serve log's TransportTask: every interval it
// checks the log's size and, at or over maxBytes, rotates it (copy+truncate)
// keeping the newest `keep` archives. interval <= 0 selects
// DefaultLogRotateInterval. Returns when stop closes; callers treat
// maxBytes <= 0 as "do not start the loop at all".
func RunLogRotatorLoop(stop <-chan struct{}, path string, maxBytes int64, keep int, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultLogRotateInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			info, err := os.Stat(path)
			if err != nil {
				if !os.IsNotExist(err) {
					logx.Warnf("[logrotate] stat %s: %v", path, err)
				}
				continue
			}
			if info.Size() < maxBytes {
				continue
			}
			archive, err := performRotation(path, keep, time.Now())
			if err != nil {
				logx.Warnf("[logrotate] rotate %s: %v", path, err)
				continue
			}
			logx.Infof("[logrotate] %s %s -> %s (kept %d)",
				filepath.Base(path), humanMiB(info.Size()), archive, keep)
		}
	}
}

// performRotation snapshots path into a sibling archive, truncates the
// original to zero, and prunes archives beyond keep (oldest first). The
// archive name is <stem>-<YYYYMMDD-HHMMSS>.log next to the log; a name
// collision (two rotations inside one second) appends -2, -3, ….
func performRotation(path string, keep int, now time.Time) (string, error) {
	src, err := os.Open(path)
	if err != nil {
		return "", err
	}
	archive, err := copyToArchive(src, path, now)
	closeErr := src.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	if err := os.Truncate(path, 0); err != nil {
		return "", fmt.Errorf("truncate %s: %w", path, err)
	}
	if _, err := pruneArchives(path, keep); err != nil {
		// The rotation itself succeeded; a failing prune must not read as a
		// failed rotation (the loop would warn and retry the whole thing).
		logx.Warnf("[logrotate] prune archives of %s: %v", path, err)
	}
	return archive, nil
}

// copyToArchive streams src's current content into a fresh archive next to
// path and returns the archive's path. O_EXCL keeps a same-stamp archive from
// being overwritten; the -N probe walks collisions.
func copyToArchive(src io.Reader, path string, now time.Time) (string, error) {
	dir := filepath.Dir(path)
	stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(filepath.Base(path)))
	stamped := stem + "-" + now.Format(archiveStampLayout)
	var archive string
	for seq := 1; ; seq++ {
		name := stamped + archiveExtension
		if seq > 1 {
			name = stamped + "-" + strconv.Itoa(seq) + archiveExtension
		}
		candidate := filepath.Join(dir, name)
		dst, err := os.OpenFile(candidate, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		archive = candidate
		_, copyErr := io.Copy(dst, src)
		closeErr := dst.Close()
		if copyErr != nil {
			return "", copyErr
		}
		if closeErr != nil {
			return "", closeErr
		}
		return archive, nil
	}
}

// pruneArchives deletes the log's stem-*.log archives beyond keep, oldest
// first by name (the stamped names sort chronologically), and returns what
// it removed. The active log itself never matches the archive pattern.
func pruneArchives(path string, keep int) ([]string, error) {
	if keep < 0 {
		keep = 0
	}
	dir := filepath.Dir(path)
	base := filepath.Base(path)
	stem := strings.TrimSuffix(base, filepath.Ext(base))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var archives []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || name == base {
			continue
		}
		if strings.HasPrefix(name, stem+"-") && strings.HasSuffix(name, archiveExtension) {
			archives = append(archives, name)
		}
	}
	if len(archives) <= keep {
		return nil, nil
	}
	// Ascending name order = oldest first (the stamps sort chronologically).
	slices.Sort(archives)
	excess := archives[:len(archives)-keep]
	for _, name := range excess {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			return nil, err
		}
	}
	return excess, nil
}

// humanMiB renders a byte count in whole MiB for rotation log lines.
func humanMiB(n int64) string {
	return strconv.FormatInt(n>>20, 10) + "MiB"
}
