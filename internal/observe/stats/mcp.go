package stats

import (
	"context"

	obscounters "model-proxy/internal/observe/counters"
)

// MCPKind tags an MCP counter name as either a configured server or a route.
type MCPKind string

const (
	MCPKindServer MCPKind = "server"
	MCPKindRoute  MCPKind = "route"
)

// MCPBucketDelta is one (name, kind, minute) delta produced by the stats flusher.
type MCPBucketDelta struct {
	Name         string
	Kind         MCPKind
	Calls        uint64
	Errors       uint64
	LatencyMsSum uint64
	LastCallAt   int64
}

// MCPBucketRow is one aggregated bucket returned by QueryMCPBuckets.
type MCPBucketRow struct {
	Name         string  `json:"name"`
	Kind         string  `json:"kind"`
	Bucket       int64   `json:"bucket"`
	Calls        uint64  `json:"calls"`
	Errors       uint64  `json:"errors"`
	LatencyMsSum uint64  `json:"latency_ms_sum"`
	LastCallAt   int64   `json:"last_call_at"`
	AvgLatencyMs float64 `json:"avg_latency_ms"`
}

// FlushMCPBuckets upserts per-name MCP deltas into one minute bucket.
func (s *Store) FlushMCPBuckets(minute int64, deltas []MCPBucketDelta) error {
	return s.FlushMCPBucketsContext(context.Background(), minute, deltas)
}

// FlushMCPBucketsContext is FlushMCPBuckets with cancellation for
// lifecycle-bounded writes.
func (s *Store) FlushMCPBucketsContext(
	ctx context.Context,
	minute int64,
	deltas []MCPBucketDelta,
) error {
	if len(deltas) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `INSERT INTO mcp_buckets
		(name, kind, minute, calls, errors, latency_ms_sum, last_call_at)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(name, kind, minute) DO UPDATE SET
			calls = calls + excluded.calls,
			errors = errors + excluded.errors,
			latency_ms_sum = latency_ms_sum + excluded.latency_ms_sum,
			last_call_at = MAX(last_call_at, excluded.last_call_at)`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, d := range deltas {
		if _, err := stmt.ExecContext(
			ctx,
			d.Name,
			string(d.Kind),
			minute,
			d.Calls,
			d.Errors,
			d.LatencyMsSum,
			d.LastCallAt,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// QueryMCPBuckets returns calendar-aligned aggregates over mcp_buckets for the
// inclusive [from, to] range. granularity must be one of minute, hour, day,
// week, or month and matches the local-time bucketing used by QueryAnalytics.
func (s *Store) QueryMCPBuckets(from, to int64, granularity string) ([]MCPBucketRow, error) {
	return s.QueryMCPBucketsContext(context.Background(), from, to, granularity)
}

// QueryMCPBucketsContext is QueryMCPBuckets with cancellation support.
func (s *Store) QueryMCPBucketsContext(
	ctx context.Context,
	from, to int64,
	granularity string,
) ([]MCPBucketRow, error) {
	labelExpr, labelArgs, layout, err := analyticsBucketing(granularity)
	if err != nil {
		return nil, err
	}

	query := `SELECT name, kind,
		` + labelExpr + ` AS d,
		SUM(calls), SUM(errors), SUM(latency_ms_sum), MAX(last_call_at)
		FROM mcp_buckets WHERE minute >= ? AND minute <= ?`
	args := append([]any{}, labelArgs...)
	args = append(args, from, to)
	query += ` GROUP BY name, kind, ` + labelExpr + ` ORDER BY name, kind, d`
	args = append(args, labelArgs...)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var buckets []MCPBucketRow
	for rows.Next() {
		var (
			row  MCPBucketRow
			date string
		)
		if err := rows.Scan(
			&row.Name,
			&row.Kind,
			&date,
			&row.Calls,
			&row.Errors,
			&row.LatencyMsSum,
			&row.LastCallAt,
		); err != nil {
			return nil, err
		}
		row.Bucket = localCalendarStart(date, layout, granularity)
		row.AvgLatencyMs = averageMilliseconds(row.LatencyMsSum, row.Calls)
		buckets = append(buckets, row)
	}
	return buckets, rows.Err()
}

// mcpDeltaField returns the delta between current and previous counters. If the
// counter reset (current < previous), current is treated as the delta so the
// post-reset activity is still recorded.
func mcpDeltaField(current, previous uint64) uint64 {
	if current < previous {
		return current
	}
	return current - previous
}

// DiffMCP turns two cumulative MCP snapshots into per-name deltas. Names whose
// kind cannot be resolved are skipped. LastCallAt is carried as the cumulative
// current value (Store merges it with MAX).
func DiffMCP(
	current, previous map[string]obscounters.MCPStatRaw,
	kind func(string) (MCPKind, bool),
) []MCPBucketDelta {
	deltas := []MCPBucketDelta{}
	for name, cur := range current {
		k, ok := kind(name)
		if !ok {
			continue
		}
		prev := previous[name]
		delta := MCPBucketDelta{
			Name:         name,
			Kind:         k,
			Calls:        mcpDeltaField(cur.Calls, prev.Calls),
			Errors:       mcpDeltaField(cur.Errors, prev.Errors),
			LatencyMsSum: mcpDeltaField(cur.LatencySum, prev.LatencySum),
			LastCallAt:   cur.LastCallAt,
		}
		if delta.Calls == 0 && delta.Errors == 0 && delta.LatencyMsSum == 0 {
			continue
		}
		deltas = append(deltas, delta)
	}
	return deltas
}
