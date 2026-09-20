package stats

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver; registers as "sqlite" with database/sql
)

const (
	driver                        = "sqlite"
	sqliteBusyTimeoutMilliseconds = 250
)

// Store is the SQLite persistence layer for minute-level statistics.
//
// database/sql is goroutine-safe. Store limits SQLite to one connection to
// match the single periodic writer while allowing query callers to share it.
type Store struct {
	db        *sql.DB
	retention time.Duration
}

// Open opens or creates a stats database and applies additive migrations.
func Open(options Options) (*Store, error) {
	if dir := filepath.Dir(options.Path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("stats db dir: %w", err)
		}
	}
	dsn := fmt.Sprintf(
		"file:%s?_pragma=busy_timeout(%d)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)",
		options.Path,
		sqliteBusyTimeoutMilliseconds,
	)
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("open stats db: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping stats db: %w", err)
	}

	store := &Store{db: db, retention: options.Retention}
	if err := store.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// Close closes the underlying database. It is safe to call on a nil Store.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS minute_buckets (
		provider    TEXT NOT NULL,
		model       TEXT NOT NULL,
		minute      INTEGER NOT NULL,
		requests    INTEGER NOT NULL DEFAULT 0,
		failovers   INTEGER NOT NULL DEFAULT 0,
		rate_limited_429 INTEGER NOT NULL DEFAULT 0,
		failures    INTEGER NOT NULL DEFAULT 0,
		input       INTEGER NOT NULL DEFAULT 0,
		output      INTEGER NOT NULL DEFAULT 0,
		cache_creation INTEGER NOT NULL DEFAULT 0,
		cache_read  INTEGER NOT NULL DEFAULT 0,
		token_requests INTEGER NOT NULL DEFAULT 0,
		last_request_at INTEGER NOT NULL DEFAULT 0,
		latency_ms_sum  INTEGER NOT NULL DEFAULT 0,
		ttft_ms_sum     INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (provider, model, minute)
	);
	CREATE INDEX IF NOT EXISTS idx_minute ON minute_buckets(minute);

	CREATE TABLE IF NOT EXISTS agent_buckets (
		agent     TEXT NOT NULL,
		provider  TEXT NOT NULL,
		model     TEXT NOT NULL,
		minute    INTEGER NOT NULL,
		requests  INTEGER NOT NULL DEFAULT 0,
		input     INTEGER NOT NULL DEFAULT 0,
		output    INTEGER NOT NULL DEFAULT 0,
		cache_creation INTEGER NOT NULL DEFAULT 0,
		cache_read  INTEGER NOT NULL DEFAULT 0,
		latency_ms_sum INTEGER NOT NULL DEFAULT 0,
		failures  INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (agent, provider, model, minute)
	);
	CREATE INDEX IF NOT EXISTS idx_agent_minute ON agent_buckets(minute);

	CREATE TABLE IF NOT EXISTS mcp_buckets (
		name TEXT NOT NULL,
		kind TEXT NOT NULL,
		minute INTEGER NOT NULL,
		calls INTEGER DEFAULT 0,
		errors INTEGER DEFAULT 0,
		latency_ms_sum INTEGER DEFAULT 0,
		last_call_at INTEGER DEFAULT 0,
		PRIMARY KEY (name, kind, minute)
	);
	CREATE INDEX IF NOT EXISTS idx_mcp_minute ON mcp_buckets(minute);`)
	if err != nil {
		return fmt.Errorf("migrate stats schema: %w", err)
	}
	if err := s.ensureColumns("agent_buckets", [][2]string{
		{"latency_ms_sum", "INTEGER NOT NULL DEFAULT 0"},
		{"failures", "INTEGER NOT NULL DEFAULT 0"},
		{"cache_creation", "INTEGER NOT NULL DEFAULT 0"},
		{"cache_read", "INTEGER NOT NULL DEFAULT 0"},
		{"ttft_ms_sum", "INTEGER NOT NULL DEFAULT 0"},
		{"duration_ms_sum", "INTEGER NOT NULL DEFAULT 0"},
	}); err != nil {
		return fmt.Errorf("migrate stats schema: %w", err)
	}
	if err := s.ensureColumns("minute_buckets", [][2]string{
		{"latency_ms_sum", "INTEGER NOT NULL DEFAULT 0"},
		{"ttft_ms_sum", "INTEGER NOT NULL DEFAULT 0"},
		{"duration_ms_sum", "INTEGER NOT NULL DEFAULT 0"},
	}); err != nil {
		return fmt.Errorf("migrate stats schema: %w", err)
	}
	return nil
}

func (s *Store) ensureColumns(table string, columns [][2]string) error {
	present, err := s.columnSet(table)
	if err != nil {
		return err
	}
	for _, column := range columns {
		if present[column[0]] {
			continue
		}
		if _, err := s.db.Exec(fmt.Sprintf(
			"ALTER TABLE %s ADD COLUMN %s %s",
			table,
			column[0],
			column[1],
		)); err != nil {
			return fmt.Errorf("add %s.%s: %w", table, column[0], err)
		}
	}
	return nil
}

func (s *Store) columnSet(table string) (map[string]bool, error) {
	rows, err := s.db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	columns := map[string]bool{}
	for rows.Next() {
		var (
			columnID int
			name     string
			typ      string
			notNull  int
			defaultV sql.NullString
			primary  int
		)
		if err := rows.Scan(&columnID, &name, &typ, &notNull, &defaultV, &primary); err != nil {
			return nil, err
		}
		columns[name] = true
	}
	return columns, rows.Err()
}

func (s *Store) isEmpty() (bool, error) {
	var count int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM minute_buckets`).Scan(&count); err != nil {
		return false, err
	}
	return count == 0, nil
}

// Flush upserts provider/model deltas into one minute bucket.
func (s *Store) Flush(minute int64, deltas map[Key]Counters) error {
	return s.FlushContext(context.Background(), minute, deltas)
}

// FlushContext is Flush with cancellation for lifecycle-bounded writes.
func (s *Store) FlushContext(
	ctx context.Context,
	minute int64,
	deltas map[Key]Counters,
) error {
	if len(deltas) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `INSERT INTO minute_buckets
		(provider, model, minute, requests, failovers, rate_limited_429, failures,
		 input, output, cache_creation, cache_read, token_requests, last_request_at,
		 latency_ms_sum, ttft_ms_sum, duration_ms_sum)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(provider, model, minute) DO UPDATE SET
			requests = requests + excluded.requests,
			failovers = failovers + excluded.failovers,
			rate_limited_429 = rate_limited_429 + excluded.rate_limited_429,
			failures = failures + excluded.failures,
			input = input + excluded.input,
			output = output + excluded.output,
			cache_creation = cache_creation + excluded.cache_creation,
			cache_read = cache_read + excluded.cache_read,
			token_requests = token_requests + excluded.token_requests,
			last_request_at = MAX(last_request_at, excluded.last_request_at),
			latency_ms_sum = latency_ms_sum + excluded.latency_ms_sum,
			ttft_ms_sum = ttft_ms_sum + excluded.ttft_ms_sum,
			duration_ms_sum = duration_ms_sum + excluded.duration_ms_sum`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for key, delta := range deltas {
		if _, err := stmt.ExecContext(
			ctx,
			key.Provider,
			key.Model,
			minute,
			delta.Requests,
			delta.Failovers,
			delta.RateLimited429,
			delta.Failures,
			delta.Input,
			delta.Output,
			delta.CacheCreation,
			delta.CacheRead,
			delta.TokenRequests,
			delta.LastRequestAt,
			delta.LatencySum,
			delta.TTFTSum,
			delta.DurationSum,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// FlushAgents upserts agent/provider/model deltas into one minute bucket.
func (s *Store) FlushAgents(minute int64, deltas map[AgentKey]AgentCounters) error {
	return s.FlushAgentsContext(context.Background(), minute, deltas)
}

// FlushAgentsContext is FlushAgents with cancellation for lifecycle-bounded writes.
func (s *Store) FlushAgentsContext(
	ctx context.Context,
	minute int64,
	deltas map[AgentKey]AgentCounters,
) error {
	if len(deltas) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `INSERT INTO agent_buckets
		(agent, provider, model, minute, requests, input, output, cache_creation, cache_read, latency_ms_sum, ttft_ms_sum, duration_ms_sum, failures)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(agent, provider, model, minute) DO UPDATE SET
			requests = requests + excluded.requests,
			input = input + excluded.input,
			output = output + excluded.output,
			cache_creation = cache_creation + excluded.cache_creation,
			cache_read = cache_read + excluded.cache_read,
			latency_ms_sum = latency_ms_sum + excluded.latency_ms_sum,
			ttft_ms_sum = ttft_ms_sum + excluded.ttft_ms_sum,
			failures = failures + excluded.failures`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for key, delta := range deltas {
		if _, err := stmt.ExecContext(
			ctx,
			key.Agent,
			key.Provider,
			key.Model,
			minute,
			delta.Requests,
			delta.Input,
			delta.Output,
			delta.CacheCreation,
			delta.CacheRead,
			delta.LatencySum,
			delta.TTFTSum,
			delta.DurationSum,
			delta.Failures,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Prune removes provider/model and agent buckets older than the retention
// window. Retention <= 0 keeps history forever.
func (s *Store) Prune(now time.Time) error {
	return s.PruneContext(context.Background(), now)
}

// PruneContext is Prune with cancellation for lifecycle-bounded maintenance.
func (s *Store) PruneContext(ctx context.Context, now time.Time) error {
	if s.retention <= 0 {
		return nil
	}
	cutoff := now.Add(-s.retention).Unix() / 60 * 60
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(
		ctx,
		`DELETE FROM minute_buckets WHERE minute < ?`,
		cutoff,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(
		ctx,
		`DELETE FROM agent_buckets WHERE minute < ?`,
		cutoff,
	); err != nil {
		return err
	}
	if _, err := tx.ExecContext(
		ctx,
		`DELETE FROM mcp_buckets WHERE minute < ?`,
		cutoff,
	); err != nil {
		return err
	}
	return tx.Commit()
}

// Reset removes all persisted provider/model and agent history.
func (s *Store) Reset() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM minute_buckets`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM agent_buckets`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM mcp_buckets`); err != nil {
		return err
	}
	return tx.Commit()
}
