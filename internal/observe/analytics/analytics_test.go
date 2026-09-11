package analytics

import (
	"math"
	"testing"

	observestats "model-proxy/internal/observe/stats"
	"model-proxy/internal/pricing"
)

func almostEqual(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func ptr(v float64) *float64 { return &v }

var pricedCatalog = &pricing.Catalog{ByModel: map[string]pricing.Entry{
	"priced": {Prompt: .001, Completion: .002, CacheRead: .003, CacheWrite: .004},
}}

func TestFoldTotalsDerivesUnifiedMetrics(t *testing.T) {
	buckets := []observestats.AnalyticsBucket{
		// 2 requests, 4s call time, 10 in / 5 out / 2 read / 1 written.
		{Provider: "p", Model: "priced", Bucket: 100, Requests: 2, Input: 10, Output: 5, CacheRead: 2, CacheCreation: 1, LatencySum: 600, TTFTSum: 60, DurationSum: 4000},
		// 1 request, no duration, no tokens.
		{Provider: "p", Model: "unpriced", Bucket: 200, Requests: 1},
	}
	got := FoldTotals(buckets, nil, pricedCatalog)
	// Four-bucket tokens: 10+5+1+2 (priced) + 0 (unpriced).
	if got.Tokens != 18 {
		t.Errorf("Tokens = %d, want 18", got.Tokens)
	}
	// tok/s = 5 output / 4s call time; latency/ttft requests-weighted over
	// BOTH buckets ((600+0)/3, (60+0)/3); err% 0 with requests.
	if got.TokSec == nil || !almostEqual(*got.TokSec, 1.25) {
		t.Errorf("TokSec = %v, want 1.25", got.TokSec)
	}
	if got.AvgLatencyMs == nil || !almostEqual(*got.AvgLatencyMs, 200) || got.AvgTtftMs == nil || !almostEqual(*got.AvgTtftMs, 20) {
		t.Errorf("averages = %v/%v, want 200/20", got.AvgLatencyMs, got.AvgTtftMs)
	}
	if got.ErrPct == nil || *got.ErrPct != 0 {
		t.Errorf("ErrPct = %v, want non-null 0", got.ErrPct)
	}
	// Cache hit = 2 reads / (10+1+2) prompt tokens.
	if got.CacheHitPct == nil || !almostEqual(*got.CacheHitPct, 200.0/13) {
		t.Errorf("CacheHitPct = %v, want 2/13", got.CacheHitPct)
	}
	// Cost prices only the priced bucket (four-bucket, per-price).
	if got.Cost == nil || !almostEqual(*got.Cost, .03) {
		t.Errorf("Cost = %v, want 0.03", got.Cost)
	}
}

func TestFoldTotalsNullsMeanNoData(t *testing.T) {
	empty := FoldTotals(nil, nil, pricedCatalog)
	if empty.Tokens != 0 || empty.Requests != 0 {
		t.Fatalf("empty fold = %+v", empty)
	}
	for name, v := range map[string]*float64{"err_pct": empty.ErrPct, "tok_sec": empty.TokSec, "cache_hit_pct": empty.CacheHitPct, "avg_latency": empty.AvgLatencyMs, "avg_ttft": empty.AvgTtftMs, "cost": empty.Cost} {
		if v != nil {
			t.Errorf("%s = %v, want nil (no data is never a fabricated zero)", name, *v)
		}
	}
	// Requests without call time: tok/s null but err%/averages present.
	noDur := FoldTotals([]observestats.AnalyticsBucket{{Requests: 2, Failures: 1, LatencySum: 800, TTFTSum: 200}}, nil, pricedCatalog)
	if noDur.TokSec != nil {
		t.Errorf("TokSec = %v, want nil without duration", *noDur.TokSec)
	}
	if noDur.ErrPct == nil || !almostEqual(*noDur.ErrPct, 50) || noDur.AvgLatencyMs == nil || !almostEqual(*noDur.AvgLatencyMs, 400) {
		t.Errorf("no-duration fold = %+v", noDur)
	}
	// Prompt workload with no reads: hit rate is 0%, not null (data exists).
	noRead := FoldTotals([]observestats.AnalyticsBucket{{Input: 100}}, nil, pricedCatalog)
	if noRead.CacheHitPct == nil || *noRead.CacheHitPct != 0 {
		t.Errorf("CacheHitPct = %v, want non-null 0", noRead.CacheHitPct)
	}
}

func TestNewPointCarriesDerivedFields(t *testing.T) {
	b := observestats.AnalyticsBucket{Provider: "p", Model: "priced", Bucket: 60, Requests: 2, Input: 10, Output: 5, CacheRead: 2, CacheCreation: 1, LatencySum: 600, TTFTSum: 60, DurationSum: 4000, AvgLatencyMs: 300, AvgTtftMs: 30, AvgDurationMs: 2000}
	p := NewPoint(b, nil, pricedCatalog)
	if p.Tokens != 18 || p.TokSec == nil || !almostEqual(*p.TokSec, 1.25) ||
		p.CacheHitPct == nil || !almostEqual(*p.CacheHitPct, 200.0/13) || p.ErrPct == nil || *p.ErrPct != 0 {
		t.Fatalf("derived point = %+v", p)
	}
	if !p.Priced || p.Cost == nil || !almostEqual(*p.Cost, .03) {
		t.Errorf("point pricing = %+v", p)
	}
	// Unpriced model: cost nil, never fabricated.
	unpriced := NewPoint(observestats.AnalyticsBucket{Model: "unknown", Requests: 1}, nil, pricedCatalog)
	if unpriced.Priced || unpriced.Cost != nil {
		t.Errorf("unpriced point = %+v", unpriced)
	}
	// Overrides win over the catalog (config prices: path) — only the
	// input price is overridden here: 10 input tokens × $1/token.
	over := NewPoint(b, map[string]pricing.Override{"priced": {Input: 1e6}}, pricedCatalog)
	if !almostEqual(*over.Cost, 10) {
		t.Errorf("override cost = %v, want 10", *over.Cost)
	}
}

func TestGroupSeriesKeysAndFoldsOwnBuckets(t *testing.T) {
	buckets := []observestats.AnalyticsBucket{
		{Agent: "", Provider: "p", Model: "m", Bucket: 100, Requests: 2, Output: 4, DurationSum: 2000},
		{Agent: "", Provider: "p", Model: "m", Bucket: 160, Requests: 1, Output: 2, DurationSum: 1000},
		{Agent: "", Provider: "q", Model: "m", Bucket: 100, Requests: 5, Output: 10, DurationSum: 5000},
	}
	byModel := Group(buckets, "model", nil, pricedCatalog)
	if len(byModel) != 2 {
		t.Fatalf("by=model series = %d, want 2", len(byModel))
	}
	first := byModel[0]
	if first.Provider != "p" || len(first.Points) != 2 {
		t.Fatalf("first series = %+v", first)
	}
	// The series Totals fold ONLY its own buckets: p/m = 3 reqs, 6 output
	// over 3s → 2 tok/s.
	if first.Totals.Requests != 3 || first.Totals.TokSec == nil || !almostEqual(*first.Totals.TokSec, 2) {
		t.Errorf("series totals = %+v", first.Totals)
	}
	// by=agent splits the same buckets per agent.
	agentBuckets := []observestats.AnalyticsBucket{
		{Agent: "codex", Provider: "p", Model: "m", Requests: 1},
		{Agent: "pi", Provider: "p", Model: "m", Requests: 2},
	}
	byAgent := Group(agentBuckets, "agent", nil, pricedCatalog)
	if len(byAgent) != 2 || byAgent[0].Agent != "codex" || byAgent[0].Totals.Requests != 1 || byAgent[1].Totals.Requests != 2 {
		t.Fatalf("by=agent series = %+v", byAgent)
	}
}
