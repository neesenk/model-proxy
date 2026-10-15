package stats

import (
	"database/sql"
	"fmt"
	"math"
	"sort"
	"time"
)

// LoadCumulative returns all-time provider/model totals. Additive counters use
// SUM and LastRequestAt uses MAX across minute buckets.
func (s *Store) LoadCumulative() (map[Key]Counters, error) {
	return s.LoadCumulativeRange(0, 0)
}

// LoadCumulativeRange is LoadCumulative restricted to buckets with
// from <= minute <= to (either bound <= 0 means unbounded on that side).
// Buckets are minute-aligned: callers truncate the window start DOWN to the
// minute boundary so the whole boundary minute counts, and a to inside a
// minute includes that minute's bucket (bucket start <= to).
func (s *Store) LoadCumulativeRange(from, to int64) (map[Key]Counters, error) {
	if to <= 0 {
		to = math.MaxInt64
	}
	rows, err := s.db.Query(`SELECT provider, model,
		SUM(requests), SUM(failovers), SUM(rate_limited_429), SUM(failures),
		SUM(input), SUM(output), SUM(cache_creation), SUM(cache_read), SUM(token_requests),
		MAX(last_request_at), SUM(latency_ms_sum), SUM(ttft_ms_sum), SUM(duration_ms_sum)
		FROM minute_buckets WHERE minute >= ? AND minute <= ? GROUP BY provider, model`,
		max(from, 0), to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cumulative := map[Key]Counters{}
	for rows.Next() {
		var (
			key      Key
			counters Counters
		)
		if err := rows.Scan(
			&key.Provider,
			&key.Model,
			&counters.Requests,
			&counters.Failovers,
			&counters.RateLimited429,
			&counters.Failures,
			&counters.Input,
			&counters.Output,
			&counters.CacheCreation,
			&counters.CacheRead,
			&counters.TokenRequests,
			&counters.LastRequestAt,
			&counters.LatencySum,
			&counters.TTFTSum,
			&counters.DurationSum,
		); err != nil {
			return nil, err
		}
		cumulative[key] = counters
	}
	return cumulative, rows.Err()
}

// LoadCumulativeAgents returns all-time agent/provider/model totals — the
// agent-dimension counterpart of LoadCumulative, used to restore the hot
// AgentCounter and the flusher's agent diff baseline at boot so restarts
// don't zero the Agents card (and the first post-boot flush doesn't
// re-count history into agent_buckets).
func (s *Store) LoadCumulativeAgents() (map[AgentKey]AgentCounters, error) {
	return s.LoadCumulativeAgentsRange(0, 0)
}

// LoadCumulativeAgentsRange is LoadCumulativeAgents restricted to buckets with
// from <= minute <= to (either bound <= 0 unbounded) — the range counterpart
// used by the token-usage time selector.
func (s *Store) LoadCumulativeAgentsRange(from, to int64) (map[AgentKey]AgentCounters, error) {
	if to <= 0 {
		to = math.MaxInt64
	}
	rows, err := s.db.Query(`SELECT agent, provider, model,
		SUM(requests), SUM(input), SUM(output), SUM(cache_creation), SUM(cache_read),
		SUM(latency_ms_sum), SUM(ttft_ms_sum), SUM(duration_ms_sum), SUM(failures)
		FROM agent_buckets WHERE minute >= ? AND minute <= ? GROUP BY agent, provider, model`,
		max(from, 0), to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cumulative := map[AgentKey]AgentCounters{}
	for rows.Next() {
		var (
			key      AgentKey
			counters AgentCounters
		)
		if err := rows.Scan(
			&key.Agent,
			&key.Provider,
			&key.Model,
			&counters.Requests,
			&counters.Input,
			&counters.Output,
			&counters.CacheCreation,
			&counters.CacheRead,
			&counters.LatencySum,
			&counters.TTFTSum,
			&counters.DurationSum,
			&counters.Failures,
		); err != nil {
			return nil, err
		}
		cumulative[key] = counters
	}
	return cumulative, rows.Err()
}

// QueryRange returns provider/model rows in the inclusive [from, to] range.
// bucketSecs <= 60 returns raw minute rows. Wider buckets aggregate counter
// sums, retain MAX(last_request_at), and floor Minute to the bucket boundary.
func (s *Store) QueryRange(from, to int64, provider, model string, bucketSecs int64) ([]Bucket, error) {
	if bucketSecs <= 60 {
		query := `SELECT provider, model, minute, requests, failovers, rate_limited_429, failures,
			input, output, cache_creation, cache_read, token_requests, last_request_at,
			latency_ms_sum, ttft_ms_sum, duration_ms_sum
			FROM minute_buckets WHERE minute >= ? AND minute <= ?`
		args := []any{from, to}
		if provider != "" {
			query += ` AND provider = ?`
			args = append(args, provider)
		}
		if model != "" {
			query += ` AND model = ?`
			args = append(args, model)
		}
		query += ` ORDER BY provider, model, minute`
		return s.queryBuckets(query, args...)
	}

	query := `SELECT provider, model,
		(minute / ?) * ? AS minute,
		SUM(requests), SUM(failovers), SUM(rate_limited_429), SUM(failures),
		SUM(input), SUM(output), SUM(cache_creation), SUM(cache_read), SUM(token_requests),
		MAX(last_request_at), SUM(latency_ms_sum), SUM(ttft_ms_sum), SUM(duration_ms_sum)
		FROM minute_buckets WHERE minute >= ? AND minute <= ?`
	args := []any{bucketSecs, bucketSecs, from, to}
	if provider != "" {
		query += ` AND provider = ?`
		args = append(args, provider)
	}
	if model != "" {
		query += ` AND model = ?`
		args = append(args, model)
	}
	query += ` GROUP BY provider, model, (minute / ?) * ? ORDER BY provider, model, minute`
	args = append(args, bucketSecs, bucketSecs)
	return s.queryBuckets(query, args...)
}

func (s *Store) queryBuckets(query string, args ...any) ([]Bucket, error) {
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var buckets []Bucket
	for rows.Next() {
		var bucket Bucket
		if err := rows.Scan(
			&bucket.Provider,
			&bucket.Model,
			&bucket.Minute,
			&bucket.Requests,
			&bucket.Failovers,
			&bucket.RateLimited429,
			&bucket.Failures,
			&bucket.Input,
			&bucket.Output,
			&bucket.CacheCreation,
			&bucket.CacheRead,
			&bucket.TokenRequests,
			&bucket.LastRequestAt,
			&bucket.LatencySum,
			&bucket.TTFTSum,
			&bucket.DurationSum,
		); err != nil {
			return nil, err
		}
		bucket.AvgLatencyMs = averageMilliseconds(bucket.LatencySum, bucket.Requests)
		bucket.AvgTtftMs = averageMilliseconds(bucket.TTFTSum, bucket.Requests)
		buckets = append(buckets, bucket)
	}
	return buckets, rows.Err()
}

func averageMilliseconds(sum, requests uint64) float64 {
	if requests == 0 {
		return 0
	}
	average := float64(sum) / float64(requests)
	return math.Round(average*10) / 10
}

// analyticsLayout validates the granularity and returns the Go layout that
// turns a bucket label back into a unix instant in time.Local. The label —
// not a strftime '%s' epoch — is the source of truth because '%s' reinterprets
// the local date as UTC and shifts the bucket by one timezone offset.
func analyticsLayout(granularity string) (string, error) {
	switch granularity {
	case "minute", "hour":
		return "2006-01-02 15:04", nil
	case "day", "week", "month":
		return "2006-01-02", nil
	default:
		return "", fmt.Errorf("granularity must be minute, hour, day, week or month, got %q", granularity)
	}
}

// analyticsBucketLabel renders one source minute's local-time bucket label,
// byte-identical to the SQLite expressions it replaced
// (strftime('%Y-%m-%d %H:%M', minute, 'unixepoch', 'localtime') and the
// date(...,'localtime',...) forms). Calendar bucketing happens in Go, not in
// SQL, because the pure-Go SQLite driver reloads the whole timezone for every
// 'localtime' evaluation — one allocation-heavy libc call per scanned row per
// expression, which dominated the daemon's allocation rate under analytics
// polling. Semantics are exactly the old GROUP BY's: two source minutes share
// one bucket iff their local labels match (the DST fall-back hour folds two
// UTC hours into one bucket; the spring-forward gap never has its label).
func analyticsBucketLabel(minute int64, granularity string, loc *time.Location) string {
	local := time.Unix(minute, 0).In(loc)
	switch granularity {
	case "minute":
		return local.Format("2006-01-02 15:04")
	case "hour":
		return local.Format("2006-01-02 15") + ":00"
	case "day":
		return local.Format("2006-01-02")
	case "week":
		// Local weeks start Monday: the old SQL's 'weekday 0','-6 days'.
		back := (int(local.Weekday()) + 6) % 7
		return time.Date(local.Year(), local.Month(), local.Day()-back, 0, 0, 0, 0, loc).Format("2006-01-02")
	case "month":
		return time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc).Format("2006-01-02")
	default:
		return ""
	}
}

// QueryAnalytics returns local calendar-hour/day/month aggregates over
// minute_buckets, grouped by (provider, model). Every counter column is
// additive across the folded minutes (latency/ttft sums included, so callers
// derive averages with the same requests-weighted semantics as QueryRange).
// SQL fetches raw per-minute PK rows (upserts already accumulated them) and
// the local-calendar merge happens in Go (analyticsBucketLabel) — see
// queryAnalyticsIn.
func (s *Store) QueryAnalytics(from, to int64, provider, model, granularity string) ([]AnalyticsBucket, error) {
	return s.queryAnalyticsIn(from, to, provider, model, granularity, time.Local)
}

// queryAnalyticsIn is QueryAnalytics with an injected timezone (the labeler
// seam tests use for DST determinism; production always passes time.Local,
// the zone SQLite's 'localtime' used to resolve).
//
// SQL fetches raw primary-key rows — each (provider, model, minute) row IS
// already that minute's accumulated totals (FlushMCP-style upserts add in
// place), so a SQL GROUP BY over the PK would re-sort 25k+ rows through a
// temp B-tree to return the exact same rows. Calendar folding happens in Go
// (analyticsBucketLabel); no ORDER BY either — the Go fold sorts by
// (dims, label) itself.
func (s *Store) queryAnalyticsIn(from, to int64, provider, model, granularity string, loc *time.Location) ([]AnalyticsBucket, error) {
	layout, err := analyticsLayout(granularity)
	if err != nil {
		return nil, err
	}

	query := `SELECT provider, model, minute,
		requests, failovers, rate_limited_429, failures,
		input, output, cache_creation, cache_read,
		last_request_at, latency_ms_sum, ttft_ms_sum, duration_ms_sum
		FROM minute_buckets WHERE minute >= ? AND minute <= ?`
	args := []any{from, to}
	if provider != "" {
		// Pooled rows live under virtual "name#<accountID>" keys; filtering by
		// the parent name matches the provider's every account (exact compare
		// on the pre-# segment — no LIKE wildcards in provider names).
		query += ` AND (provider = ? OR (instr(provider, '#') > 0 AND substr(provider, 1, instr(provider, '#') - 1) = ?))`
		args = append(args, provider, provider)
	}
	if model != "" {
		query += ` AND model = ?`
		args = append(args, model)
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	type groupKey struct {
		provider, model, label string
	}
	groups := map[groupKey]*AnalyticsBucket{}
	labeler := minuteLabeler{granularity: granularity, loc: loc}
	for rows.Next() {
		var (
			bucket AnalyticsBucket
			minute int64
		)
		if err := rows.Scan(
			&bucket.Provider, &bucket.Model, &minute,
			&bucket.Requests, &bucket.Failovers, &bucket.RateLimited429, &bucket.Failures,
			&bucket.Input, &bucket.Output, &bucket.CacheCreation, &bucket.CacheRead,
			&bucket.LastRequestAt, &bucket.LatencySum, &bucket.TTFTSum, &bucket.DurationSum,
		); err != nil {
			_ = rows.Close()
			return nil, err
		}
		key := groupKey{bucket.Provider, bucket.Model, labeler.bucketLabel(minute)}
		if dst, ok := groups[key]; ok {
			mergeAnalyticsBucket(dst, &bucket)
		} else {
			groups[key] = &bucket
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
		if a.provider != b.provider {
			return a.provider < b.provider
		}
		if a.model != b.model {
			return a.model < b.model
		}
		return a.label < b.label
	})
	var buckets []AnalyticsBucket
	for _, key := range ordered {
		bucket := groups[key]
		bucket.Bucket = localCalendarStart(key.label, layout, granularity)
		bucket.AvgLatencyMs = averageMilliseconds(bucket.LatencySum, bucket.Requests)
		bucket.AvgTtftMs = averageMilliseconds(bucket.TTFTSum, bucket.Requests)
		bucket.AvgDurationMs = averageMilliseconds(bucket.DurationSum, bucket.Requests)
		buckets = append(buckets, *bucket)
	}
	return buckets, nil
}

// minuteLabeler memoizes the bucket label of the current minute. The
// idx_minute range scans deliver rows minute-ascending, so every (dims) row
// of one minute reuses one label computation (and one shared string) instead
// of recomputing time.Unix+In+Format per row.
type minuteLabeler struct {
	granularity string
	loc         *time.Location
	minute      int64
	label       string
}

func (m *minuteLabeler) bucketLabel(minute int64) string {
	if minute != m.minute {
		m.minute = minute
		m.label = analyticsBucketLabel(minute, m.granularity, m.loc)
	}
	return m.label
}

// mergeAnalyticsBucket folds one (provider, model, minute) partial bucket into
// its calendar bucket: counters add, LastRequestAt keeps the max (the old
// SQL's MAX over the group is the max of the per-minute MAXes).
func mergeAnalyticsBucket(dst, src *AnalyticsBucket) {
	dst.Requests += src.Requests
	dst.Failovers += src.Failovers
	dst.RateLimited429 += src.RateLimited429
	dst.Failures += src.Failures
	dst.Input += src.Input
	dst.Output += src.Output
	dst.CacheCreation += src.CacheCreation
	dst.CacheRead += src.CacheRead
	if src.LastRequestAt > dst.LastRequestAt {
		dst.LastRequestAt = src.LastRequestAt
	}
	dst.LatencySum += src.LatencySum
	dst.TTFTSum += src.TTFTSum
	dst.DurationSum += src.DurationSum
}

// QueryAnalyticsAgents is the agent-dimension form of QueryAnalytics:
// calendar buckets grouped by (agent, provider, model) over agent_buckets.
// agent_buckets carries no failover/429 counters, so those stay zero.
func (s *Store) QueryAnalyticsAgents(from, to int64, agent, provider, model, granularity string) ([]AnalyticsBucket, error) {
	layout, err := analyticsLayout(granularity)
	if err != nil {
		return nil, err
	}

	query := `SELECT agent, provider, model, minute,
		requests, failures,
		input, output, cache_creation, cache_read,
		latency_ms_sum, ttft_ms_sum, duration_ms_sum
		FROM agent_buckets WHERE minute >= ? AND minute <= ?`
	args := []any{from, to}
	if agent != "" {
		query += ` AND agent = ?`
		args = append(args, agent)
	}
	if provider != "" {
		// Same parent-name semantics as QueryAnalytics: a pooled provider's
		// rows live under virtual "name#<accountID>" keys.
		query += ` AND (provider = ? OR (instr(provider, '#') > 0 AND substr(provider, 1, instr(provider, '#') - 1) = ?))`
		args = append(args, provider, provider)
	}
	if model != "" {
		query += ` AND model = ?`
		args = append(args, model)
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	type groupKey struct {
		agent, provider, model, label string
	}
	groups := map[groupKey]*AnalyticsBucket{}
	labeler := minuteLabeler{granularity: granularity, loc: time.Local}
	for rows.Next() {
		var (
			bucket AnalyticsBucket
			minute int64
		)
		if err := rows.Scan(
			&bucket.Agent, &bucket.Provider, &bucket.Model, &minute,
			&bucket.Requests, &bucket.Failures,
			&bucket.Input, &bucket.Output, &bucket.CacheCreation, &bucket.CacheRead,
			&bucket.LatencySum, &bucket.TTFTSum, &bucket.DurationSum,
		); err != nil {
			_ = rows.Close()
			return nil, err
		}
		key := groupKey{bucket.Agent, bucket.Provider, bucket.Model, labeler.bucketLabel(minute)}
		if dst, ok := groups[key]; ok {
			mergeAnalyticsBucket(dst, &bucket)
		} else {
			groups[key] = &bucket
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
		if a.agent != b.agent {
			return a.agent < b.agent
		}
		if a.provider != b.provider {
			return a.provider < b.provider
		}
		if a.model != b.model {
			return a.model < b.model
		}
		return a.label < b.label
	})
	var buckets []AnalyticsBucket
	for _, key := range ordered {
		bucket := groups[key]
		bucket.Bucket = localCalendarStart(key.label, layout, granularity)
		bucket.AvgLatencyMs = averageMilliseconds(bucket.LatencySum, bucket.Requests)
		bucket.AvgTtftMs = averageMilliseconds(bucket.TTFTSum, bucket.Requests)
		bucket.AvgDurationMs = averageMilliseconds(bucket.DurationSum, bucket.Requests)
		buckets = append(buckets, *bucket)
	}
	return buckets, nil
}

// QueryAgentNames lists the distinct agents with traffic in the window
// (provider/model filtered). It deliberately ignores any agent filter — the
// caller uses it as a suggestion list, which must not collapse to the
// already-selected value.
func (s *Store) QueryAgentNames(from, to int64, provider, model string) ([]string, error) {
	query := `SELECT DISTINCT agent FROM agent_buckets WHERE minute >= ? AND minute <= ?`
	var args []any
	if provider != "" {
		query += ` AND provider = ?`
		args = append(args, provider)
	}
	if model != "" {
		query += ` AND model = ?`
		args = append(args, model)
	}
	query += ` ORDER BY agent`
	args = append([]any{from, to}, args...)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// localCalendarStart turns a SQLite local-time label into the unix instant of
// the calendar bucket start in time.Local. layout comes from
// analyticsBucketing; month labels keep their legacy "2006-01" truncation.
func localCalendarStart(label, layout, granularity string) int64 {
	if granularity == "month" {
		if len(label) < len("2006-01") {
			return 0
		}
		label = label[:7]
		layout = "2006-01"
	}
	parsed, err := time.ParseInLocation(layout, label, time.Local)
	if err != nil {
		return 0
	}
	return parsed.Unix()
}

// QueryAgents returns agent/provider/model rows in the inclusive [from, to]
// range. bucketSecs <= 60 preserves each raw minute row; wider buckets sum and
// floor exactly like QueryRange.
func (s *Store) QueryAgents(from, to int64, agent, provider, model string, bucketSecs int64) ([]AgentBucket, error) {
	selectColumns := "agent, provider, model, minute, requests, input, output, cache_creation, cache_read, latency_ms_sum, ttft_ms_sum, duration_ms_sum, failures"
	groupClause := ""
	if bucketSecs > 60 {
		selectColumns = "agent, provider, model, (minute / ?) * ? AS minute, SUM(requests), SUM(input), SUM(output), SUM(cache_creation), SUM(cache_read), SUM(latency_ms_sum), SUM(ttft_ms_sum), SUM(duration_ms_sum), SUM(failures)"
		groupClause = " GROUP BY agent, provider, model, (minute / ?) * ?"
	}

	query := "SELECT " + selectColumns + " FROM agent_buckets WHERE minute >= ? AND minute <= ?"
	args := []any{}
	if bucketSecs > 60 {
		args = append(args, bucketSecs, bucketSecs)
	}
	args = append(args, from, to)
	if agent != "" {
		query += " AND agent = ?"
		args = append(args, agent)
	}
	if provider != "" {
		query += " AND provider = ?"
		args = append(args, provider)
	}
	if model != "" {
		query += " AND model = ?"
		args = append(args, model)
	}
	query += groupClause + " ORDER BY agent, provider, model, minute"
	if bucketSecs > 60 {
		args = append(args, bucketSecs, bucketSecs)
	}

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var buckets []AgentBucket
	for rows.Next() {
		var bucket AgentBucket
		if err := rows.Scan(
			&bucket.Agent,
			&bucket.Provider,
			&bucket.Model,
			&bucket.Minute,
			&bucket.Requests,
			&bucket.Input,
			&bucket.Output,
			&bucket.CacheCreation,
			&bucket.CacheRead,
			&bucket.LatencySum,
			&bucket.TTFTSum,
			&bucket.DurationSum,
			&bucket.Failures,
		); err != nil {
			return nil, err
		}
		buckets = append(buckets, bucket)
	}
	return buckets, rows.Err()
}

// EarliestMinute returns the oldest bucket minute across BOTH tables (unix
// seconds), 0 when no rows exist. It anchors the "Since <time>" label for the
// cumulative usage surfaces (/api/tokens): the counters are seeded from the
// persisted all-time totals, so the earliest bucket is when the current
// counting epoch began (initial data or the last full reset).
func (s *Store) EarliestMinute() int64 {
	return s.earliestMinuteOf("minute_buckets", "agent_buckets")
}

// EarliestMinuteAll returns the oldest bucket minute across ALL stats tables
// (unix seconds), 0 when no rows exist: the LLM pair (minute_buckets,
// agent_buckets) plus the MCP pair (mcp_buckets, mcp_tool_buckets). It
// anchors all-time (from=0) windows on MCP surfaces: MCP usage can predate
// the first LLM bucket, and the LLM-only EarliestMinute would clamp the
// window past that older MCP history (docs/web-api.md /api/mcp/analytics).
func (s *Store) EarliestMinuteAll() int64 {
	return s.earliestMinuteOf("minute_buckets", "agent_buckets", "mcp_buckets", "mcp_tool_buckets")
}

// earliestMinuteOf returns the smallest MIN(minute) across the given stats
// tables. Table names are package-internal constants (never user input), so
// the identifier interpolation is safe. 0 on a query error or when every
// table is empty (fail-closed: no clamp, same as EarliestMinute).
func (s *Store) earliestMinuteOf(tables ...string) int64 {
	var earliest sql.NullInt64
	for _, table := range tables {
		var v sql.NullInt64
		if err := s.db.QueryRow(`SELECT MIN(minute) FROM ` + table).Scan(&v); err != nil {
			return 0
		}
		if v.Valid && (!earliest.Valid || v.Int64 < earliest.Int64) {
			earliest = v
		}
	}
	if !earliest.Valid {
		return 0
	}
	return earliest.Int64
}
