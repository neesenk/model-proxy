package requestlog

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// headWindow bounds a log file's identity fingerprint: the first bytes of its
// first line. A record line opens with its timestamp and request id, so this
// prefix identifies the file's first record — and with it the file — without
// reading a multi-MB line.
const headWindow = 256

// fileTrack is the reconcile loop's mirror of one files-table row (cursor =
// size_indexed, head) plus the stat it last processed. The table stays the
// authority (tailScan and a restarted indexer read it); the mirror only lets
// a pass skip files that did not change since the previous pass — no
// per-file query and no read for the month of rotated files a busy log
// directory carries. Owned by the reconcile loop; never touched by readers.
type fileTrack struct {
	cursor int64
	// head is the file's identity fingerprint (headOf of its first line);
	// nil while nothing is indexed, or for a row from before heads were
	// recorded until the next pass backfills it.
	head []byte
	// size/mtime are the stat this process last processed the file at;
	// observed is false until the first pass that saw it.
	size, mtime int64
	observed    bool
}

// headOf cuts a file prefix to its fingerprint: up to and including the
// first newline when it falls inside the window, else the whole window.
func headOf(prefix []byte) []byte {
	if len(prefix) > headWindow {
		prefix = prefix[:headWindow]
	}
	if i := bytes.IndexByte(prefix, '\n'); i >= 0 {
		prefix = prefix[:i+1]
	}
	return prefix
}

// readHead returns the current fingerprint of a log file.
func (x *Indexer) readHead(name string) ([]byte, error) {
	file, err := os.Open(filepath.Join(x.dir, name))
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	buf := make([]byte, headWindow)
	n, err := io.ReadFull(file, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return headOf(buf[:n]), nil
}

// loadTracks seeds the mirror from the files table (first pass, or after a
// failed pass dropped it).
func (x *Indexer) loadTracks() error {
	rows, err := x.db.Query(`SELECT path, size_indexed, head FROM files`)
	if err != nil {
		return err
	}
	defer rows.Close()
	tracks := map[string]*fileTrack{}
	for rows.Next() {
		var path string
		var cursor int64
		var head []byte
		if err := rows.Scan(&path, &cursor, &head); err != nil {
			return err
		}
		tracks[path] = &fileTrack{cursor: cursor, head: head}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	x.tracks = tracks
	return nil
}

// listLogFiles stats every request-log file in the directory.
func (x *Indexer) listLogFiles() (map[string]fileState, error) {
	entries, err := os.ReadDir(x.dir)
	if err != nil {
		return nil, fmt.Errorf("readdir: %w", err)
	}
	present := map[string]fileState{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, filePrefix) || !strings.HasSuffix(name, ".log") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		present[name] = fileState{size: info.Size(), mtime: info.ModTime().UnixNano()}
	}
	return present, nil
}

// reconcile brings the index in line with the directory:
//
//   - a file whose stat is unchanged since the previous pass is skipped;
//   - a changed file is identified by its head: same head and not shrunk
//     below its cursor = the same file grown (tailed from the cursor);
//     otherwise its indexed content left the name (replaced or truncated);
//   - a name that vanished, or whose content was replaced, is a rename
//     source: a new file carrying the source's head and at least its indexed
//     bytes IS the source moved (the writer's size rotation renames the active
//     file to its archive name), so the rows move with one UPDATE instead of
//     the archive being re-read and re-parsed from offset zero;
//   - an unclaimed vanished file drops its rows (retention sweep); an
//     unclaimed replaced file drops its rows and re-indexes from zero;
//   - every file is then tailed from its cursor. Only newline-terminated
//     lines are consumed; a trailing partial line (a record the writer has
//     not finished) waits for a later pass.
//
// A failed pass drops the mirror so the next one restarts from the table.
func (x *Indexer) reconcile() error {
	if x.tracks == nil {
		if err := x.loadTracks(); err != nil {
			return err
		}
	}
	present, err := x.listLogFiles()
	if err != nil {
		return err
	}
	if err := x.reconcileFiles(present); err != nil {
		x.tracks = nil
		return err
	}
	return nil
}

// renameSource is a tracked name whose indexed bytes no longer live under it.
type renameSource struct {
	name     string
	vanished bool // false: the name now holds different content
	claimed  bool
}

func (x *Indexer) reconcileFiles(present map[string]fileState) error {
	var sources []*renameSource
	var backfill []string
	trackedNames := make([]string, 0, len(x.tracks))
	for name := range x.tracks {
		trackedNames = append(trackedNames, name)
	}
	sort.Strings(trackedNames)
	for _, name := range trackedNames {
		track := x.tracks[name]
		state, ok := present[name]
		if !ok {
			sources = append(sources, &renameSource{name: name, vanished: true})
			continue
		}
		if track.observed && state.size == track.size && state.mtime == track.mtime {
			continue
		}
		if track.cursor == 0 {
			continue // nothing indexed yet: nothing to verify
		}
		head, err := x.readHead(name)
		if errors.Is(err, fs.ErrNotExist) {
			// Vanished between the listing and the open: the next pass
			// sees it as vanished.
			delete(present, name)
			continue
		}
		if err != nil {
			return fmt.Errorf("head %s: %w", name, err)
		}
		switch {
		case track.head == nil && state.size >= track.cursor:
			// A row indexed before heads were recorded: identity is only
			// checkable by size (the pre-head rule), and the content is
			// taken as the indexed file — record its head from now on.
			track.head = head
			backfill = append(backfill, name)
		case track.head == nil || !bytes.Equal(head, track.head) || state.size < track.cursor:
			sources = append(sources, &renameSource{name: name})
		}
	}
	if err := x.persistHeads(backfill); err != nil {
		return err
	}

	if len(sources) > 0 {
		var fresh []string
		for name, state := range present {
			if _, tracked := x.tracks[name]; !tracked && state.size > 0 {
				fresh = append(fresh, name)
			}
		}
		sort.Strings(fresh)
		for _, name := range fresh {
			head, err := x.readHead(name)
			if errors.Is(err, fs.ErrNotExist) {
				delete(present, name)
				continue
			}
			if err != nil {
				return fmt.Errorf("head %s: %w", name, err)
			}
			for _, source := range sources {
				track := x.tracks[source.name]
				if source.claimed || track.head == nil || !bytes.Equal(head, track.head) || present[name].size < track.cursor {
					continue
				}
				if err := x.moveFileRows(source, name, present[name].mtime); err != nil {
					return err
				}
				source.claimed = true
				break
			}
		}
		for _, source := range sources {
			if source.claimed {
				continue
			}
			if err := x.dropFileRows(source); err != nil {
				return err
			}
		}
	}

	names := make([]string, 0, len(present))
	for name := range present {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		state := present[name]
		track := x.tracks[name]
		if track == nil {
			track = &fileTrack{}
			x.tracks[name] = track
		}
		if !(track.observed && state.size == track.size && state.mtime == track.mtime) && state.size > track.cursor {
			if err := x.appendFile(name, track, state.mtime); err != nil {
				return fmt.Errorf("index %s: %w", name, err)
			}
		}
		track.size, track.mtime, track.observed = state.size, state.mtime, true
	}
	return nil
}

// persistHeads records backfilled heads in one transaction.
func (x *Indexer) persistHeads(names []string) error {
	if len(names) == 0 {
		return nil
	}
	tx, err := x.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, name := range names {
		if _, err := tx.Exec(`UPDATE files SET head = ? WHERE path = ?`, x.tracks[name].head, name); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// moveFileRows carries a rename source's rows and cursor over to the name its
// bytes now live under. A replaced source keeps a fresh zero cursor for its
// new content; a vanished one is forgotten. Session aggregates are unaffected
// (the rows and their contents are unchanged).
func (x *Indexer) moveFileRows(source *renameSource, target string, targetMtime int64) error {
	track := x.tracks[source.name]
	tx, err := x.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE records SET file = ? WHERE file = ?`, target, source.name); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM files WHERE path = ?`, source.name); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO files (path, size_indexed, mtime, head) VALUES (?, ?, ?, ?)
		 ON CONFLICT(path) DO UPDATE SET size_indexed = excluded.size_indexed, mtime = excluded.mtime, head = excluded.head`,
		target, track.cursor, targetMtime, track.head,
	); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	x.tracks[target] = &fileTrack{cursor: track.cursor, head: track.head}
	if source.vanished {
		delete(x.tracks, source.name)
	} else {
		x.tracks[source.name] = &fileTrack{}
	}
	return nil
}

// dropFileRows removes an unclaimed source's rows: a vanished file is
// forgotten (retention sweep), a replaced one restarts from cursor zero.
func (x *Indexer) dropFileRows(source *renameSource) error {
	tx, err := x.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	droppedSessions, err := deleteFileRows(tx, source.name)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM files WHERE path = ?`, source.name); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// The re-index re-inserts a replaced file's rows from zero and folds them
	// back in as they arrive; dropping first is what keeps the fold from
	// double-counting the pre-replacement rows.
	x.sessions.dropSessions(droppedSessions)
	if source.vanished {
		delete(x.tracks, source.name)
	} else {
		x.tracks[source.name] = &fileTrack{}
	}
	return nil
}
