package requestlog

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"model-proxy/internal/observe/logfile"
	"model-proxy/internal/observe/logx"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver; registers as "sqlite" with database/sql
)

const (
	// indexFileName is the SQLite derived index inside the request-log directory.
	indexFileName = "index.db"
	// indexBatchLines bounds one insert transaction so the single SQLite
	// connection stays responsive to web queries during a large catch-up.
	indexBatchLines = 500
	// indexBatchMaxBytes bounds one insert batch by the JSONL line bytes it
	// retained: a burst catch-up of large-body records must not hold a whole
	// 500-line batch of multi-MB lines (with bodies blanked post-parse the
	// retention is metadata-only, this cap is the belt-and-braces bound on
	// any future field growth).
	indexBatchMaxBytes = 64 << 20
	// defaultReconcileInterval is the tailing tick: how often the indexer
	// picks up appended lines, rotations, truncations and retention deletes.
	defaultReconcileInterval = 250 * time.Millisecond
	// indexDetailLimit bounds the indexed locations one Detail id may seek
	// (mirrors the scan-side detail filter's Limit 50).
	indexDetailLimit = 50
)

// Indexer tails the request-log JSONL files into a SQLite derived index so the
// web read path stops full-scanning the logs on every query. The index holds
// per-record metadata, the ExtractUsage projection (parsed once at index time)
// and the (file, offset, length) coordinates of each raw line; bodies stay in
// the JSONL files and are read back by a single seek for detail queries.
//
// The index is a derived view, never a source of truth: a vanished file drops
// its rows, a shrunk file (truncation/rotation replace) is re-indexed from
// zero, and a missing or unopenable index.db is rebuilt empty and re-tailed
// from scratch. Reconcile runs on a ticker; the hot write path (forward commit
// → Logger.Enqueue) never touches SQLite.
//
// Lifecycle mirrors Logger: the application starts Run exactly once and calls
// Shutdown after the log writer has drained — Shutdown's final reconcile pass
// is the final flush that catches the drained tail.
type Indexer struct {
	dir  string
	db   *sql.DB
	tick time.Duration
	// batchMaxBytes bounds one insert batch by retained line bytes
	// (indexBatchMaxBytes; a test seam for byte-cap coverage).
	batchMaxBytes int64

	done     chan struct{}
	closed   chan struct{}
	stopOnce sync.Once

	reconcileErrors atomic.Uint64
	// reconciled is signaled (non-blocking) after every finished reconcile
	// pass — a test seam for observing the background loop without sleeps.
	reconciled chan struct{}
	// sessions is the incremental per-session aggregate cache behind
	// SessionSummaries (see sessionAggCache).
	sessions sessionAggCache
}

// NewIndexer opens (or creates) the index at <dir>/index.db and applies the
// schema. A directory missing a readable index (deleted, or a corrupted file
// SQLite cannot open) self-heals: the file is removed and the index rebuilt
// empty, after which reconcile re-tails every log file from offset zero.
func NewIndexer(dir string) (*Indexer, error) {
	if err := logfile.EnsureDir(dir); err != nil {
		return nil, fmt.Errorf("request log index dir: %w", err)
	}
	path := filepath.Join(dir, indexFileName)
	db, err := openIndexDB(path)
	if err != nil {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			_ = os.Remove(path + suffix)
		}
		if db, err = openIndexDB(path); err != nil {
			return nil, err
		}
	}
	// Owner-only like the JSONL files (logfile does not cover files it does
	// not create).
	_ = os.Chmod(path, 0o600)
	indexer := &Indexer{
		dir:           dir,
		db:            db,
		tick:          defaultReconcileInterval,
		batchMaxBytes: indexBatchMaxBytes,
		done:          make(chan struct{}),
		closed:        make(chan struct{}),
		reconciled:    make(chan struct{}, 1),
	}
	if err := indexer.migrate(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate request log index: %w", err)
	}
	return indexer, nil
}

// openIndexDB mirrors internal/observe/stats store.go: one connection (the
// single periodic writer plus web readers serialize on it), WAL so readers do
// not block behind the reconcile writer, busy_timeout absorbing the short
// per-batch write transactions.
func openIndexDB(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)",
		path,
		250,
	)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open request log index: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping request log index: %w", err)
	}
	return db, nil
}

// migrate is additive-only: CREATE TABLE/INDEX IF NOT EXISTS. Future columns
// are added with ALTER TABLE ... ADD COLUMN guarded by PRAGMA table_info (the
// stats ensureColumns pattern); never change a column's meaning in place.
func (x *Indexer) migrate() error {
	_, err := x.db.Exec(`CREATE TABLE IF NOT EXISTS records (
		request_id     TEXT NOT NULL,
		ts             TEXT NOT NULL,
		ts_ms          INTEGER NOT NULL,
		session_id     TEXT NOT NULL,
		agent          TEXT NOT NULL,
		protocol       TEXT NOT NULL,
		method         TEXT NOT NULL,
		path           TEXT NOT NULL,
		called_model   TEXT NOT NULL,
		upstream_model TEXT NOT NULL,
		exposed        TEXT NOT NULL,
		provider       TEXT NOT NULL,
		attempt        INTEGER NOT NULL,
		status         INTEGER NOT NULL,
		latency_ms     INTEGER NOT NULL,
		request_size   INTEGER NOT NULL,
		response_size  INTEGER NOT NULL,
		turn_key       TEXT NOT NULL DEFAULT '',
		routing        TEXT,
		schedule       TEXT,
		shadow         INTEGER NOT NULL,
		attempt_only   INTEGER NOT NULL DEFAULT 0,
		input          INTEGER NOT NULL,
		output         INTEGER NOT NULL,
		cache_read     INTEGER NOT NULL,
		cache_creation INTEGER NOT NULL,
		file           TEXT NOT NULL,
		"offset"       INTEGER NOT NULL,
		length         INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_records_ts ON records(ts);
	CREATE INDEX IF NOT EXISTS idx_records_session ON records(session_id);
	CREATE INDEX IF NOT EXISTS idx_records_request ON records(request_id);

	CREATE TABLE IF NOT EXISTS files (
		path         TEXT PRIMARY KEY,
		size_indexed INTEGER NOT NULL,
		mtime        INTEGER NOT NULL
	);`)
	if err != nil {
		return err
	}
	return x.ensureColumns()
}

// ensureColumns adds late index columns additively (PRAGMA table_info
// guarded ALTER — the stats ensureColumns pattern; never change a column's
// meaning in place). ttft_ms joined after latency_ms existed; rows written
// before it keep 0 = unknown. Same for turn_key, kind and tool (the MCP
// tools/call name): pre-existing indexed rows keep the empty default until
// the index is rebuilt from the JSONL files. routing stores the JSONL
// routing decision object and schedule the scheduling-source object
// ({"source":...}); pre-existing rows keep NULL.
func (x *Indexer) ensureColumns() error {
	rows, err := x.db.Query(`PRAGMA table_info(records)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	hasTTFT := false
	hasTurnKey := false
	hasKind := false
	hasTool := false
	hasRouting := false
	hasAPIKey := false
	hasSchedule := false
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		switch name {
		case "ttft_ms":
			hasTTFT = true
		case "turn_key":
			hasTurnKey = true
		case "kind":
			hasKind = true
		case "tool":
			hasTool = true
		case "routing":
			hasRouting = true
		case "schedule":
			hasSchedule = true
		case "api_key":
			hasAPIKey = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !hasTTFT {
		if _, err := x.db.Exec(`ALTER TABLE records ADD COLUMN ttft_ms INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	if !hasTurnKey {
		if _, err := x.db.Exec(`ALTER TABLE records ADD COLUMN turn_key TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if !hasKind {
		if _, err := x.db.Exec(`ALTER TABLE records ADD COLUMN kind TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if !hasTool {
		if _, err := x.db.Exec(`ALTER TABLE records ADD COLUMN tool TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if !hasRouting {
		if _, err := x.db.Exec(`ALTER TABLE records ADD COLUMN routing TEXT`); err != nil {
			return err
		}
	}
	if !hasSchedule {
		if _, err := x.db.Exec(`ALTER TABLE records ADD COLUMN schedule TEXT`); err != nil {
			return err
		}
	}
	if !hasAPIKey {
		if _, err := x.db.Exec(`ALTER TABLE records ADD COLUMN api_key TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if _, err := x.db.Exec(`ALTER TABLE records ADD COLUMN attempt_only INTEGER NOT NULL DEFAULT 0`); err != nil {
		_ = err // already present on a newer schema — not an error
	}
	if _, err := x.db.Exec(`CREATE INDEX IF NOT EXISTS idx_records_api_key ON records(api_key)`); err != nil {
		return err
	}
	// Covering index for rankRecentSessions' newest-first session walk. It
	// must be created here, after the attempt_only ALTER: on a pre-column
	// database the partial-index WHERE clause cannot reference the column yet.
	if _, err := x.db.Exec(`CREATE INDEX IF NOT EXISTS idx_records_ts_session ON records(ts, session_id) WHERE attempt_only = 0`); err != nil {
		return err
	}
	return nil
}

// Run reconciles immediately and then on the tailing tick until Shutdown. The
// stop signal triggers one final reconcile — the final flush after the caller
// drained the log writer — before Run returns.
func (x *Indexer) Run() {
	if x == nil {
		return
	}
	defer close(x.closed)
	x.runReconcile()
	ticker := time.NewTicker(x.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			x.runReconcile()
		case <-x.done:
			x.runReconcile()
			return
		}
	}
}

// Shutdown stops the loop, waits for Run's final reconcile and closes the
// database. Run must have been started exactly once first.
func (x *Indexer) Shutdown() {
	if x == nil {
		return
	}
	x.stopOnce.Do(func() { close(x.done) })
	<-x.closed
	if err := x.db.Close(); err != nil {
		logx.Warnf("[request_log] index close failed: %v", err)
	}
}

func (x *Indexer) runReconcile() {
	if err := x.reconcile(); err != nil {
		n := x.reconcileErrors.Add(1)
		if n == 1 || n%1000 == 0 {
			logx.Warnf("[request_log] index reconcile failed (%d errors total): %v", n, err)
		}
	}
	select {
	case x.reconciled <- struct{}{}:
	default:
	}
}

// WaitReconciled blocks until the background loop finishes at least one more
// reconcile pass (or ctx is done). It is a test seam for observing the indexer
// without sleeps.
func (x *Indexer) WaitReconciled(ctx context.Context) error {
	if x == nil {
		return errors.New("nil indexer")
	}
	select {
	case <-x.reconciled:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// fileState is one log file's on-disk identity for reconcile.
type fileState struct {
	size  int64
	mtime int64
}

// reconcile brings the index in line with the directory: vanished files drop
// their rows (retention sweep), shrunk files are re-indexed from zero
// (truncation/rotation replace), and every file is tailed from its indexed
// cursor. Only newline-terminated lines are consumed; a trailing partial line
// (a record the writer has not finished) waits for the next pass.
func (x *Indexer) reconcile() error {
	entries, err := os.ReadDir(x.dir)
	if err != nil {
		return fmt.Errorf("readdir: %w", err)
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
	if err := x.dropVanished(present); err != nil {
		return err
	}
	names := make([]string, 0, len(present))
	for name := range present {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := x.syncFile(name, present[name].size, present[name].mtime); err != nil {
			return fmt.Errorf("index %s: %w", name, err)
		}
	}
	return nil
}

// deleteFileRows removes one file's indexed rows and returns the session ids
// they belonged to, so the caller can drop those sessions from the
// incremental aggregate cache (recompute-on-demand keeps the cache a derived
// view, never a source of truth). Shared by both delete sites: vanished
// files (retention sweep) and truncation re-index.
func deleteFileRows(tx *sql.Tx, path string) ([]string, error) {
	rows, err := tx.Query(`DELETE FROM records WHERE file = ? RETURNING session_id`, path)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if id != "" {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	return ids, nil
}

// dropVanished deletes the rows of indexed files that no longer exist on disk.
func (x *Indexer) dropVanished(present map[string]fileState) error {
	rows, err := x.db.Query(`SELECT path FROM files`)
	if err != nil {
		return err
	}
	var stale []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			_ = rows.Close()
			return err
		}
		if _, ok := present[path]; !ok {
			stale = append(stale, path)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	if len(stale) == 0 {
		return nil
	}
	tx, err := x.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var droppedSessions []string
	for _, path := range stale {
		ids, err := deleteFileRows(tx, path)
		if err != nil {
			return err
		}
		droppedSessions = append(droppedSessions, ids...)
		if _, err := tx.Exec(`DELETE FROM files WHERE path = ?`, path); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	x.sessions.dropSessions(droppedSessions)
	return nil
}

// syncFile tails one file: a size below the indexed cursor means the file was
// truncated or replaced (rotation rename + recreate), so its rows are dropped
// and the cursor rewinds; new bytes beyond the cursor are parsed and inserted
// in bounded batches, one transaction per batch.
func (x *Indexer) syncFile(name string, size, mtime int64) error {
	var cursor int64
	err := x.db.QueryRow(`SELECT size_indexed FROM files WHERE path = ?`, name).Scan(&cursor)
	if err == sql.ErrNoRows {
		cursor = 0
	} else if err != nil {
		return err
	}
	if size < cursor {
		tx, err := x.db.Begin()
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		droppedSessions, err := deleteFileRows(tx, name)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(
			`INSERT INTO files (path, size_indexed, mtime) VALUES (?, 0, ?)
			 ON CONFLICT(path) DO UPDATE SET size_indexed = 0, mtime = excluded.mtime`,
			name, mtime,
		); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		// The re-index re-inserts this file's rows from zero and folds them
		// back in as they arrive; dropping first is what keeps the fold from
		// double-counting the pre-truncation rows.
		x.sessions.dropSessions(droppedSessions)
		cursor = 0
	}
	if size == cursor {
		return nil
	}
	return x.appendFile(name, cursor, mtime)
}

// appendFile indexes the newline-terminated lines in (cursor, EOF). A line
// that fails to decode (a torn write or foreign content after a same-name
// recreate) is skipped but still advances the cursor — the file scan query
// path skips unparseable lines the same way.
func (x *Indexer) appendFile(name string, cursor, mtime int64) error {
	file, err := os.Open(filepath.Join(x.dir, name))
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Seek(cursor, io.SeekStart); err != nil {
		return err
	}
	reader := bufio.NewReaderSize(file, 256<<10)
	offset := cursor
	for {
		var batch []recordRow
		batchBytes := 0
		eof := false
		for len(batch) < indexBatchLines && batchBytes < int(x.batchMaxBytes) && !eof {
			line, readErr := reader.ReadBytes('\n')
			if readErr != nil {
				if readErr != io.EOF {
					return readErr
				}
				// io.EOF: the trailing bytes are an incomplete line still
				// being written — leave the cursor before them.
				eof = true
				continue
			}
			lineOffset := offset
			offset += int64(len(line))
			if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 {
				if row, ok := rowFromLine(name, trimmed, lineOffset, int64(len(line))); ok {
					batch = append(batch, row)
					batchBytes += len(line)
				}
			}
		}
		// Flush on a full batch, and on EOF even with an empty batch: complete
		// lines that failed to decode still advance the cursor (they are
		// consumed), otherwise reconcile would re-read the same garbage
		// forever.
		if len(batch) > 0 || (eof && offset != cursor) {
			if err := x.insertBatch(name, batch, offset, mtime); err != nil {
				return err
			}
		}
		if eof {
			return nil
		}
	}
}

// recordRow is one records-table row under construction.
type recordRow struct {
	record Record
	usage  Usage
	tsMs   int64
	file   string
	offset int64
	length int64
}

// rowFromLine decodes one JSONL line into its index row. The usage columns are
// the ExtractUsage projection of the response body, parsed once here so the
// sessions aggregation never re-parses bodies. Bodies themselves are blanked
// before the row joins a batch: the index persists only metadata plus the
// (file, offset, length) coordinates (Detail seeks the JSONL file), and a
// 500-line batch retaining full multi-MB bodies was the daemon's GB-scale heap
// spike during large-body catch-ups. Unparseable timestamps index as
// ts_ms = 0 (Filter.matches drops them whenever a time bound is present; the
// SQL mapping below does the same).
func rowFromLine(file string, line []byte, offset, length int64) (recordRow, bool) {
	var record Record
	if err := json.Unmarshal(line, &record); err != nil {
		return recordRow{}, false
	}
	usage := ExtractUsage(record.ResponseBody)
	record.RequestBody, record.ResponseBody = "", ""
	row := recordRow{
		record: record,
		usage:  usage,
		file:   file,
		offset: offset,
		length: length,
	}
	if ts, err := time.Parse(time.RFC3339, record.Ts); err == nil {
		row.tsMs = ts.UnixMilli()
	}
	return row, true
}

func (x *Indexer) insertBatch(name string, batch []recordRow, cursor, mtime int64) error {
	tx, err := x.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.Prepare(`INSERT INTO records
		(request_id, ts, ts_ms, session_id, agent, protocol, method, path, tool,
		 called_model, upstream_model, exposed, provider, attempt, status,
		 latency_ms, ttft_ms, request_size, response_size, turn_key, routing, schedule, kind, shadow, attempt_only,
		 input, output, cache_read, cache_creation, file, "offset", length, api_key)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, row := range batch {
		shadow := 0
		if row.record.Shadow {
			shadow = 1
		}
		attemptOnly := 0
		if row.record.AttemptOnly {
			attemptOnly = 1
		}
		routingArg := any(nil)
		if row.record.Routing != nil {
			if b, err := json.Marshal(row.record.Routing); err == nil {
				routingArg = string(b)
			}
		}
		scheduleArg := any(nil)
		if row.record.Schedule != nil && row.record.Schedule.Source != "" {
			if b, err := json.Marshal(row.record.Schedule); err == nil {
				scheduleArg = string(b)
			}
		}
		if _, err := stmt.Exec(
			row.record.RequestID, row.record.Ts, row.tsMs, row.record.SessionID,
			row.record.Agent, row.record.Protocol, row.record.Method, row.record.Path, row.record.Tool,
			row.record.CalledModel, row.record.UpstreamModel, row.record.Exposed,
			row.record.Provider, row.record.Attempt, row.record.Status,
			row.record.LatencyMs, row.record.TTFTMs, row.record.RequestSize, row.record.ResponseSize,
			row.record.TurnKey, routingArg, scheduleArg, row.record.Kind, shadow, attemptOnly, row.usage.Input, row.usage.Output, row.usage.CacheRead,
			row.usage.CacheCreation, row.file, row.offset, row.length, row.record.APIKeyID,
		); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO files (path, size_indexed, mtime) VALUES (?,?,?)
		 ON CONFLICT(path) DO UPDATE SET size_indexed = excluded.size_indexed, mtime = excluded.mtime`,
		name, cursor, mtime,
	); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Only after the commit are the rows queryable, so only then may the
	// incremental session aggregates fold them in.
	x.sessions.foldBatch(batch)
	return nil
}

// filterSQL maps Filter onto SQL with the exact matches() semantics: model is
// a case-insensitive substring across the three model fields, provider a
// case-insensitive substring, agent an exact match, status exact, errors the
// >= 400 band, shadow the only/exclude tri-state. With a time bound present, matches drops records
// whose Ts fails to parse — they index as ts_ms = 0, so the bound requires
// ts_ms > 0. Bounds compare whole milliseconds (ts is stored truncated); the
// web layer passes whole-second times, and sub-millisecond boundary records
// are the only divergence from the scan's full-precision instant compare.
func filterSQL(filter Filter) (string, []any) {
	var clauses []string
	var args []any
	if filter.RequestID != "" {
		clauses = append(clauses, `request_id = ?`)
		args = append(args, filter.RequestID)
	}
	if filter.Session != "" {
		clauses = append(clauses, `session_id = ?`)
		args = append(args, filter.Session)
	}
	if filter.Model != "" {
		clauses = append(clauses, `(instr(lower(called_model), lower(?)) > 0 OR instr(lower(upstream_model), lower(?)) > 0 OR instr(lower(exposed), lower(?)) > 0)`)
		args = append(args, filter.Model, filter.Model, filter.Model)
	}
	if filter.Provider != "" {
		clauses = append(clauses, `instr(lower(provider), lower(?)) > 0`)
		args = append(args, filter.Provider)
	}
	if filter.Agent != "" {
		clauses = append(clauses, `agent = ?`)
		args = append(args, filter.Agent)
	}
	if filter.Status != 0 {
		clauses = append(clauses, `status = ?`)
		args = append(args, filter.Status)
	}
	if filter.ErrorsOnly {
		clauses = append(clauses, `status >= 400`)
	}
	switch filter.Shadow {
	case "only":
		clauses = append(clauses, `shadow = 1`)
	case "exclude":
		clauses = append(clauses, `shadow = 0`)
	}
	switch filter.Kind {
	case "mcp":
		clauses = append(clauses, `kind = 'mcp'`)
	case "llm":
		clauses = append(clauses, `kind = ''`)
	}
	if !filter.From.IsZero() || !filter.To.IsZero() {
		clauses = append(clauses, `ts_ms > 0`)
		if !filter.From.IsZero() {
			clauses = append(clauses, `ts_ms >= ?`)
			args = append(args, filter.From.UnixMilli())
		}
		if !filter.To.IsZero() {
			clauses = append(clauses, `ts_ms <= ?`)
			args = append(args, filter.To.UnixMilli())
		}
	}
	if len(clauses) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// SummariesWithFacets is QuerySummariesWithFacets served from the index: same
// filter semantics, same newest-first top-K (the scan orders by the Ts string,
// so the index orders by the stored ts text), same Summary projection — the
// usage fields are populated only under UsageOnly, exactly like the scan's
// metadataOnly/UsageOnly projection. Facets keep the scan's "collected before
// the filter" semantics as one DISTINCT pass over every indexed row (the scan
// version's window is additionally bounded by its file-level early
// termination; the index answers over the whole retained set).
func (x *Indexer) SummariesWithFacets(filter Filter) ([]Summary, Facets, error) {
	where, args := filterSQL(filter)
	// The fold (semantics pinned equal to the scan engine's foldLegs pass):
	// committed rows always pass; a leg survives only when its request never
	// committed (unanswered — the client disconnected mid-failover). Paired
	// legs are re-attached below as the committed row's FailedHops. The
	// correlated NOT EXISTS rides idx_records_request.
	const fold = `(attempt_only = 0 OR NOT EXISTS (SELECT 1 FROM records c WHERE c.request_id = r.request_id AND c.attempt_only = 0))`
	if where == "" {
		where = ` WHERE ` + fold
	} else {
		where = ` WHERE ` + fold + ` AND ` + strings.TrimPrefix(where, " WHERE ")
	}
	query := `SELECT ts, request_id, session_id, protocol, method, path, tool, exposed,
		called_model, upstream_model, provider, agent, attempt, status, latency_ms, ttft_ms,
		request_size, response_size, turn_key, kind, input, output, cache_read, cache_creation, shadow, attempt_only
		FROM records r` + where + ` ORDER BY ts DESC, rowid DESC`
	if filter.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, filter.Limit)
	}
	rows, err := x.db.Query(query, args...)
	if err != nil {
		return nil, Facets{}, err
	}
	summaries := make([]Summary, 0)
	for rows.Next() {
		var (
			summary     Summary
			shadow      int
			attemptOnly int
		)
		if err := rows.Scan(
			&summary.Ts, &summary.RequestID, &summary.SessionID, &summary.Protocol,
			&summary.Method, &summary.Path, &summary.Tool, &summary.Exposed, &summary.CalledModel,
			&summary.UpstreamModel, &summary.Provider, &summary.Agent, &summary.Attempt,
			&summary.Status, &summary.LatencyMs, &summary.TTFTMs, &summary.RequestSize, &summary.ResponseSize,
			&summary.TurnKey, &summary.Kind, &summary.Input, &summary.Output, &summary.CacheRead, &summary.CacheCreation,
			&shadow, &attemptOnly,
		); err != nil {
			_ = rows.Close()
			return nil, Facets{}, err
		}
		summary.Shadow = shadow != 0
		summary.AttemptOnly = attemptOnly != 0
		if !filter.UsageOnly {
			summary.Input, summary.Output, summary.CacheRead, summary.CacheCreation = 0, 0, 0, 0
		}
		summaries = append(summaries, summary)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, Facets{}, err
	}
	_ = rows.Close()

	summaries, err = x.attachFailedHops(summaries)
	if err != nil {
		return nil, Facets{}, err
	}

	facets, err := x.queryFacets(filter)
	if err != nil {
		return nil, Facets{}, err
	}
	return summaries, facets, nil
}

// attachFailedHops completes the fold the SQL predicate started: the result
// set can still hold several legs of one unanswered request (each passed the
// fold's NOT EXISTS), so they collapse to the newest (first in the
// newest-first order) with the older legs as its FailedHops; every committed
// row then gains its legs' hop chain from a chunked lookup over the page's
// ids (idx_records_request). Hops sort attempt-ascending with Ts as the
// tie-break — the same order the scan engine's heldHops produces, so the
// index==scan parity holds field for field.
func (x *Indexer) attachFailedHops(summaries []Summary) ([]Summary, error) {
	committedIdx := make(map[string]int, len(summaries)) // committed request id -> out index
	seenLeg := make(map[string]int)                      // unanswered request id -> out index of its surviving leg
	legHops := make(map[int][]Summary)
	out := summaries[:0] // compact in place: a fold never adds rows, only drops
	for i := range summaries {
		if !summaries[i].AttemptOnly {
			committedIdx[summaries[i].RequestID] = len(out)
			out = append(out, summaries[i])
			continue
		}
		if idx, dup := seenLeg[summaries[i].RequestID]; dup {
			legHops[idx] = append(legHops[idx], summaries[i])
			continue
		}
		seenLeg[summaries[i].RequestID] = len(out)
		out = append(out, summaries[i])
	}
	for idx, older := range legHops {
		out[idx].FailedHops = sortedHops(older)
	}
	ids := make([]string, 0, len(committedIdx))
	for id := range committedIdx {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, chunk := range chunkIDs(ids, 500) {
		placeholders := strings.Repeat("?,", len(chunk))
		rows, err := x.db.Query(`SELECT request_id, provider, status, attempt, latency_ms, ts
			FROM records WHERE attempt_only = 1 AND request_id IN (`+placeholders[:len(placeholders)-1]+`)
			ORDER BY rowid`, idArgs(chunk)...)
		if err != nil {
			return nil, err
		}
		byID := make(map[string][]Summary, len(chunk))
		for rows.Next() {
			var s Summary
			if err := rows.Scan(&s.RequestID, &s.Provider, &s.Status, &s.Attempt, &s.LatencyMs, &s.Ts); err != nil {
				_ = rows.Close()
				return nil, err
			}
			byID[s.RequestID] = append(byID[s.RequestID], s)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return nil, err
		}
		_ = rows.Close()
		for id, hops := range byID {
			out[committedIdx[id]].FailedHops = sortedHops(hops)
		}
	}
	return out, nil
}

// sortedHops projects summary-shaped leg rows into FailedHops, sorted by
// (attempt, ts) ascending — the hop-chain order contract.
func sortedHops(legs []Summary) []FailedHop {
	sorted := make([]Summary, len(legs))
	copy(sorted, legs)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Attempt != sorted[j].Attempt {
			return sorted[i].Attempt < sorted[j].Attempt
		}
		return sorted[i].Ts < sorted[j].Ts
	})
	hops := make([]FailedHop, 0, len(sorted))
	for _, leg := range sorted {
		hops = append(hops, FailedHop{Provider: leg.Provider, Status: leg.Status, Attempt: leg.Attempt, LatencyMs: leg.LatencyMs})
	}
	return hops
}

func chunkIDs(ids []string, size int) [][]string {
	var chunks [][]string
	for start := 0; start < len(ids); start += size {
		end := start + size
		if end > len(ids) {
			end = len(ids)
		}
		chunks = append(chunks, ids[start:end])
	}
	return chunks
}

func idArgs(ids []string) []any {
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return args
}

// ShadowReport is requestlog.ShadowReport served from the index: the same
// window semantics (filterSQL's From/To over ts_ms), the same newest-first
// top-K retention before pairing (ORDER BY ts DESC LIMIT, where the scan
// keeps its timestamp heap), and the same pairing/aggregation (both feed
// shadowPairRow into aggregateShadowPairs). Like the scan, it never filters
// by kind: shadow evaluation rows are LLM-stream rows, and legacy kind=mcp
// rows can never pair (their ids carry no shadow- twin).
func (x *Indexer) ShadowReport(filter Filter) ([]ShadowReportEntry, error) {
	// Shadow evaluation rows are LLM-stream rows; ignore any kind filter so the
	// report stays comparable to the scan path.
	filter.Kind = ""
	where, args := filterSQL(filter)
	query := `SELECT request_id, shadow, exposed, provider, status, latency_ms, response_size
		FROM records` + where + ` ORDER BY ts DESC, rowid DESC`
	if filter.Limit > 0 {
		query += ` LIMIT ?`
		args = append(args, filter.Limit)
	}
	rows, err := x.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	pairRows := make([]shadowPairRow, 0, 64)
	for rows.Next() {
		var row shadowPairRow
		var shadow int
		if err := rows.Scan(&row.RequestID, &shadow, &row.Exposed, &row.Provider, &row.Status, &row.LatencyMs, &row.ResponseSize); err != nil {
			return nil, err
		}
		row.Shadow = shadow != 0
		pairRows = append(pairRows, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return aggregateShadowPairs(pairRows), nil
}

// queryFacets collects the distinct provider/model/agent sets over every
// indexed row through the same facetCollector the scan feeds, so the shapes
// (sorted slices, non-nil maps) are identical.
func (x *Indexer) queryFacets(filter Filter) (Facets, error) {
	// Kind is a stream selector, not a data facet: unlike model/provider
	// (whose facets are deliberately collected before the filter so dropdowns
	// cannot narrow themselves into a dead end), browsing one stream must not
	// offer the other stream's values — mcp server names would pollute the
	// model dropdown of an LLM-only view. Legs are excluded like the scan's
	// facet feed: the dropdowns describe the traffic that produces rows, and
	// legs fold into their committed rows.
	clauses := []string{`attempt_only = 0`}
	switch filter.Kind {
	case "mcp":
		clauses = append(clauses, `kind = 'mcp'`)
	case "llm":
		clauses = append(clauses, `kind = ''`)
	}
	rows, err := x.db.Query(`SELECT DISTINCT provider, exposed, called_model, agent FROM records WHERE ` + strings.Join(clauses, ` AND `))
	if err != nil {
		return Facets{}, err
	}
	defer func() { _ = rows.Close() }()
	collector := newFacetCollector(filter.Kind)
	for rows.Next() {
		var provider, exposed, called, agent string
		if err := rows.Scan(&provider, &exposed, &called, &agent); err != nil {
			return Facets{}, err
		}
		collector.add(Record{Provider: provider, Exposed: exposed, CalledModel: called, Agent: agent})
	}
	if err := rows.Err(); err != nil {
		return Facets{}, err
	}
	return collector.facets(), nil
}

// Detail returns the full records (bodies included) for one request id: a
// (file, offset, length) seek per indexed row instead of a directory scan.
// Any index miss — the id is not indexed yet (committed after the last
// reconcile pass), the file was rotated away, or the stored bytes no longer
// decode — falls back to tailScan, which streams only the UN-indexed bytes
// of each file: a just-committed record never reports "not logged", while a
// never-logged id (any pre-commit terminal, e.g. the live view's client-gone
// 499) stays a cheap miss instead of a whole-history scan.
func (x *Indexer) Detail(requestID string) ([]Record, error) {
	rows, err := x.db.Query(
		`SELECT file, "offset", length FROM records WHERE request_id = ? ORDER BY ts DESC, rowid DESC LIMIT ?`,
		requestID, indexDetailLimit,
	)
	if err != nil {
		return nil, err
	}
	type location struct {
		file   string
		offset int64
		length int64
	}
	var locations []location
	for rows.Next() {
		var loc location
		if err := rows.Scan(&loc.file, &loc.offset, &loc.length); err != nil {
			_ = rows.Close()
			return nil, err
		}
		locations = append(locations, loc)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	if len(locations) == 0 {
		return x.tailScan(requestID)
	}
	records := make([]Record, 0, len(locations))
	for _, loc := range locations {
		record, err := x.readRecordAt(loc.file, loc.offset, loc.length)
		if err != nil {
			return x.tailScan(requestID)
		}
		records = append(records, record)
	}
	return records, nil
}

// tailScan searches the UN-indexed tail of every log file for one request id:
// the Detail fallback's scanner. An index miss means the record either is not
// logged at all (any pre-commit terminal never writes one) or was committed
// after the last reconcile pass and lives beyond a file's indexed cursor;
// that lag window is the only region that can hold the latter, so exactly it
// is streamed — never the indexed history (the full-directory QueryRecords
// fallback this replaced turned every never-logged id into a multi-GB scan).
// Files are visited newest-first (reverse name order, like query) and EVERY
// match is collected, the aggregate capped at indexDetailLimit and ordered
// newest-first — the same shape as Detail's indexed path, which also returns
// up to indexDetailLimit rows for one id (retried attempts share the id).
// A file absent from the files table was
// never reconciled (an index being rebuilt from scratch), so its cursor is
// zero and the whole file is the tail; a truncated/replaced file (size below
// the cursor) has no readable tail until reconcile rewinds it (< one tick).
func (x *Indexer) tailScan(requestID string) ([]Record, error) {
	entries, err := os.ReadDir(x.dir)
	if err != nil {
		return nil, err
	}
	sizes := make(map[string]int64, len(entries))
	names := make([]string, 0, len(entries))
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
		sizes[name] = info.Size()
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil, nil
	}
	cursors := make(map[string]int64, len(names))
	rows, err := x.db.Query(`SELECT path, size_indexed FROM files`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var name string
		var cursor int64
		if err := rows.Scan(&name, &cursor); err != nil {
			_ = rows.Close()
			return nil, err
		}
		cursors[name] = cursor
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	sort.Strings(names)
	var matches []Record
	for i := len(names) - 1; i >= 0; i-- {
		name := names[i]
		if sizes[name] <= cursors[name] {
			continue // fully indexed: no un-indexed bytes can hold a lagging commit
		}
		records, err := scanTailForID(x.dir, name, cursors[name], requestID, indexDetailLimit-len(matches))
		if err != nil {
			return nil, err
		}
		matches = append(matches, records...)
		if len(matches) >= indexDetailLimit {
			break
		}
	}
	sort.SliceStable(matches, func(i, j int) bool { return matches[i].Ts > matches[j].Ts })
	return matches, nil
}

// scanTailForID reads the JSONL lines beyond cursor collecting every record
// with the given request id, at most limit of them — a non-positive limit
// collects nothing (never an unbounded tail scan of a multi-GB file, the
// hazard this bounded scan exists to avoid). cursor sits on a line
// boundary by construction (reconcile advances it only past
// newline-terminated lines); the final unterminated line is parsed too when
// it decodes (the scan path's convention — the writer's newline lands in the
// same Write, so a decodable line is complete content), while a torn write
// that fails to decode is skipped, same as every other scan path. The id
// prefilter is the byte containment query() applies before decoding, with
// Filter.matches's exact post-decode check inlined.
func scanTailForID(dir, name string, cursor int64, requestID string, limit int) ([]Record, error) {
	if limit <= 0 {
		return nil, nil
	}
	file, err := os.Open(filepath.Join(dir, name))
	if err != nil {
		// Vanished between readdir and open (retention sweep): nothing new.
		return nil, nil
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Seek(cursor, io.SeekStart); err != nil {
		return nil, err
	}
	reader := bufio.NewReaderSize(file, 256<<10)
	needle := []byte(requestID)
	var matches []Record
	for len(matches) < limit {
		line, readErr := reader.ReadBytes('\n')
		if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 && bytes.Contains(trimmed, needle) {
			var record Record
			if json.Unmarshal(trimmed, &record) == nil && record.RequestID == requestID {
				matches = append(matches, record)
			}
		}
		if readErr != nil {
			if readErr != io.EOF {
				return nil, readErr
			}
			return matches, nil
		}
	}
	return matches, nil
}

// readRecordAt reads and decodes one raw JSONL line at its indexed position.
func (x *Indexer) readRecordAt(file string, offset, length int64) (Record, error) {
	handle, err := os.Open(filepath.Join(x.dir, file))
	if err != nil {
		return Record{}, err
	}
	defer func() { _ = handle.Close() }()
	buffer := make([]byte, length)
	if _, err := handle.ReadAt(buffer, offset); err != nil {
		return Record{}, err
	}
	var record Record
	if err := json.Unmarshal(bytes.TrimSpace(buffer), &record); err != nil {
		return Record{}, err
	}
	return record, nil
}

// sessionAggregateGroupsSQL aggregates sessions' whole retained history in
// one grouped pass (session×provider×model×agent), so a computed session's
// summary always covers all of its records — never a newest-N window. Each
// group carries its per-dimension MIN/MAX so session span and provider/agent
// first-seen order fold newest-first, matching aggregateSessions. Used with
// an IN bound for the cache's compute pass, or bare (every session) for the
// limit <= 0 path.
const sessionAggregateGroupsSQL = `SELECT r.session_id, MIN(r.ts), MAX(r.ts), r.provider,
	COALESCE(NULLIF(r.upstream_model, ''), r.called_model) AS model,
	NULLIF(r.agent, '') AS agent,
	COUNT(*), SUM(CASE WHEN r.status >= 400 THEN 1 ELSE 0 END), SUM(r.shadow),
	SUM(r.input), SUM(r.output), SUM(r.cache_read), SUM(r.cache_creation),
	MAX(r.ts_ms)
	FROM records r
	WHERE r.session_id != '' AND r.attempt_only = 0`

// sessionRankScanCap bounds the newest-first ranking walk (rankRecentSessions):
// the limit most-recently-active sessions are collected by walking records
// newest-first until limit distinct ids appear, and this cap keeps a single
// runaway session from making that walk unbounded. A session newer than the
// cap's window is still aggregated exactly once listed — only the listing
// itself can come up short under pathological single-session domination.
const sessionRankScanCap = 100000

// rankRecentSessions lists up to limit session ids in true last-activity
// order: a newest-first covering-index walk over idx_records_ts_session
// (session-less and attempt-only rows skipped by predicate/projection), the
// k-th distinct id being the k-th most recently active session. The walk is
// bounded by sessionRankScanCap rows.
func (x *Indexer) rankRecentSessions(limit int) ([]string, error) {
	rows, err := x.db.Query(`SELECT session_id FROM records
		WHERE attempt_only = 0 AND session_id != ''
		ORDER BY ts DESC LIMIT ?`, sessionRankScanCap)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, limit)
	seen := make(map[string]bool, limit)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
		if len(ids) == limit {
			break
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	return ids, nil
}

// sessionRecency is one provider's/agent's/model-group's newest-record key
// (millisecond ts, second-precision ts text as tiebreak) — used to rebuild
// the newest-first first-seen lists aggregateSessions produces walking
// records newest-first.
type sessionRecency struct {
	ms int64
	ts string
}

func (r sessionRecency) after(o sessionRecency) bool {
	if r.ms != o.ms {
		return r.ms > o.ms
	}
	return r.ts > o.ts
}

// recencyOrder lists the seen keys newest-first by their newest record (the
// name tiebreak keeps it deterministic on exact ties).
func recencyOrder(seen map[string]sessionRecency) []string {
	out := make([]string, 0, len(seen))
	for key := range seen {
		out = append(out, key)
	}
	sort.Slice(out, func(i, j int) bool {
		if seen[out[i]].ms != seen[out[j]].ms {
			return seen[out[i]].ms > seen[out[j]].ms
		}
		if seen[out[i]].ts != seen[out[j]].ts {
			return seen[out[i]].ts > seen[out[j]].ts
		}
		return out[i] < out[j]
	})
	return out
}

type sessionModelKey struct{ provider, model string }

// sessionAggState is one session's folded aggregate: the counters behind
// SessionSummary (requests/errors/shadow, usage totals and per-(provider,
// model) splits for cost, provider/agent/model identity, session span). It
// is maintained two ways that must stay equivalent: computed from scratch by
// one grouped SQL pass (sessionAggStates) and folded record-by-record as the
// indexer inserts new rows (fold). Cost is NOT folded — pricing is resolved
// per request at render time.
type sessionAggState struct {
	requests, errors, shadow int
	usage                    Usage
	firstTs, lastTs          string
	byModel                  map[sessionModelKey]Usage
	provSeen                 map[string]sessionRecency
	agentSeen                map[string]sessionRecency
}

// fold merges one indexed record (already filtered: session-bearing,
// non-attempt). Mirrors the SQL grouping exactly.
func (s *sessionAggState) fold(rec Record, usage Usage, tsMs int64) {
	s.requests++
	if rec.Status >= 400 {
		s.errors++
	}
	if rec.Shadow {
		s.shadow++
	}
	s.usage.Input += usage.Input
	s.usage.Output += usage.Output
	s.usage.CacheRead += usage.CacheRead
	s.usage.CacheCreation += usage.CacheCreation
	model := rec.UpstreamModel
	if model == "" {
		model = rec.CalledModel
	}
	mk := sessionModelKey{provider: rec.Provider, model: model}
	u := s.byModel[mk]
	u.Input += usage.Input
	u.Output += usage.Output
	u.CacheRead += usage.CacheRead
	u.CacheCreation += usage.CacheCreation
	s.byModel[mk] = u
	last := sessionRecency{ms: tsMs, ts: rec.Ts}
	if last.after(s.provSeen[rec.Provider]) {
		s.provSeen[rec.Provider] = last
	}
	if rec.Agent != "" && last.after(s.agentSeen[rec.Agent]) {
		s.agentSeen[rec.Agent] = last
	}
	// RFC3339 fixed-width text compares chronologically.
	if rec.Ts < s.firstTs || s.firstTs == "" {
		s.firstTs = rec.Ts
	}
	if rec.Ts > s.lastTs {
		s.lastTs = rec.Ts
	}
}

// render applies the per-call pricing callback and projects the API shape.
func (s *sessionAggState) render(id string, costOf func(provider, model string, usage Usage) float64) SessionSummary {
	out := SessionSummary{
		SessionID:      id,
		FirstTs:        s.firstTs,
		LastTs:         s.lastTs,
		Requests:       s.requests,
		ShadowRequests: s.shadow,
		Errors:         s.errors,
		Usage:          s.usage,
		Providers:      recencyOrder(s.provSeen),
		Agents:         recencyOrder(s.agentSeen),
	}
	modelSeen := map[string]bool{}
	keys := make([]sessionModelKey, 0, len(s.byModel))
	for mk := range s.byModel {
		keys = append(keys, mk)
		if !modelSeen[mk.model] {
			modelSeen[mk.model] = true
			out.Models = append(out.Models, mk.model)
		}
	}
	sort.Strings(out.Models)
	if costOf != nil {
		// Deterministic order so cost float summation is reproducible
		// (aggregateSessions iterated a map — same terms, random order).
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].provider != keys[j].provider {
				return keys[i].provider < keys[j].provider
			}
			return keys[i].model < keys[j].model
		})
		for _, mk := range keys {
			out.CostUSD += costOf(mk.provider, mk.model, s.byModel[mk])
		}
	}
	return out
}

// sessionCacheMax bounds the incremental aggregate cache. Crossing it resets
// the map wholesale: sessions are recomputed from SQL on demand, so the cap
// trades one cold pass for unbounded memory in a months-long uptime.
const sessionCacheMax = 16384

// sessionAggCache is the Indexer's incremental per-session aggregate cache:
// entries are computed from SQL on first request, then kept exact by folding
// every inserted record (foldBatch, after the insert transaction commits)
// and dropping sessions whose rows were deleted (dropSessions, retention
// sweep and truncation re-index). Never a source of truth — dropping is
// always safe, recompute is always available.
type sessionAggCache struct {
	mu   sync.Mutex
	byID map[string]*sessionAggState
}

func (c *sessionAggCache) foldBatch(batch []recordRow) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.byID) == 0 {
		return
	}
	for _, row := range batch {
		rec := row.record
		if rec.AttemptOnly || rec.SessionID == "" {
			continue
		}
		if st := c.byID[rec.SessionID]; st != nil {
			st.fold(rec, row.usage, row.tsMs)
		}
	}
}

func (c *sessionAggCache) dropSessions(ids []string) {
	if len(ids) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range ids {
		delete(c.byID, id)
	}
}

// sessionAggStates computes the grouped-SQL aggregate for exactly ids (one
// pass). A session with no retained rows is absent from the result.
func (x *Indexer) sessionAggStates(ids []string) (map[string]*sessionAggState, error) {
	placeholders := make([]string, len(ids))
	args := make([]any, 0, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args = append(args, id)
	}
	query := sessionAggregateGroupsSQL +
		` AND r.session_id IN (` + strings.Join(placeholders, ",") + `)` +
		` GROUP BY r.session_id, r.provider, model, agent`
	return x.scanSessionAggStates(x.db.Query(query, args...))
}

// scanSessionAggStates folds grouped SQL rows into per-session states.
func (x *Indexer) scanSessionAggStates(rows *sql.Rows, err error) (map[string]*sessionAggState, error) {
	if err != nil {
		return nil, err
	}
	bySession := map[string]*sessionAggState{}
	for rows.Next() {
		var (
			sessionID, firstTs, lastTs, provider, model string
			agent                                       sql.NullString
			requests, errors, shadow                    int64
			input, output, cacheRead, cacheCreation     int64
			lastMs                                      int64
		)
		if err := rows.Scan(
			&sessionID, &firstTs, &lastTs, &provider, &model, &agent,
			&requests, &errors, &shadow, &input, &output, &cacheRead, &cacheCreation,
			&lastMs,
		); err != nil {
			_ = rows.Close()
			return nil, err
		}
		s := bySession[sessionID]
		if s == nil {
			s = &sessionAggState{
				firstTs:   firstTs,
				lastTs:    lastTs,
				byModel:   map[sessionModelKey]Usage{},
				provSeen:  map[string]sessionRecency{},
				agentSeen: map[string]sessionRecency{},
			}
			bySession[sessionID] = s
		}
		// first/last ts arrive per (provider,model,agent) group — fold to the
		// session span.
		if firstTs < s.firstTs {
			s.firstTs = firstTs
		}
		if lastTs > s.lastTs {
			s.lastTs = lastTs
		}
		s.requests += int(requests)
		s.errors += int(errors)
		s.shadow += int(shadow)
		s.usage.Input += uint64(input)
		s.usage.Output += uint64(output)
		s.usage.CacheRead += uint64(cacheRead)
		s.usage.CacheCreation += uint64(cacheCreation)
		mk := sessionModelKey{provider: provider, model: model}
		u := s.byModel[mk]
		u.Input += uint64(input)
		u.Output += uint64(output)
		u.CacheRead += uint64(cacheRead)
		u.CacheCreation += uint64(cacheCreation)
		s.byModel[mk] = u
		last := sessionRecency{ms: lastMs, ts: lastTs}
		if last.after(s.provSeen[provider]) {
			s.provSeen[provider] = last
		}
		if agent.Valid && agent.String != "" && last.after(s.agentSeen[agent.String]) {
			s.agentSeen[agent.String] = last
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	return bySession, nil
}

// SessionSummaries serves exact per-session aggregates from the index: every
// retained record of a kept session counts (attempt legs and session-less
// rows excluded, like the scan), not just a newest-N window. The session
// drill-down table (/api/requests?session=) answers over the whole retained
// set, so the summary must not be window-truncated either — a global
// newest-records window made a session that started days ago but is active
// today report only its newest requests (2 of 12 with the matching token
// subset). Sessions rank by last activity via a bounded newest-first walk
// (rankRecentSessions); their aggregates are served from the incremental
// cache, the missing subset computed in one grouped SQL pass with the cache
// lock held (so a concurrent reconcile fold can neither race the store nor
// be lost). limit <= 0 keeps every session — that path skips the cache and
// ranks the whole grouped result in Go (tests only). Bounded-window
// aggregation exists only on the file-scan fallback (package SessionSummaries).
func (x *Indexer) SessionSummaries(limit int, costOf func(provider, model string, usage Usage) float64) ([]SessionSummary, error) {
	if limit <= 0 {
		states, err := x.scanSessionAggStates(x.db.Query(sessionAggregateGroupsSQL + ` GROUP BY r.session_id, r.provider, model, agent`))
		if err != nil {
			return nil, err
		}
		out := make([]SessionSummary, 0, len(states))
		for id, s := range states {
			out = append(out, s.render(id, costOf))
		}
		sort.Slice(out, func(i, j int) bool { return out[i].LastTs > out[j].LastTs })
		return out, nil
	}
	ids, err := x.rankRecentSessions(limit)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return []SessionSummary{}, nil
	}
	// Hold the cache lock across the compute-and-store so folds from
	// reconcile commits serialize against it (blocked folds simply run after;
	// their rows are already in the SQL the compute reads, or arrive after
	// the state is stored and fold on top — either way no delta is lost).
	x.sessions.mu.Lock()
	defer x.sessions.mu.Unlock()
	states := make([]*sessionAggState, len(ids))
	var missing []string
	for i, id := range ids {
		if st := x.sessions.byID[id]; st != nil {
			states[i] = st
		} else {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		computed, err := x.sessionAggStates(missing)
		if err != nil {
			return nil, err
		}
		if x.sessions.byID == nil || len(x.sessions.byID)+len(computed) > sessionCacheMax {
			x.sessions.byID = make(map[string]*sessionAggState, len(computed))
		}
		for id, st := range computed {
			x.sessions.byID[id] = st
		}
		for i, id := range ids {
			if states[i] == nil {
				states[i] = computed[id]
			}
		}
	}
	out := make([]SessionSummary, 0, len(ids))
	for i, id := range ids {
		// A session whose rows vanished between walk and compute (retention
		// sweep) simply drops out of the listing.
		if states[i] == nil {
			continue
		}
		out = append(out, states[i].render(id, costOf))
	}
	return out, nil
}
