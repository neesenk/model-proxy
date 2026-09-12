package requestlog

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
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
	// defaultReconcileInterval is the tailing tick: how often the indexer
	// picks up appended lines, rotations, truncations and retention deletes.
	defaultReconcileInterval = 250 * time.Millisecond
	// indexDetailLimit mirrors the detail handler's scan filter
	// (Filter{RequestID: id, Limit: 50}).
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

	done     chan struct{}
	closed   chan struct{}
	stopOnce sync.Once

	reconcileErrors atomic.Uint64
	// reconciled is signaled (non-blocking) after every finished reconcile
	// pass — a test seam for observing the background loop without sleeps.
	reconciled chan struct{}
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
		dir:        dir,
		db:         db,
		tick:       defaultReconcileInterval,
		done:       make(chan struct{}),
		closed:     make(chan struct{}),
		reconciled: make(chan struct{}, 1),
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
		shadow         INTEGER NOT NULL,
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
// before it keep 0 = unknown.
func (x *Indexer) ensureColumns() error {
	rows, err := x.db.Query(`PRAGMA table_info(records)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	hasTTFT := false
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == "ttft_ms" {
			hasTTFT = true
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
	for _, path := range stale {
		if _, err := tx.Exec(`DELETE FROM records WHERE file = ?`, path); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM files WHERE path = ?`, path); err != nil {
			return err
		}
	}
	return tx.Commit()
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
		if _, err := tx.Exec(`DELETE FROM records WHERE file = ?`, name); err != nil {
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
		eof := false
		for len(batch) < indexBatchLines && !eof {
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
// sessions aggregation never re-parses bodies. Unparseable timestamps index as
// ts_ms = 0 (Filter.matches drops them whenever a time bound is present; the
// SQL mapping below does the same).
func rowFromLine(file string, line []byte, offset, length int64) (recordRow, bool) {
	var record Record
	if err := json.Unmarshal(line, &record); err != nil {
		return recordRow{}, false
	}
	row := recordRow{
		record: record,
		usage:  ExtractUsage(record.ResponseBody),
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
		(request_id, ts, ts_ms, session_id, agent, protocol, method, path,
		 called_model, upstream_model, exposed, provider, attempt, status,
		 latency_ms, ttft_ms, request_size, response_size, shadow,
		 input, output, cache_read, cache_creation, file, "offset", length)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, row := range batch {
		shadow := 0
		if row.record.Shadow {
			shadow = 1
		}
		if _, err := stmt.Exec(
			row.record.RequestID, row.record.Ts, row.tsMs, row.record.SessionID,
			row.record.Agent, row.record.Protocol, row.record.Method, row.record.Path,
			row.record.CalledModel, row.record.UpstreamModel, row.record.Exposed,
			row.record.Provider, row.record.Attempt, row.record.Status,
			row.record.LatencyMs, row.record.TTFTMs, row.record.RequestSize, row.record.ResponseSize,
			shadow, row.usage.Input, row.usage.Output, row.usage.CacheRead,
			row.usage.CacheCreation, row.file, row.offset, row.length,
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
	return tx.Commit()
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
	query := `SELECT ts, request_id, session_id, protocol, method, path, exposed,
		called_model, upstream_model, provider, agent, attempt, status, latency_ms, ttft_ms,
		request_size, response_size, input, output, cache_read, cache_creation, shadow
		FROM records` + where + ` ORDER BY ts DESC, rowid DESC`
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
			summary Summary
			shadow  int
		)
		if err := rows.Scan(
			&summary.Ts, &summary.RequestID, &summary.SessionID, &summary.Protocol,
			&summary.Method, &summary.Path, &summary.Exposed, &summary.CalledModel,
			&summary.UpstreamModel, &summary.Provider, &summary.Agent, &summary.Attempt,
			&summary.Status, &summary.LatencyMs, &summary.TTFTMs, &summary.RequestSize, &summary.ResponseSize,
			&summary.Input, &summary.Output, &summary.CacheRead, &summary.CacheCreation,
			&shadow,
		); err != nil {
			_ = rows.Close()
			return nil, Facets{}, err
		}
		summary.Shadow = shadow != 0
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

	facets, err := x.queryFacets()
	if err != nil {
		return nil, Facets{}, err
	}
	return summaries, facets, nil
}

// queryFacets collects the distinct provider/model/agent sets over every
// indexed row through the same facetCollector the scan feeds, so the shapes
// (sorted slices, non-nil maps) are identical.
func (x *Indexer) queryFacets() (Facets, error) {
	rows, err := x.db.Query(`SELECT DISTINCT provider, exposed, called_model, agent FROM records`)
	if err != nil {
		return Facets{}, err
	}
	defer func() { _ = rows.Close() }()
	collector := newFacetCollector()
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
// (file, offset, length) seek per indexed row instead of a directory scan. Any
// index miss — the id is not indexed yet (committed within the last reconcile
// tick), the file was rotated away, or the stored bytes no longer decode —
// falls back to the file scan, so a just-committed record never reports
// "not logged".
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
		return QueryRecords(x.dir, Filter{RequestID: requestID, Limit: indexDetailLimit})
	}
	records := make([]Record, 0, len(locations))
	for _, loc := range locations {
		record, err := x.readRecordAt(loc.file, loc.offset, loc.length)
		if err != nil {
			return QueryRecords(x.dir, Filter{RequestID: requestID, Limit: indexDetailLimit})
		}
		records = append(records, record)
	}
	return records, nil
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

// SessionSummaries is SessionSummaries served from the index: the newest
// scanLimit rows (session-less rows included, matching the scan's top-K
// window) with usage read from the index columns, aggregated by the same Go
// code the file-scan version uses.
func (x *Indexer) SessionSummaries(scanLimit, limit int, costOf func(model string, usage Usage) float64) ([]SessionSummary, error) {
	query := `SELECT ts, session_id, shadow, status, provider, upstream_model,
		called_model, agent, input, output, cache_read, cache_creation
		FROM records ORDER BY ts DESC, rowid DESC`
	var args []any
	if scanLimit > 0 {
		query += ` LIMIT ?`
		args = append(args, scanLimit)
	}
	rows, err := x.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	records := make([]Record, 0)
	for rows.Next() {
		var (
			record Record
			shadow int
		)
		if err := rows.Scan(
			&record.Ts, &record.SessionID, &shadow, &record.Status, &record.Provider,
			&record.UpstreamModel, &record.CalledModel, &record.Agent,
			&record.ParsedUsage.Input, &record.ParsedUsage.Output,
			&record.ParsedUsage.CacheRead, &record.ParsedUsage.CacheCreation,
		); err != nil {
			_ = rows.Close()
			return nil, err
		}
		record.Shadow = shadow != 0
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()
	return aggregateSessions(records, limit, costOf), nil
}
