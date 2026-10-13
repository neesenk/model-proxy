package stats

import (
	"context"
	"sort"
	"time"

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
	layout, err := analyticsLayout(granularity)
	if err != nil {
		return nil, err
	}

	// Raw PK rows (upserts already accumulated in place); calendar folding in
	// Go — see queryAnalyticsIn.
	query := `SELECT name, kind, minute,
		calls, errors, latency_ms_sum, last_call_at
		FROM mcp_buckets WHERE minute >= ? AND minute <= ?`

	rows, err := s.db.QueryContext(ctx, query, from, to)
	if err != nil {
		return nil, err
	}
	type groupKey struct {
		name, kind, label string
	}
	groups := map[groupKey]*MCPBucketRow{}
	labeler := minuteLabeler{granularity: granularity, loc: time.Local}
	for rows.Next() {
		var (
			row    MCPBucketRow
			minute int64
		)
		if err := rows.Scan(
			&row.Name, &row.Kind, &minute,
			&row.Calls, &row.Errors, &row.LatencyMsSum, &row.LastCallAt,
		); err != nil {
			_ = rows.Close()
			return nil, err
		}
		key := groupKey{row.Name, row.Kind, labeler.bucketLabel(minute)}
		if dst, ok := groups[key]; ok {
			mergeMCPBucketRow(dst, &row)
		} else {
			groups[key] = &row
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	ordered := make([]groupKey, 0, len(groups))
	for key := range groups {
		ordered = append(ordered, key)
	}
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.name != b.name {
			return a.name < b.name
		}
		if a.kind != b.kind {
			return a.kind < b.kind
		}
		return a.label < b.label
	})
	var buckets []MCPBucketRow
	for _, key := range ordered {
		row := groups[key]
		row.Bucket = localCalendarStart(key.label, layout, granularity)
		row.AvgLatencyMs = averageMilliseconds(row.LatencyMsSum, row.Calls)
		buckets = append(buckets, *row)
	}
	return buckets, nil
}

// mergeMCPBucketRow folds one (name, kind, minute) partial row into its
// calendar bucket: counters add, LastCallAt keeps the max.
func mergeMCPBucketRow(dst, src *MCPBucketRow) {
	dst.Calls += src.Calls
	dst.Errors += src.Errors
	dst.LatencyMsSum += src.LatencyMsSum
	if src.LastCallAt > dst.LastCallAt {
		dst.LastCallAt = src.LastCallAt
	}
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

// MCPToolBucketDelta is one (name, tool, minute) delta produced by the stats
// flusher. The tool name is the client-facing one (canonical for route
// exchanges); kind is resolved from the exposed name exactly like DiffMCP.
type MCPToolBucketDelta struct {
	Name         string
	Tool         string
	Kind         MCPKind
	Calls        uint64
	Errors       uint64
	LatencyMsSum uint64
	LastCallAt   int64
}

// MCPToolBucketRow is one aggregated (name, tool) bucket returned by
// QueryMCPToolBuckets.
type MCPToolBucketRow struct {
	Name         string  `json:"name"`
	Tool         string  `json:"tool"`
	Kind         string  `json:"kind"`
	Bucket       int64   `json:"bucket"`
	Calls        uint64  `json:"calls"`
	Errors       uint64  `json:"errors"`
	LatencyMsSum uint64  `json:"latency_ms_sum"`
	LastCallAt   int64   `json:"last_call_at"`
	AvgLatencyMs float64 `json:"avg_latency_ms"`
}

// FlushMCPToolBuckets upserts per-(name, tool) MCP deltas into one minute
// bucket.
func (s *Store) FlushMCPToolBuckets(minute int64, deltas []MCPToolBucketDelta) error {
	return s.FlushMCPToolBucketsContext(context.Background(), minute, deltas)
}

// FlushMCPToolBucketsContext is FlushMCPToolBuckets with cancellation for
// lifecycle-bounded writes.
func (s *Store) FlushMCPToolBucketsContext(
	ctx context.Context,
	minute int64,
	deltas []MCPToolBucketDelta,
) error {
	if len(deltas) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `INSERT INTO mcp_tool_buckets
		(name, tool, kind, minute, calls, errors, latency_ms_sum, last_call_at)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(name, tool, minute) DO UPDATE SET
			kind = excluded.kind,
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
			d.Tool,
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

// QueryMCPToolBuckets returns calendar-aligned (name, tool) aggregates over
// mcp_tool_buckets for the inclusive [from, to] range. granularity matches the
// local-time bucketing used by QueryMCPBuckets.
func (s *Store) QueryMCPToolBuckets(from, to int64, granularity string) ([]MCPToolBucketRow, error) {
	return s.QueryMCPToolBucketsContext(context.Background(), from, to, granularity)
}

// QueryMCPToolBucketsContext is QueryMCPToolBuckets with cancellation support.
func (s *Store) QueryMCPToolBucketsContext(
	ctx context.Context,
	from, to int64,
	granularity string,
) ([]MCPToolBucketRow, error) {
	layout, err := analyticsLayout(granularity)
	if err != nil {
		return nil, err
	}

	// Raw PK rows (upserts already accumulated in place); calendar folding in
	// Go — see queryAnalyticsIn.
	query := `SELECT name, tool, kind, minute,
		calls, errors, latency_ms_sum, last_call_at
		FROM mcp_tool_buckets WHERE minute >= ? AND minute <= ?`

	rows, err := s.db.QueryContext(ctx, query, from, to)
	if err != nil {
		return nil, err
	}
	type groupKey struct {
		name, tool, kind, label string
	}
	groups := map[groupKey]*MCPToolBucketRow{}
	labeler := minuteLabeler{granularity: granularity, loc: time.Local}
	for rows.Next() {
		var (
			row    MCPToolBucketRow
			minute int64
		)
		if err := rows.Scan(
			&row.Name, &row.Tool, &row.Kind, &minute,
			&row.Calls, &row.Errors, &row.LatencyMsSum, &row.LastCallAt,
		); err != nil {
			_ = rows.Close()
			return nil, err
		}
		key := groupKey{row.Name, row.Tool, row.Kind, labeler.bucketLabel(minute)}
		if dst, ok := groups[key]; ok {
			mergeMCPToolBucketRow(dst, &row)
		} else {
			groups[key] = &row
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	_ = rows.Close()

	ordered := make([]groupKey, 0, len(groups))
	for key := range groups {
		ordered = append(ordered, key)
	}
	sort.Slice(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if a.name != b.name {
			return a.name < b.name
		}
		if a.tool != b.tool {
			return a.tool < b.tool
		}
		if a.kind != b.kind {
			return a.kind < b.kind
		}
		return a.label < b.label
	})
	var buckets []MCPToolBucketRow
	for _, key := range ordered {
		row := groups[key]
		row.Bucket = localCalendarStart(key.label, layout, granularity)
		row.AvgLatencyMs = averageMilliseconds(row.LatencyMsSum, row.Calls)
		buckets = append(buckets, *row)
	}
	return buckets, nil
}

// mergeMCPToolBucketRow folds one (name, tool, kind, minute) partial row into
// its calendar bucket: counters add, LastCallAt keeps the max.
func mergeMCPToolBucketRow(dst, src *MCPToolBucketRow) {
	dst.Calls += src.Calls
	dst.Errors += src.Errors
	dst.LatencyMsSum += src.LatencyMsSum
	if src.LastCallAt > dst.LastCallAt {
		dst.LastCallAt = src.LastCallAt
	}
}

// DiffMCPTools turns two cumulative (name, tool) snapshots into deltas. Names
// whose kind cannot be resolved are skipped; LastCallAt is carried as the
// cumulative current value (Store merges it with MAX).
func DiffMCPTools(
	current, previous map[obscounters.MCPToolKey]obscounters.MCPStatRaw,
	kind func(string) (MCPKind, bool),
) []MCPToolBucketDelta {
	deltas := []MCPToolBucketDelta{}
	for key, cur := range current {
		k, ok := kind(key.Name)
		if !ok {
			continue
		}
		prev := previous[key]
		delta := MCPToolBucketDelta{
			Name:         key.Name,
			Tool:         key.Tool,
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
