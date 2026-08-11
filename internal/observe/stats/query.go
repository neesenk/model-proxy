package stats

import (
	"fmt"
	"math"
	"time"
)

// LoadCumulative returns all-time provider/model totals. Additive counters use
// SUM and LastRequestAt uses MAX across minute buckets.
func (s *Store) LoadCumulative() (map[Key]Counters, error) {
	rows, err := s.db.Query(`SELECT provider, model,
		SUM(requests), SUM(failovers), SUM(rate_limited_429), SUM(failures),
		SUM(input), SUM(output), SUM(cache_creation), SUM(cache_read), SUM(token_requests),
		MAX(last_request_at), SUM(latency_ms_sum), SUM(ttft_ms_sum)
		FROM minute_buckets GROUP BY provider, model`)
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
			latency_ms_sum, ttft_ms_sum
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
		MAX(last_request_at), SUM(latency_ms_sum), SUM(ttft_ms_sum)
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

// QueryAnalytics returns local calendar-day or calendar-month aggregates.
func (s *Store) QueryAnalytics(from, to int64, provider, model, granularity string) ([]AnalyticsBucket, error) {
	if granularity != "day" && granularity != "month" {
		return nil, fmt.Errorf("granularity must be day or month, got %q", granularity)
	}
	truncation := "start of day"
	if granularity == "month" {
		truncation = "start of month"
	}

	query := `SELECT provider, model,
		date(minute,'unixepoch','localtime',?) AS d,
		SUM(requests), SUM(input), SUM(output), SUM(cache_creation), SUM(cache_read),
		MAX(last_request_at)
		FROM minute_buckets WHERE minute >= ? AND minute <= ?`
	args := []any{truncation, from, to}
	if provider != "" {
		query += ` AND provider = ?`
		args = append(args, provider)
	}
	if model != "" {
		query += ` AND model = ?`
		args = append(args, model)
	}
	query += ` GROUP BY provider, model, date(minute,'unixepoch','localtime',?) ORDER BY provider, model, d`
	args = append(args, truncation)

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var buckets []AnalyticsBucket
	for rows.Next() {
		var (
			bucket AnalyticsBucket
			date   string
		)
		if err := rows.Scan(
			&bucket.Provider,
			&bucket.Model,
			&date,
			&bucket.Requests,
			&bucket.Input,
			&bucket.Output,
			&bucket.CacheCreation,
			&bucket.CacheRead,
			&bucket.LastRequestAt,
		); err != nil {
			return nil, err
		}
		bucket.Bucket = localCalendarStart(date, granularity)
		buckets = append(buckets, bucket)
	}
	return buckets, rows.Err()
}

func localCalendarStart(date, granularity string) int64 {
	layout := "2006-01-02"
	if granularity == "month" {
		if len(date) < len("2006-01") {
			return 0
		}
		date = date[:7]
		layout = "2006-01"
	}
	parsed, err := time.ParseInLocation(layout, date, time.Local)
	if err != nil {
		return 0
	}
	return parsed.Unix()
}

// QueryAgents returns agent/provider/model rows in the inclusive [from, to]
// range. bucketSecs <= 60 preserves each raw minute row; wider buckets sum and
// floor exactly like QueryRange.
func (s *Store) QueryAgents(from, to int64, agent, provider, model string, bucketSecs int64) ([]AgentBucket, error) {
	selectColumns := "agent, provider, model, minute, requests, input, output, latency_ms_sum, failures"
	groupClause := ""
	if bucketSecs > 60 {
		selectColumns = "agent, provider, model, (minute / ?) * ? AS minute, SUM(requests), SUM(input), SUM(output), SUM(latency_ms_sum), SUM(failures)"
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
			&bucket.LatencySum,
			&bucket.Failures,
		); err != nil {
			return nil, err
		}
		buckets = append(buckets, bucket)
	}
	return buckets, rows.Err()
}
