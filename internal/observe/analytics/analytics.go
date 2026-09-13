// Package analytics owns the unified derived-metric definitions projected
// over observestats buckets: tokens totals (four-bucket), tok/s decode
// speed, cache-hit rate, error rate, requests-weighted latency averages and
// equivalent payg cost. One authority — the same block is folded identically
// for window totals, the equal-length compare window and each series, and
// consumers (the web transport's /api/analytics, future CLI columns) read
// the fields instead of re-deriving formulas, so a metric can never drift
// per-view.
package analytics

import (
	"math"
	"sort"

	observestats "model-proxy/internal/observe/stats"
	"model-proxy/internal/pricing"
)

// Totals is the unified derived-metric block. Nullable fields use pointers:
// null means "no data" (no requests / no call time / nothing read / nothing
// priced), never a fabricated zero. Failovers/RateLimited429 are additive
// attempt counters (minute_buckets carries them; the agent dimension reads
// agent_buckets, which has no such columns — they stay zero there).
type Totals struct {
	Requests       uint64   `json:"requests"`
	Failovers      uint64   `json:"failovers"`
	RateLimited429 uint64   `json:"rate_limited_429"`
	Failures       uint64   `json:"failures"`
	ErrPct         *float64 `json:"err_pct"` // null when no requests
	Input          uint64   `json:"input"`
	Output         uint64   `json:"output"`
	CacheCreation  uint64   `json:"cache_creation"`
	CacheRead      uint64   `json:"cache_read"`
	Tokens         uint64   `json:"tokens"`         // four-bucket total
	AvgLatencyMs   *float64 `json:"avg_latency_ms"` // null when no requests
	AvgTtftMs      *float64 `json:"avg_ttft_ms"`    // null when no requests
	TokSec         *float64 `json:"tok_sec"`        // output / full call seconds; null without call time
	CacheHitPct    *float64 `json:"cache_hit_pct"`  // reads / full prompt workload; null without reads
	Cost           *float64 `json:"cost"`           // nil when nothing in the window is priced
}

// Point is one bucket's raw counters plus the same derived metrics, so the
// per-bucket view (trend charts) and the aggregate views share one set of
// definitions. Bucket-average fields keep the store's 0-baseline (the raw
// sums stay reconstructible); the rate fields carry the null semantics.
type Point struct {
	Bucket         int64    `json:"bucket"`
	Requests       uint64   `json:"requests"`
	Failovers      uint64   `json:"failovers"`
	RateLimited429 uint64   `json:"rate_limited_429"`
	Failures       uint64   `json:"failures"`
	Input          uint64   `json:"input"`
	Output         uint64   `json:"output"`
	CacheCreation  uint64   `json:"cache_creation"`
	CacheRead      uint64   `json:"cache_read"`
	Tokens         uint64   `json:"tokens"`
	AvgLatencyMs   float64  `json:"avg_latency_ms"`
	AvgTtftMs      float64  `json:"avg_ttft_ms"`
	AvgDurationMs  float64  `json:"avg_duration_ms"`
	TokSec         *float64 `json:"tok_sec"`
	CacheHitPct    *float64 `json:"cache_hit_pct"`
	ErrPct         *float64 `json:"err_pct"`
	Cost           *float64 `json:"cost"`
	Priced         bool     `json:"priced"`
}

// Series groups one (agent, provider, model) slice — the chart line and the
// leaderboard row — with the same Totals block folded over its own buckets.
type Series struct {
	Agent    string  `json:"agent,omitempty"`
	Provider string  `json:"provider"`
	Model    string  `json:"model"`
	Points   []Point `json:"points"`
	Totals   Totals  `json:"totals"`
}

// NewPoint projects one store bucket into a Point, pricing it with the same
// per-bucket path as FoldTotals (unknown price → Cost nil, Priced false —
// never fabricated).
func NewPoint(b observestats.AnalyticsBucket, overrides map[string]pricing.Override, catalog *pricing.Catalog) Point {
	p := Point{
		Bucket: b.Bucket, Requests: b.Requests, Failovers: b.Failovers,
		RateLimited429: b.RateLimited429, Failures: b.Failures,
		Input: b.Input, Output: b.Output,
		CacheCreation: b.CacheCreation, CacheRead: b.CacheRead,
		Tokens:       b.Input + b.Output + b.CacheCreation + b.CacheRead,
		AvgLatencyMs: b.AvgLatencyMs, AvgTtftMs: b.AvgTtftMs, AvgDurationMs: b.AvgDurationMs,
	}
	if callSec := float64(b.DurationSum) / 1000; callSec > 0 {
		tokSec := float64(b.Output) / callSec
		p.TokSec = &tokSec
	}
	if denom := b.Input + b.CacheCreation + b.CacheRead; denom > 0 {
		hit := float64(b.CacheRead) / float64(denom) * 100
		p.CacheHitPct = &hit
	}
	if b.Requests > 0 {
		errPct := float64(b.Failures) / float64(b.Requests) * 100
		p.ErrPct = &errPct
	}
	if entry, ok := pricing.Resolve(overrides, catalog, b.Model); ok {
		cost := pricing.ComputeCost(b.Input, b.Output, b.CacheRead, b.CacheCreation, entry)
		p.Cost, p.Priced = &cost, true
	}
	return p
}

// FoldTotals folds one window of buckets into the unified derived-metric
// block. Requests-weighted averages round to one decimal (the same
// averageMilliseconds convention as the store's per-bucket projection).
func FoldTotals(buckets []observestats.AnalyticsBucket, overrides map[string]pricing.Override, catalog *pricing.Catalog) Totals {
	var t Totals
	var latencySum, ttftSum, durationSum uint64
	for _, b := range buckets {
		t.Requests += b.Requests
		t.Failovers += b.Failovers
		t.RateLimited429 += b.RateLimited429
		t.Failures += b.Failures
		t.Input += b.Input
		t.Output += b.Output
		t.CacheCreation += b.CacheCreation
		t.CacheRead += b.CacheRead
		latencySum += b.LatencySum
		ttftSum += b.TTFTSum
		durationSum += b.DurationSum
		if entry, ok := pricing.Resolve(overrides, catalog, b.Model); ok {
			x := pricing.ComputeCost(b.Input, b.Output, b.CacheRead, b.CacheCreation, entry)
			if t.Cost == nil {
				t.Cost = new(float64)
			}
			*t.Cost += x
		}
	}
	t.Tokens = t.Input + t.Output + t.CacheCreation + t.CacheRead
	if t.Requests > 0 {
		errPct := float64(t.Failures) / float64(t.Requests) * 100
		lat := math.Round(float64(latencySum)/float64(t.Requests)*10) / 10
		ttft := math.Round(float64(ttftSum)/float64(t.Requests)*10) / 10
		t.ErrPct, t.AvgLatencyMs, t.AvgTtftMs = &errPct, &lat, &ttft
	}
	if durationSum > 0 {
		tokSec := float64(t.Output) / (float64(durationSum) / 1000)
		t.TokSec = &tokSec
	}
	if denom := t.Input + t.CacheCreation + t.CacheRead; denom > 0 {
		hit := float64(t.CacheRead) / float64(denom) * 100
		t.CacheHitPct = &hit
	}
	return t
}

// Group folds buckets into per-(agent, provider, model) series (by="agent"
// keys by agent; anything else keys by provider+model), preserving the
// buckets' order of first appearance. Each series carries its own Totals
// block folded over exactly its buckets.
func Group(buckets []observestats.AnalyticsBucket, by string, overrides map[string]pricing.Override, catalog *pricing.Catalog) []Series {
	type key struct {
		agent, provider, model string
	}
	var order []key
	byKey := map[key]*Series{}
	slices := map[key][]observestats.AnalyticsBucket{}
	for _, b := range buckets {
		k := key{provider: b.Provider, model: b.Model}
		if by == "agent" {
			k.agent = b.Agent
		}
		v := byKey[k]
		if v == nil {
			v = &Series{Agent: b.Agent, Provider: b.Provider, Model: b.Model}
			byKey[k] = v
			order = append(order, k)
		}
		v.Points = append(v.Points, NewPoint(b, overrides, catalog))
		slices[k] = append(slices[k], b)
	}
	out := make([]Series, 0, len(order))
	for _, k := range order {
		s := *byKey[k]
		s.Totals = FoldTotals(slices[k], overrides, catalog)
		out = append(out, s)
	}
	return out
}

// YearCell is one local-calendar day of the usage heatmap: the same unified
// Totals block folded over that day's buckets, so the day view and the
// series/window views can never disagree on a metric.
type YearCell struct {
	Day int64 `json:"day"` // unix seconds of the local-calendar midnight
	Totals
}

// YearCells folds day-granularity buckets (the store's calendar "day"
// bucketing — Bucket is the local midnight) into per-day cells, ascending,
// only days with buckets. The heatmap's fixed trailing-year window is the
// caller's concern; the fold itself works on any day-granularity set.
func YearCells(buckets []observestats.AnalyticsBucket, overrides map[string]pricing.Override, catalog *pricing.Catalog) []YearCell {
	groups := map[int64][]observestats.AnalyticsBucket{}
	for _, b := range buckets {
		groups[b.Bucket] = append(groups[b.Bucket], b)
	}
	days := make([]int64, 0, len(groups))
	for d := range groups {
		days = append(days, d)
	}
	sort.Slice(days, func(i, j int) bool { return days[i] < days[j] })
	out := make([]YearCell, 0, len(days))
	for _, d := range days {
		out = append(out, YearCell{Day: d, Totals: FoldTotals(groups[d], overrides, catalog)})
	}
	return out
}
