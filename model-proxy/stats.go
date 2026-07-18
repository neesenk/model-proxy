package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver; registers as "sqlite" with database/sql
)

// statsCounters is the unified per-(provider,model) cumulative counter set,
// merging metricsStore (requests/failovers/429/failures/last_request_at) and
// tokenCounter (input/output/cache/token_requests). It is both the in-memory
// cumulative snapshot the flusher diffs against and the shape persisted per
// minute-bucket in SQLite.
type statsCounters struct {
	Requests       uint64
	Failovers      uint64
	RateLimited429 uint64
	Failures       uint64
	Input          uint64
	Output         uint64
	CacheCreation  uint64
	CacheRead      uint64
	TokenRequests  uint64
	LastRequestAt  int64
	// LatencySum/TTFTSum: cumulative ms over committed responses (avg = sum /
	// requests at query time). Additive counters, like the token sums.
	LatencySum uint64
	TTFTSum    uint64
}

// statsBucket is one persisted row: a (provider, model, minute) + the counter
// deltas observed in that minute. Returned by queryRange for /api/stats and the
// `stats` CLI.
type statsBucket struct {
	Provider       string `json:"provider"`
	Model          string `json:"model"`
	Minute         int64  `json:"minute"` // unix seconds, floored to 60
	Requests       uint64 `json:"requests"`
	Failovers      uint64 `json:"failovers"`
	RateLimited429 uint64 `json:"rate_limited_429"`
	Failures       uint64 `json:"failures"`
	Input          uint64 `json:"input"`
	Output         uint64 `json:"output"`
	CacheCreation  uint64 `json:"cache_creation"`
	CacheRead      uint64 `json:"cache_read"`
	TokenRequests  uint64 `json:"token_requests"`
	LastRequestAt  int64  `json:"last_request_at"`
	// LatencySum/TTFTSum are the raw additive sums (what is stored); the derived
	// Avg* fields are filled in at query time (SUM / requests, 0 when no reqs).
	LatencySum   uint64  `json:"latency_ms_sum"`
	TTFTSum      uint64  `json:"ttft_ms_sum"`
	AvgLatencyMs float64 `json:"avg_latency_ms"`
	AvgTtftMs    float64 `json:"avg_ttft_ms"`
}

// statsStore is the SQLite persistence layer for call statistics. It owns one
// table (minute_buckets) holding per-(provider,model,minute) deltas. The hot
// path never touches it: in-memory accumulators (metricsStore + tokenCounter)
// are diffed by the statsFlusher once per minute and upserted here.
//
// database/sql is goroutine-safe, so statsStore needs no lock of its own. The
// flusher is single-goroutine; query handlers (handleStats) are read-only
// SELECTs. SQLite serializes writes via the single connection (SetMaxOpenConns
// 1), so there is no write contention to manage.
type statsStore struct {
	db        *sql.DB
	retention time.Duration // 0 = keep forever
}

const statsDriver = "sqlite"

// openStatsStore opens (creating if absent) the SQLite stats DB, applies PRAGMAs
// + schema, and returns it. retention bounds history (0 = forever).
func openStatsStore(path string, retention time.Duration) (*statsStore, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("stats db dir: %w", err)
		}
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open(statsDriver, dsn)
	if err != nil {
		return nil, fmt.Errorf("open stats db: %w", err)
	}
	// SQLite serializes writes; a single connection avoids "database is locked"
	// contention and matches the single-writer (flusher) access pattern.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping stats db: %w", err)
	}
	s := &statsStore{db: db, retention: retention}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *statsStore) migrate() error {
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
	PRIMARY KEY (agent, provider, model, minute)
);
CREATE INDEX IF NOT EXISTS idx_agent_minute ON agent_buckets(minute);`)
	if err != nil {
		return fmt.Errorf("migrate stats schema: %w", err)
	}
	// Additive migration for DBs created before latency columns existed: CREATE
	// TABLE IF NOT EXISTS only shapes a fresh DB, so ALTER missing columns in
	// place. Harmless on a fresh DB (columns already present → skipped).
	if err := s.ensureColumns("minute_buckets", [][2]string{
		{"latency_ms_sum", "INTEGER NOT NULL DEFAULT 0"},
		{"ttft_ms_sum", "INTEGER NOT NULL DEFAULT 0"},
	}); err != nil {
		return fmt.Errorf("migrate stats schema: %w", err)
	}
	return nil
}

// ensureColumns adds any (column, type-def) pairs missing from table via ALTER
// TABLE ADD COLUMN. Used for additive migrations on pre-existing DBs (CREATE
// TABLE IF NOT EXISTS does not add columns to an existing table). A column that
// already exists is skipped.
func (s *statsStore) ensureColumns(table string, cols [][2]string) error {
	present, err := s.columnSet(table)
	if err != nil {
		return err
	}
	for _, c := range cols {
		if present[c[0]] {
			continue
		}
		if _, err := s.db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, c[0], c[1])); err != nil {
			return fmt.Errorf("add %s.%s: %w", table, c[0], err)
		}
	}
	return nil
}

// columnSet returns the set of column names currently on a table (PRAGMA
// table_info). Names are lower-cased by SQLite, so keys are matched as-is.
func (s *statsStore) columnSet(table string) (map[string]bool, error) {
	rows, err := s.db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var cid int
		var name, typ string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

func (s *statsStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// isEmpty reports whether the bucket table has no rows (used to gate one-time
// JSON migration so it only runs on a fresh DB).
func (s *statsStore) isEmpty() (bool, error) {
	var n int64
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM minute_buckets`).Scan(&n); err != nil {
		return false, err
	}
	return n == 0, nil
}

// migrateFromTokensJSON imports the legacy ~/.model-proxy/token_usage.json
// (flat map["provider\x00model"]tokenUsage) as a single baseline bucket so prior
// token totals survive the switch to SQLite. Runs only when the DB is empty and
// the JSON file exists. The JSON file is left in place (never deletes user
// data). Returns the imported key count.
func (s *statsStore) migrateFromTokensJSON(path string) (int, error) {
	empty, err := s.isEmpty()
	if err != nil {
		return 0, err
	}
	if !empty {
		return 0, nil // already has data; don't re-import
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil // nothing to migrate
		}
		return 0, err
	}
	var flat map[string]tokenUsage
	if err := json.Unmarshal(data, &flat); err != nil {
		return 0, fmt.Errorf("parse legacy token_usage.json: %w", err)
	}
	// Bucket minute = the file's mtime floored to a minute, so the imported
	// totals are attributed to when they were last persisted (pre-history).
	minute := int64(0)
	if fi, err := os.Stat(path); err == nil {
		minute = fi.ModTime().Unix() / 60 * 60
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	stmt, err := tx.Prepare(`INSERT INTO minute_buckets
		(provider, model, minute, input, output, cache_creation, cache_read, token_requests)
		VALUES (?,?,?,?,?,?,?,?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	n := 0
	for k, u := range flat {
		prov, mod := splitLegacyKey(k)
		if _, err := stmt.Exec(prov, mod, minute, u.Input, u.Output, u.CacheCreation, u.CacheRead, u.Requests); err != nil {
			return n, err
		}
		n++
	}
	if err := tx.Commit(); err != nil {
		return n, err
	}
	return n, nil
}

// splitLegacyKey splits a "provider\x00model" key from the old JSON format.
func splitLegacyKey(k string) (string, string) {
	for i := 0; i < len(k); i++ {
		if k[i] == 0 {
			return k[:i], k[i+1:]
		}
	}
	return k, ""
}

// loadCumulative returns the all-time per-(provider,model) totals (SUM across
// all minute buckets) + MAX(last_request_at). Used at boot to seed the
// in-memory accumulators so metrics/tokens survive a restart.
func (s *statsStore) loadCumulative() (map[pmKey]statsCounters, error) {
	rows, err := s.db.Query(`SELECT provider, model,
		SUM(requests), SUM(failovers), SUM(rate_limited_429), SUM(failures),
		SUM(input), SUM(output), SUM(cache_creation), SUM(cache_read), SUM(token_requests),
		MAX(last_request_at), SUM(latency_ms_sum), SUM(ttft_ms_sum)
		FROM minute_buckets GROUP BY provider, model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[pmKey]statsCounters{}
	for rows.Next() {
		var k pmKey
		var c statsCounters
		if err := rows.Scan(&k.Provider, &k.Model,
			&c.Requests, &c.Failovers, &c.RateLimited429, &c.Failures,
			&c.Input, &c.Output, &c.CacheCreation, &c.CacheRead, &c.TokenRequests,
			&c.LastRequestAt, &c.LatencySum, &c.TTFTSum); err != nil {
			return nil, err
		}
		out[k] = c
	}
	return out, rows.Err()
}

// flushDeltas upserts non-zero per-minute deltas. last_request_at is merged as
// MAX (it's a timestamp, not a counter); all other columns are additive.
func (s *statsStore) flushDeltas(minute int64, deltas map[pmKey]statsCounters) error {
	if len(deltas) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	stmt, err := tx.Prepare(`INSERT INTO minute_buckets
		(provider, model, minute, requests, failovers, rate_limited_429, failures,
		 input, output, cache_creation, cache_read, token_requests, last_request_at,
		 latency_ms_sum, ttft_ms_sum)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
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
			ttft_ms_sum = ttft_ms_sum + excluded.ttft_ms_sum`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for k, d := range deltas {
		if _, err := stmt.Exec(k.Provider, k.Model, minute,
			d.Requests, d.Failovers, d.RateLimited429, d.Failures,
			d.Input, d.Output, d.CacheCreation, d.CacheRead, d.TokenRequests,
			d.LastRequestAt, d.LatencySum, d.TTFTSum); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// prune deletes buckets older than the retention window. No-op if retention is
// 0 (keep forever). Run on each flush tick.
func (s *statsStore) prune(now time.Time) error {
	if s.retention <= 0 {
		return nil
	}
	cutoff := now.Add(-s.retention).Unix() / 60 * 60
	_, err := s.db.Exec(`DELETE FROM minute_buckets WHERE minute < ?`, cutoff)
	return err
}

// resetAll deletes all bucket history. Called together with the in-memory
// resets by Proxy.resetStats so "reset counters" zeroes both layers.
func (s *statsStore) resetAll() error {
	if _, err := s.db.Exec(`DELETE FROM minute_buckets`); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM agent_buckets`)
	return err
}

// queryRange returns bucket rows in [from, to] (unix seconds, inclusive),
// optionally filtered by provider and/or model. bucketSecs controls the display
// granularity:
//   - bucketSecs <= 60: return raw 1-minute rows as stored (the default; zero
//     behavior change).
//   - bucketSecs > 60:  GROUP the 1-minute rows into wider display buckets
//     (SUM of counters, MAX of last_request_at), so a 10m/1h view returns 1/10
//     or 1/60 the rows. Storage stays 1-minute (lossless); only the view widens.
//
// bucketSecs must be a multiple of 60 (enforced by normalizeBucket) so bucket
// boundaries align to minute edges. Ordered by provider, model, minute.
func (s *statsStore) queryRange(from, to int64, provider, model string, bucketSecs int64) ([]statsBucket, error) {
	if bucketSecs <= 60 {
		// Raw 1-minute rows: the original path, unchanged shape/order.
		q := `SELECT provider, model, minute, requests, failovers, rate_limited_429, failures,
			input, output, cache_creation, cache_read, token_requests, last_request_at,
			latency_ms_sum, ttft_ms_sum
			FROM minute_buckets WHERE minute >= ? AND minute <= ?`
		args := []any{from, to}
		if provider != "" {
			q += ` AND provider = ?`
			args = append(args, provider)
		}
		if model != "" {
			q += ` AND model = ?`
			args = append(args, model)
		}
		q += ` ORDER BY provider, model, minute`
		rows, err := s.db.Query(q, args...)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []statsBucket
		for rows.Next() {
			var b statsBucket
			if err := rows.Scan(&b.Provider, &b.Model, &b.Minute,
				&b.Requests, &b.Failovers, &b.RateLimited429, &b.Failures,
				&b.Input, &b.Output, &b.CacheCreation, &b.CacheRead, &b.TokenRequests,
				&b.LastRequestAt, &b.LatencySum, &b.TTFTSum); err != nil {
				return nil, err
			}
			b.AvgLatencyMs = avgMs(b.LatencySum, b.Requests)
			b.AvgTtftMs = avgMs(b.TTFTSum, b.Requests)
			out = append(out, b)
		}
		return out, rows.Err()
	}
	// Aggregated display bucket: floor each minute to its bucket start, SUM the
	// counters, MAX the timestamp. (minute / B) * B is integer floor; B is a
	// multiple of 60 so buckets align to minute edges.
	q := `SELECT provider, model,
		(minute / ?) * ? AS minute,
		SUM(requests), SUM(failovers), SUM(rate_limited_429), SUM(failures),
		SUM(input), SUM(output), SUM(cache_creation), SUM(cache_read), SUM(token_requests),
		MAX(last_request_at), SUM(latency_ms_sum), SUM(ttft_ms_sum)
		FROM minute_buckets WHERE minute >= ? AND minute <= ?`
	args := []any{bucketSecs, bucketSecs, from, to}
	if provider != "" {
		q += ` AND provider = ?`
		args = append(args, provider)
	}
	if model != "" {
		q += ` AND model = ?`
		args = append(args, model)
	}
	q += ` GROUP BY provider, model, (minute / ?) * ? ORDER BY provider, model, minute`
	args = append(args, bucketSecs, bucketSecs)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []statsBucket
	for rows.Next() {
		var b statsBucket
		if err := rows.Scan(&b.Provider, &b.Model, &b.Minute,
			&b.Requests, &b.Failovers, &b.RateLimited429, &b.Failures,
			&b.Input, &b.Output, &b.CacheCreation, &b.CacheRead, &b.TokenRequests,
			&b.LastRequestAt, &b.LatencySum, &b.TTFTSum); err != nil {
			return nil, err
		}
		b.AvgLatencyMs = avgMs(b.LatencySum, b.Requests)
		b.AvgTtftMs = avgMs(b.TTFTSum, b.Requests)
		out = append(out, b)
	}
	return out, rows.Err()
}

// avgMs returns the per-request average of a millisecond sum (0 when there are
// no requests, instead of NaN). Rounded to 1 decimal place — latencies are
// observability, not billing.
func avgMs(sum, requests uint64) float64 {
	if requests == 0 {
		return 0
	}
	v := float64(sum) / float64(requests)
	return math.Round(v*10) / 10
}

// analyticsBucket is one persisted calendar-day/month aggregate row for the
// analytics dashboard. Bucket = unix start of the local calendar day/month.
type analyticsBucket struct {
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	Bucket        int64  `json:"bucket"`
	Requests      uint64 `json:"requests"`
	Input         uint64 `json:"input"`
	Output        uint64 `json:"output"`
	CacheCreation uint64 `json:"cache_creation"`
	CacheRead     uint64 `json:"cache_read"`
	LastRequestAt int64  `json:"last_request_at"`
}

// queryAnalytics returns calendar-day or calendar-month aggregates (local tz)
// in [from, to]. Storage stays 1-minute (lossless); only the view widens via
// SQL GROUP BY on date(minute,'unixepoch','localtime',<trunc>). The bucket is
// the true local-midnight/first-of-month unix instant: SQL returns the local
// date STRING, Go converts it via time.ParseInLocation(...,time.Local) (not
// strftime('%s',…), which would misread the local date as UTC midnight and shift
// the label by the tz offset). granularity must be "day" or "month". Ordered by
// provider, model, date.
func (s *statsStore) queryAnalytics(from, to int64, provider, model, granularity string) ([]analyticsBucket, error) {
	if granularity != "day" && granularity != "month" {
		return nil, fmt.Errorf("granularity must be day or month, got %q", granularity)
	}
	trunc := "start of day"
	if granularity == "month" {
		trunc = "start of month"
	}
	q := `SELECT provider, model,
		date(minute,'unixepoch','localtime',?) AS d,
		SUM(requests), SUM(input), SUM(output), SUM(cache_creation), SUM(cache_read),
		MAX(last_request_at)
		FROM minute_buckets WHERE minute >= ? AND minute <= ?`
	args := []any{trunc, from, to}
	if provider != "" {
		q += ` AND provider = ?`
		args = append(args, provider)
	}
	if model != "" {
		q += ` AND model = ?`
		args = append(args, model)
	}
	q += ` GROUP BY provider, model, date(minute,'unixepoch','localtime',?) ORDER BY provider, model, d`
	args = append(args, trunc)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []analyticsBucket
	for rows.Next() {
		var b analyticsBucket
		var d string
		if err := rows.Scan(&b.Provider, &b.Model, &d,
			&b.Requests, &b.Input, &b.Output, &b.CacheCreation, &b.CacheRead,
			&b.LastRequestAt); err != nil {
			return nil, err
		}
		b.Bucket = localDayStart(d, granularity)
		out = append(out, b)
	}
	return out, rows.Err()
}

// localDayStart converts a SQLite local date string to the unix start of that
// local calendar day/month, in the host timezone (mirrors SQLite 'localtime').
// SQLite date() always returns "YYYY-MM-DD" even with 'start of month' (which
// truncates to day 1 but does not shorten the string), so for month we slice
// the year-month prefix before parsing. 0 on a parse failure.
func localDayStart(d, granularity string) int64 {
	layout := "2006-01-02"
	if granularity == "month" {
		d = d[:7]
		layout = "2006-01"
	}
	t, err := time.ParseInLocation(layout, d, time.Local)
	if err != nil {
		return 0
	}
	return t.Unix()
}

// agentBucket is one persisted agent-dimension row: a (agent, provider, model,
// minute) + that minute's request count and observed input/output tokens.
// Returned by queryAgentRange for /api/agents and the `stats --by-agent` CLI.
type agentBucket struct {
	Agent    string `json:"agent"`
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Minute   int64  `json:"minute"`
	Requests uint64 `json:"requests"`
	Input    uint64 `json:"input"`
	Output   uint64 `json:"output"`
}

// flushAgentDeltas upserts per-minute agent deltas (additive counters). Mirrors
// flushDeltas but for the parallel agent_buckets table.
func (s *statsStore) flushAgentDeltas(minute int64, deltas map[agentKey]agentCount) error {
	if len(deltas) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	stmt, err := tx.Prepare(`INSERT INTO agent_buckets
		(agent, provider, model, minute, requests, input, output)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(agent, provider, model, minute) DO UPDATE SET
			requests = requests + excluded.requests,
			input = input + excluded.input,
			output = output + excluded.output`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for k, d := range deltas {
		if _, err := stmt.Exec(k.Agent, k.Provider, k.Model, minute, d.Requests, d.Input, d.Output); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// queryAgentRange returns agent-dimension buckets in [from, to]. Optional agent/
// provider/model filters. bucketSecs widens the display bucket exactly like
// queryRange (1-minute storage is lossless). Ordered by agent, provider, model,
// minute.
func (s *statsStore) queryAgentRange(from, to int64, agent, provider, model string, bucketSecs int64) ([]agentBucket, error) {
	selectCols := "agent, provider, model, minute, requests, input, output"
	groupCols := ""
	if bucketSecs > 60 {
		selectCols = "agent, provider, model, (minute / ?) * ? AS minute, SUM(requests), SUM(input), SUM(output)"
		groupCols = ", (minute / ?) * ?"
	}
	q := "SELECT " + selectCols + " FROM agent_buckets WHERE minute >= ? AND minute <= ?"
	args := []any{}
	if bucketSecs > 60 {
		args = append(args, bucketSecs, bucketSecs)
	}
	args = append(args, from, to)
	if agent != "" {
		q += " AND agent = ?"
		args = append(args, agent)
	}
	if provider != "" {
		q += " AND provider = ?"
		args = append(args, provider)
	}
	if model != "" {
		q += " AND model = ?"
		args = append(args, model)
	}
	q += " GROUP BY agent, provider, model" + groupCols + " ORDER BY agent, provider, model, minute"
	if bucketSecs > 60 {
		args = append(args, bucketSecs, bucketSecs)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []agentBucket
	for rows.Next() {
		var b agentBucket
		if err := rows.Scan(&b.Agent, &b.Provider, &b.Model, &b.Minute, &b.Requests, &b.Input, &b.Output); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// pruneAgent deletes agent buckets older than the retention window (no-op when
// retention is 0). Run alongside the minute-bucket prune.
func (s *statsStore) pruneAgent(now time.Time) error {
	if s.retention <= 0 {
		return nil
	}
	cutoff := now.Add(-s.retention).Unix() / 60 * 60
	_, err := s.db.Exec(`DELETE FROM agent_buckets WHERE minute < ?`, cutoff)
	return err
}

// normalizeBucket parses a bucket-granularity spec into seconds, a multiple of
// 60. Accepted forms: "1m"/"10m"/"1h"/"1d" (time.Duration), a bare integer
// (seconds), "" or "0" (-> 60, raw 1-minute rows). Values < 60 clamp up to 60
// (we don't sub-minute); non-multiples of 60 round up to the next multiple so
// bucket edges align to minute boundaries.
func normalizeBucket(s string) int64 {
	if s == "" || s == "0" {
		return 60
	}
	var secs int64
	if d, err := time.ParseDuration(s); err == nil {
		secs = int64(d.Seconds())
	} else if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		secs = n
	} else {
		return 60 // unparseable -> default 1-minute rows
	}
	if secs <= 60 {
		return 60
	}
	// Round up to the next multiple of 60 so bucket edges land on minute marks.
	if r := secs % 60; r != 0 {
		secs += 60 - r
	}
	return secs
}

// --- flusher: diffs in-memory accumulators into SQLite once per minute ---

// statsFlusher owns the per-minute diff loop. It snapshots the in-memory
// metricsStore + tokenCounter, diffs against the previous snapshot, and upserts
// the deltas as a minute bucket. prev is seeded at boot from loadCumulative so
// the first flush writes only post-boot deltas (no double-count).
type statsFlusher struct {
	stats   *statsStore
	metrics *metricsStore
	tokens  *tokenCounter
	agents  *agentCounter

	mu         sync.Mutex
	prev       map[pmKey]statsCounters
	agentPrev  map[agentKey]agentCount
	lastBucket int64 // last minute written; guards against jitter double-writes
}

func newStatsFlusher(stats *statsStore, metrics *metricsStore, tokens *tokenCounter, agents *agentCounter, baseline map[pmKey]statsCounters) *statsFlusher {
	return &statsFlusher{stats: stats, metrics: metrics, tokens: tokens, agents: agents, prev: baseline}
}

// collect merges the current metrics + token snapshots into one cumulative map.
// Keys present in only one store (e.g. a failed request has metrics but no
// tokens) are merged with zero for the missing side.
func (f *statsFlusher) collect() map[pmKey]statsCounters {
	out := map[pmKey]statsCounters{}
	for k, m := range f.metrics.snapshot() {
		c := out[k]
		c.Requests = m.Requests
		c.Failovers = m.Failovers
		c.RateLimited429 = m.RateLimited429
		c.Failures = m.Failures
		c.LastRequestAt = m.LastRequestAt
		c.LatencySum = m.LatencySum
		c.TTFTSum = m.TTFTSum
		out[k] = c
	}
	for k, u := range f.tokens.snapshot() {
		c := out[k]
		c.Input = u.Input
		c.Output = u.Output
		c.CacheCreation = u.CacheCreation
		c.CacheRead = u.CacheRead
		c.TokenRequests = u.Requests
		out[k] = c
	}
	return out
}

// diffCounters returns the per-key deltas (cur - prev), clamped at 0 so a
// reset-between-snapshot race can't produce negative counters. last_request_at
// is carried as the cumulative max (the upsert merges via MAX, so re-passing an
// unchanged value is a harmless no-op). Keys with no counter change are omitted.
func diffCounters(cur, prev map[pmKey]statsCounters) map[pmKey]statsCounters {
	out := map[pmKey]statsCounters{}
	for k, c := range cur {
		p := prev[k]
		d := statsCounters{
			Requests:       sub(c.Requests, p.Requests),
			Failovers:      sub(c.Failovers, p.Failovers),
			RateLimited429: sub(c.RateLimited429, p.RateLimited429),
			Failures:       sub(c.Failures, p.Failures),
			Input:          sub(c.Input, p.Input),
			Output:         sub(c.Output, p.Output),
			CacheCreation:  sub(c.CacheCreation, p.CacheCreation),
			CacheRead:      sub(c.CacheRead, p.CacheRead),
			TokenRequests:  sub(c.TokenRequests, p.TokenRequests),
			LastRequestAt:  c.LastRequestAt,
			LatencySum:     sub(c.LatencySum, p.LatencySum),
			TTFTSum:        sub(c.TTFTSum, p.TTFTSum),
		}
		if d.Requests == 0 && d.Failovers == 0 && d.RateLimited429 == 0 && d.Failures == 0 &&
			d.Input == 0 && d.Output == 0 && d.CacheCreation == 0 && d.CacheRead == 0 && d.TokenRequests == 0 &&
			d.LatencySum == 0 && d.TTFTSum == 0 {
			continue
		}
		out[k] = d
	}
	return out
}

func sub(a, b uint64) uint64 {
	if a <= b {
		return 0
	}
	return a - b
}

// flush performs one diff+upsert+prune cycle. Returns whether it wrote.
//
// Holds f.mu for the ENTIRE cycle (collect → diff → DB write → prune) so that
// resetAll (the "reset counters" path) can't interleave: if reset ran between
// the diff and the DB write, stale deltas would land in the just-cleared DB.
// The lock is held during the SQLite write, but a flush writes ~20-200 rows in
// <1ms, and it runs once per minute — the contention with resetAll is negligible.
func (f *statsFlusher) flush(now time.Time) bool {
	f.mu.Lock()
	defer f.mu.Unlock()

	cur := f.collect()
	prev := f.prev
	f.prev = cur

	var agentDeltas map[agentKey]agentCount
	if f.agents != nil {
		ac := f.agents.snapshot()
		agentDeltas = diffAgent(ac, f.agentPrev)
		f.agentPrev = ac
	}

	deltas := diffCounters(cur, prev)
	if len(deltas) == 0 && len(agentDeltas) == 0 {
		return false
	}

	minute := now.Unix()/60*60 - 60
	if minute <= f.lastBucket {
		minute = f.lastBucket + 60
	}
	f.lastBucket = minute

	if len(deltas) > 0 {
		if err := f.stats.flushDeltas(minute, deltas); err != nil {
			log.Printf("[stats] flush failed: %v", err)
		}
	}
	if len(agentDeltas) > 0 {
		if err := f.stats.flushAgentDeltas(minute, agentDeltas); err != nil {
			log.Printf("[stats] agent flush failed: %v", err)
		}
	}
	if err := f.stats.prune(now); err != nil {
		log.Printf("[stats] prune failed: %v", err)
	}
	if err := f.stats.pruneAgent(now); err != nil {
		log.Printf("[stats] agent prune failed: %v", err)
	}
	return true
}

// resetAll performs the full stats reset (in-memory counters + SQLite history +
// the flusher's diff baseline) UNDER f.mu, preventing a concurrent flush from
// writing stale deltas to the just-cleared DB. Proxy.resetStats delegates here
// when a flusher exists; the non-flusher path (degenerate tests) resets directly.
func (f *statsFlusher) resetAll(p *Proxy) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p.metrics != nil {
		p.metrics.reset()
	}
	if p.tokens != nil {
		p.tokens.reset()
	}
	if p.agents != nil {
		p.agents.reset()
	}
	if p.cache != nil {
		p.cache.reset()
	}
	if f.stats != nil {
		if err := f.stats.resetAll(); err != nil {
			log.Printf("[stats] resetAll failed: %v", err)
		}
	}
	// Re-baseline prev to the now-zeroed counters so the next flush sees 0 delta.
	f.prev = f.collect()
	f.agentPrev = nil
	f.lastBucket = 0
}

// diffAgent returns per-key agent deltas (cur - prev), clamped at 0. Keys with no
// change are omitted. Mirrors diffCounters for the parallel agent pipeline.
func diffAgent(cur, prev map[agentKey]agentCount) map[agentKey]agentCount {
	out := map[agentKey]agentCount{}
	for k, c := range cur {
		p := prev[k]
		d := agentCount{
			Requests: sub(c.Requests, p.Requests),
			Input:    sub(c.Input, p.Input),
			Output:   sub(c.Output, p.Output),
		}
		if d.Requests == 0 && d.Input == 0 && d.Output == 0 {
			continue
		}
		out[k] = d
	}
	return out
}

// (resetPrev removed — superseded by resetAll which does the full reset under mu.)

// statsFlushLoop ticks at wall-clock minute boundaries, flushing per-minute
// deltas to SQLite. Nil-safe so a Proxy without a flusher (degenerate tests) is
// a no-op. Returns immediately if the flusher is nil.
func (p *Proxy) statsFlushLoop() {
	if p.flusher == nil {
		return
	}
	for {
		d := untilNextMinute(time.Now())
		time.Sleep(d)
		p.flusher.flush(time.Now())
	}
}

// untilNextMinute returns the duration to the next wall-clock minute boundary
// plus a small grace so the tick lands just past :00 (floorMinute(now)-60 then
// reliably refers to the completed minute). Minimum 50ms.
func untilNextMinute(now time.Time) time.Duration {
	next := now.Truncate(time.Minute).Add(time.Minute)
	d := time.Until(next) + 200*time.Millisecond
	if d < 50*time.Millisecond {
		d = 50 * time.Millisecond
	}
	return d
}

// legacyTokensPath is the pre-SQLite token-usage JSON file (~/.model-proxy/
// token_usage.json), imported once into SQLite on a fresh DB.
func legacyTokensPath() string {
	return filepath.Join(homeDir(), ".model-proxy", "token_usage.json")
}

// initStats opens the SQLite store, imports the legacy token_usage.json once,
// restores the cumulative baseline into the in-memory metrics + token counters
// (so totals survive restart), and wires the flusher. Best-effort: on failure it
// logs and leaves p.stats/p.flusher nil so the proxy runs without persisted
// stats (in-memory counters still function). Called from runProxy only - direct
// NewProxy callers (tests) stay in-memory and never touch ~/.model-proxy/.
func (p *Proxy) initStats(sc StatsConfig) {
	dbPath := sc.dbPath()
	ss, err := openStatsStore(dbPath, sc.retention())
	if err != nil {
		log.Printf("[stats] open failed (%s): %v - running without persisted stats", dbPath, err)
		return
	}
	p.stats = ss
	if n, err := ss.migrateFromTokensJSON(legacyTokensPath()); err != nil {
		log.Printf("[stats] legacy token_usage.json migration failed: %v", err)
	} else if n > 0 {
		log.Printf("[stats] imported %d entries from legacy token_usage.json", n)
	}
	baseline, err := ss.loadCumulative()
	if err != nil {
		log.Printf("[stats] load baseline failed: %v", err)
		baseline = map[pmKey]statsCounters{}
	}
	for k, c := range baseline {
		p.metrics.seed(k, providerMetricsSnapshot{
			Requests:       c.Requests,
			Failovers:      c.Failovers,
			RateLimited429: c.RateLimited429,
			Failures:       c.Failures,
			LastRequestAt:  c.LastRequestAt,
			LatencySum:     c.LatencySum,
			TTFTSum:        c.TTFTSum,
		})
		p.tokens.seed(k, tokenUsage{
			Input:         c.Input,
			Output:        c.Output,
			CacheCreation: c.CacheCreation,
			CacheRead:     c.CacheRead,
			Requests:      c.TokenRequests,
		})
	}
	p.flusher = newStatsFlusher(ss, p.metrics, p.tokens, p.agents, baseline)
}
