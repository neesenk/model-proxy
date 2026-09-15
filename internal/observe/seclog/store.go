// store.go — the queryable SQLite half of the security audit log.
//
// The audit log is deliberately two-tier:
//
//   - the JSONL half (observe/logfile sink) records EVERYTHING, including
//     ignored-tier low verdicts — a full-fidelity raw trail an operator can
//     tail, but it is no longer queried by the daemon, WebUI or CLI;
//   - this SQLite half (security.db next to the JSONL files) is the only
//     query surface: exact-match hits, drift, medium/high verdicts and the
//     fail-open error/skipped records land here; low verdicts are excluded
//     by design ("ignored" means no query presence, not no trace).
//
// Concurrency follows the in-repo SQLite recipe (internal/observe/stats,
// requestlog's index): WAL + busy_timeout(250ms) + synchronous(NORMAL) with a
// single connection per handle, so the daemon writer and offline CLI readers
// coexist on one database file.
package seclog

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // pure-Go driver; registers as "sqlite" with database/sql
)

const (
	storeDriver           = "sqlite"
	storeBusyTimeoutMs    = 250
	storeFileName         = "security.db"
	storeInsertStatements = `INSERT INTO audit_records
		(ts, kind, request_id, session_id, agent, protocol, exposed, names, action, verdict, reason, evidence, model, detail)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
)

// verdictLow is the ignored tier: suppressed LLM verdicts keep their JSONL
// trace but never enter the queryable store. Package-private — the verdict
// vocabulary is owned by the producers; this package only persists it.
const verdictLow = "low"

// Filter narrows an audit query. From/To are unix milliseconds, inclusive;
// zero means unbounded. Limit <= 0 returns every match. A non-empty
// RequestID restricts the query to one request's records.
type Filter struct {
	Kind      string
	From, To  int64
	Limit     int
	RequestID string
}

// Result is the outcome of a Query: matching records newest first. Truncated
// reports that a positive Limit dropped at least one matching record, so the
// returned set is a newest-first prefix, not the whole filtered set. Skipped
// stays in the shape for API compatibility but is always zero now — a SQLite
// row cannot be an unreadable line.
type Result struct {
	Records   []*Record
	Skipped   int
	Truncated bool
}

// store is one open handle on security.db.
type store struct {
	db *sql.DB
}

// storePath returns the database file location inside the audit directory.
func storePath(dir string) string { return filepath.Join(dir, storeFileName) }

// openStore opens or creates the audit store in dir, applying migrations.
func openStore(dir string) (*store, error) {
	if dir == "" {
		return nil, fmt.Errorf("seclog: empty directory")
	}
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, fmt.Errorf("seclog: store dir: %w", err)
	}
	dsn := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)",
		storePath(dir), storeBusyTimeoutMs,
	)
	db, err := sql.Open(storeDriver, dsn)
	if err != nil {
		return nil, fmt.Errorf("seclog: open store: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("seclog: ping store: %w", err)
	}
	// Owner-only, same contract as the JSONL files (the driver creates the
	// file with the process umask).
	_ = os.Chmod(storePath(dir), logFileMode)
	s := &store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// openQueryStore opens the store only when the database file already exists —
// a query must never create storage. A missing file yields (nil, nil): no
// queryable data yet, indistinguishable from an empty store for the caller.
func openQueryStore(dir string) (*store, error) {
	if dir == "" {
		return nil, fmt.Errorf("seclog: empty directory")
	}
	if _, err := os.Stat(storePath(dir)); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("seclog: stat store: %w", err)
	}
	return openStore(dir)
}

// Close releases the handle. Safe on a nil store.
func (s *store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// migrate is additive-only (same policy as the stats store).
func (s *store) migrate() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS audit_records (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		ts         INTEGER NOT NULL,
		kind       TEXT    NOT NULL,
		request_id TEXT    NOT NULL DEFAULT '',
		session_id TEXT    NOT NULL DEFAULT '',
		agent      TEXT    NOT NULL DEFAULT '',
		protocol   TEXT    NOT NULL DEFAULT '',
		exposed    TEXT    NOT NULL DEFAULT '',
		names      TEXT    NOT NULL DEFAULT '',
		action     TEXT    NOT NULL DEFAULT '',
		verdict    TEXT    NOT NULL DEFAULT '',
		reason     TEXT    NOT NULL DEFAULT '',
		evidence   TEXT    NOT NULL DEFAULT '',
		model      TEXT    NOT NULL DEFAULT '',
		detail     TEXT    NOT NULL DEFAULT ''
	);
	CREATE INDEX IF NOT EXISTS idx_audit_ts      ON audit_records(ts);
	CREATE INDEX IF NOT EXISTS idx_audit_kind_ts ON audit_records(kind, ts);
	CREATE INDEX IF NOT EXISTS idx_audit_request ON audit_records(request_id);`)
	if err != nil {
		return fmt.Errorf("seclog: migrate store: %w", err)
	}
	return nil
}

// insert persists one record unless it is an ignored-tier low verdict. Callers
// run on a single goroutine (the JSONL writer goroutine, or an offline CLI
// path), so one connection serializes every write.
func (s *store) insert(rec *Record) error {
	if s == nil || s.db == nil {
		return nil
	}
	if rec.Verdict == verdictLow {
		return nil
	}
	names := ""
	if len(rec.Names) > 0 {
		b, err := json.Marshal(rec.Names)
		if err != nil {
			return fmt.Errorf("seclog: encode names: %w", err)
		}
		names = string(b)
	}
	_, err := s.db.Exec(storeInsertStatements,
		rec.Ts, rec.Kind, rec.RequestID, rec.SessionID, rec.Agent, rec.Protocol,
		rec.Exposed, names, rec.Action, rec.Verdict, rec.Reason, rec.Evidence, rec.Model, rec.Detail)
	if err != nil {
		return fmt.Errorf("seclog: insert audit record: %w", err)
	}
	return nil
}

// query returns matching records newest first; a positive limit keeps the
// newest K and reports Truncated exactly via a same-predicate count.
func (s *store) query(f Filter) (*Result, error) {
	where, args := storeWhere(f)
	rows, err := s.db.Query(`SELECT ts, kind, request_id, session_id, agent, protocol, exposed,
		names, action, verdict, reason, evidence, model, detail
		FROM audit_records`+where+` ORDER BY ts DESC, id DESC`+storeLimit(f), args...)
	if err != nil {
		return nil, fmt.Errorf("seclog: query store: %w", err)
	}
	defer rows.Close()
	result := &Result{}
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		result.Records = append(result.Records, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("seclog: query store: %w", err)
	}
	if f.Limit > 0 {
		var total int
		q := `SELECT COUNT(*) FROM audit_records` + where
		if err := s.db.QueryRow(q, args...).Scan(&total); err != nil {
			return nil, fmt.Errorf("seclog: count store: %w", err)
		}
		result.Truncated = total > f.Limit
	}
	return result, nil
}

// Counts aggregates verdict totals over the same predicate Query uses
// (kind/from/to; limit and request_id are row-shaped concerns and ignored).
// It is the durable source for the Security KPI tiles: counting the merged
// client feed instead made the tiles drift with the in-memory ring's state.
// Verdict "" (classic records without a verdict — exact-match blocks, drift)
// is included under the empty key for the caller to fold where needed.
func Counts(dir string, filter Filter) (map[string]int64, error) {
	s, err := openQueryStore(dir)
	if err != nil {
		return nil, err
	}
	defer s.Close() // nil-safe
	if s == nil {
		return map[string]int64{}, nil
	}
	f := Filter{Kind: filter.Kind, From: filter.From, To: filter.To}
	where, args := storeWhere(f)
	rows, err := s.db.Query(`SELECT verdict, COUNT(*) FROM audit_records`+where+` GROUP BY verdict`, args...)
	if err != nil {
		return nil, fmt.Errorf("seclog: counts store: %w", err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var verdict string
		var n int64
		if err := rows.Scan(&verdict, &n); err != nil {
			return nil, err
		}
		out[verdict] = n
	}
	return out, rows.Err()
}

// storeWhere builds the predicate for a filter (kind exact, inclusive ms
// bounds); args matches its placeholder order so SELECT and COUNT agree.
func storeWhere(f Filter) (string, []any) {
	clauses := make([]string, 0, 3)
	args := make([]any, 0, 3)
	if f.Kind != "" {
		clauses = append(clauses, "kind = ?")
		args = append(args, f.Kind)
	}
	if f.RequestID != "" {
		clauses = append(clauses, "request_id = ?")
		args = append(args, f.RequestID)
	}
	if f.From != 0 {
		clauses = append(clauses, "ts >= ?")
		args = append(args, f.From)
	}
	if f.To != 0 {
		clauses = append(clauses, "ts <= ?")
		args = append(args, f.To)
	}
	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + joinStoreClauses(clauses), args
}

func joinStoreClauses(clauses []string) string {
	out := clauses[0]
	for _, c := range clauses[1:] {
		out += " AND " + c
	}
	return out
}

func storeLimit(f Filter) string {
	if f.Limit > 0 {
		return fmt.Sprintf(" LIMIT %d", f.Limit)
	}
	return ""
}

func scanRecord(rows *sql.Rows) (*Record, error) {
	var rec Record
	var names string
	if err := rows.Scan(&rec.Ts, &rec.Kind, &rec.RequestID, &rec.SessionID, &rec.Agent,
		&rec.Protocol, &rec.Exposed, &names, &rec.Action, &rec.Verdict, &rec.Reason,
		&rec.Evidence, &rec.Model, &rec.Detail); err != nil {
		return nil, fmt.Errorf("seclog: scan audit record: %w", err)
	}
	if names != "" {
		if err := json.Unmarshal([]byte(names), &rec.Names); err != nil {
			return nil, fmt.Errorf("seclog: decode names: %w", err)
		}
	}
	return &rec, nil
}

// Query returns matching audit records newest first from the SQLite store in
// dir. A directory without a database file yet is an empty store (empty
// result, no error, no file created). Semantics carried over from the JSONL
// era: Filter.Kind exact, From/To inclusive unix milliseconds, Limit > 0 the
// newest K with exact Truncated, Limit <= 0 everything.
func Query(dir string, filter Filter) (*Result, error) {
	s, err := openQueryStore(dir)
	if err != nil {
		return nil, err
	}
	defer s.Close() // nil-safe
	if s == nil {
		return &Result{}, nil
	}
	return s.query(filter)
}

// queryByRequestIDsChunk is the IN-list bound: one query page of request ids,
// comfortably under SQLite's default 999 host-parameter limit.
const queryByRequestIDsChunk = 500

// QueryByRequestIDs returns the audit records for a batch of requests in one
// round trip family (one chunked IN query per 500 ids), newest first per id.
// It is the request↔guard correlation source for the requests read surfaces;
// empty ids return an empty map, and a directory without a database file is an
// empty store (no error, no file created).
func QueryByRequestIDs(dir string, ids []string) (map[string][]*Record, error) {
	out := make(map[string][]*Record, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	// Deduplicate: a summary page can repeat a request id (primary + shadow
	// records of one request).
	unique := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		unique = append(unique, id)
	}
	if len(unique) == 0 {
		return out, nil
	}
	s, err := openQueryStore(dir)
	if err != nil {
		return nil, err
	}
	defer s.Close() // nil-safe
	if s == nil {
		return out, nil
	}
	for start := 0; start < len(unique); start += queryByRequestIDsChunk {
		end := start + queryByRequestIDsChunk
		if end > len(unique) {
			end = len(unique)
		}
		page := unique[start:end]
		placeholders := make([]byte, 0, len(page)*2)
		args := make([]any, 0, len(page))
		for i, id := range page {
			if i > 0 {
				placeholders = append(placeholders, ',')
			}
			placeholders = append(placeholders, '?')
			args = append(args, id)
		}
		rows, err := s.db.Query(`SELECT ts, kind, request_id, session_id, agent, protocol, exposed,
			names, action, verdict, reason, evidence, model, detail
			FROM audit_records WHERE request_id IN (`+string(placeholders)+`) ORDER BY ts DESC, id DESC`, args...)
		if err != nil {
			return nil, fmt.Errorf("seclog: query store by request ids: %w", err)
		}
		for rows.Next() {
			rec, err := scanRecord(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			out[rec.RequestID] = append(out[rec.RequestID], rec)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("seclog: query store by request ids: %w", err)
		}
		rows.Close()
	}
	return out, nil
}
