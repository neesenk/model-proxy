package stats

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestQueryRangeAggregationFiltersAndLosslessStorage(t *testing.T) {
	store := newTestStore(t, 0)
	base := time.Now().Unix() / 600 * 600
	key := Key{Provider: "zhipu", Model: "glm-5"}
	rows := []struct {
		minute int64
		value  Counters
	}{
		{base, Counters{Requests: 1, Failovers: 1, RateLimited429: 2, Failures: 3, Input: 10, Output: 20, CacheCreation: 1, CacheRead: 2, TokenRequests: 1, LastRequestAt: 1000, LatencySum: 100, TTFTSum: 10}},
		{base + 60, Counters{Requests: 2, Failovers: 2, RateLimited429: 3, Failures: 4, Input: 20, Output: 30, CacheCreation: 2, CacheRead: 3, TokenRequests: 2, LastRequestAt: 3000, LatencySum: 250, TTFTSum: 35}},
		{base + 120, Counters{Requests: 3, Failovers: 3, RateLimited429: 4, Failures: 5, Input: 30, Output: 40, CacheCreation: 3, CacheRead: 4, TokenRequests: 3, LastRequestAt: 2000, LatencySum: 550, TTFTSum: 75}},
		{base + 600, Counters{Requests: 7, Input: 70, LastRequestAt: 7000}},
	}
	for _, row := range rows {
		if err := store.Flush(row.minute, map[Key]Counters{key: row.value}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Flush(base, map[Key]Counters{
		{Provider: "other", Model: "glm-5"}: {Requests: 11},
		{Provider: "zhipu", Model: "other"}: {Requests: 13},
	}); err != nil {
		t.Fatal(err)
	}

	wide, err := store.QueryRange(base-60, base+700, "zhipu", "glm-5", 600)
	if err != nil {
		t.Fatal(err)
	}
	if len(wide) != 2 {
		t.Fatalf("wide rows = %d, want 2: %+v", len(wide), wide)
	}
	first := wide[0]
	if first.Minute != base {
		t.Errorf("bucket start = %d, want %d", first.Minute, base)
	}
	if first.Requests != 6 || first.Failovers != 6 || first.RateLimited429 != 9 ||
		first.Failures != 12 || first.Input != 60 || first.Output != 90 ||
		first.CacheCreation != 6 || first.CacheRead != 9 || first.TokenRequests != 6 ||
		first.LastRequestAt != 3000 || first.LatencySum != 900 || first.TTFTSum != 120 {
		t.Errorf("aggregated counters = %+v", first)
	}
	if first.AvgLatencyMs != 150 || first.AvgTtftMs != 20 {
		t.Errorf("aggregated averages = %.1f/%.1f, want 150/20", first.AvgLatencyMs, first.AvgTtftMs)
	}
	if wide[1].Minute != base+600 || wide[1].Requests != 7 || wide[1].LastRequestAt != 7000 {
		t.Errorf("second bucket = %+v", wide[1])
	}

	raw, err := store.QueryRange(base-60, base+700, "zhipu", "glm-5", 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 4 {
		t.Errorf("raw rows = %d, want 4", len(raw))
	}
	providerOnly, err := store.QueryRange(base-60, base+60, "other", "", 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(providerOnly) != 1 || providerOnly[0].Requests != 11 {
		t.Errorf("provider filter = %+v", providerOnly)
	}
	modelOnly, err := store.QueryRange(base-60, base+60, "", "other", 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(modelOnly) != 1 || modelOnly[0].Requests != 13 {
		t.Errorf("model filter = %+v", modelOnly)
	}
}

func TestQueryAnalyticsDayMonthAndFilters(t *testing.T) {
	store := newTestStore(t, 0)
	now := time.Now().In(time.Local)
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.Local)
	dayStart := monthStart.AddDate(0, 0, 2)
	first := dayStart.Add(time.Hour).Unix() / 60 * 60
	second := dayStart.Add(2*time.Hour).Unix() / 60 * 60
	nextDay := dayStart.AddDate(0, 0, 1).Add(time.Hour).Unix() / 60 * 60
	key := Key{Provider: "p", Model: "m"}
	for minute, counters := range map[int64]Counters{
		first:   {Requests: 1, Input: 10, Output: 20, CacheCreation: 2, CacheRead: 3, LastRequestAt: 100},
		second:  {Requests: 2, Input: 30, Output: 40, CacheCreation: 4, CacheRead: 5, LastRequestAt: 300},
		nextDay: {Requests: 4, Input: 50, Output: 60, CacheCreation: 6, CacheRead: 7, LastRequestAt: 200},
	} {
		if err := store.Flush(minute, map[Key]Counters{key: counters}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Flush(first, map[Key]Counters{
		{Provider: "other", Model: "m"}: {Requests: 8},
	}); err != nil {
		t.Fatal(err)
	}

	days, err := store.QueryAnalytics(first-60, nextDay+60, "p", "m", "day")
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 2 {
		t.Fatalf("days = %d, want 2: %+v", len(days), days)
	}
	if days[0].Bucket != dayStart.Unix() || days[0].Requests != 3 ||
		days[0].Input != 40 || days[0].Output != 60 ||
		days[0].CacheCreation != 6 || days[0].CacheRead != 8 ||
		days[0].LastRequestAt != 300 {
		t.Errorf("first day = %+v", days[0])
	}

	months, err := store.QueryAnalytics(first-60, nextDay+60, "p", "", "month")
	if err != nil {
		t.Fatal(err)
	}
	if len(months) != 1 || months[0].Bucket != monthStart.Unix() ||
		months[0].Requests != 7 || months[0].Input != 90 {
		t.Errorf("month = %+v", months)
	}
	if _, err := store.QueryAnalytics(0, 1, "", "", "hour"); err == nil ||
		!strings.Contains(err.Error(), "day or month") {
		t.Fatalf("invalid granularity error = %v", err)
	}
}

func TestFlushAndQueryAgentsRawWideAndFilters(t *testing.T) {
	store := newTestStore(t, 0)
	key := AgentKey{Agent: "codex", Provider: "zhipu", Model: "glm-5"}
	if err := store.FlushAgents(0, map[AgentKey]AgentCounters{
		key: {Requests: 3, Input: 100, Output: 200, CacheCreation: 5, CacheRead: 40, LatencySum: 1500, Failures: 0},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FlushAgents(60, map[AgentKey]AgentCounters{
		key: {Requests: 5, Input: 400, Output: 800, CacheCreation: 10, CacheRead: 60, LatencySum: 6000, Failures: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FlushAgents(60, map[AgentKey]AgentCounters{
		key: {Requests: 2, Input: 10, Output: 20, CacheRead: 4, LatencySum: 500, Failures: 2},
		{Agent: "claude-code", Provider: "zhipu", Model: "glm-5"}: {Requests: 7},
		{Agent: "codex", Provider: "other", Model: "other"}:       {Requests: 11},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FlushAgents(120, nil); err != nil {
		t.Fatalf("empty FlushAgents: %v", err)
	}

	raw, err := store.QueryAgents(0, 60, "codex", "zhipu", "glm-5", 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) != 2 {
		t.Fatalf("raw agent rows = %d, want 2: %+v", len(raw), raw)
	}
	if raw[0].Minute != 0 || raw[0].Requests != 3 || raw[0].CacheCreation != 5 || raw[0].CacheRead != 40 {
		t.Errorf("minute 0 = %+v", raw[0])
	}
	if raw[1].Minute != 60 || raw[1].Requests != 7 || raw[1].Input != 410 ||
		raw[1].Output != 820 || raw[1].CacheCreation != 10 || raw[1].CacheRead != 64 ||
		raw[1].LatencySum != 6500 || raw[1].Failures != 3 {
		t.Errorf("minute 60 upsert = %+v", raw[1])
	}

	wide, err := store.QueryAgents(0, 60, "codex", "zhipu", "glm-5", 120)
	if err != nil {
		t.Fatal(err)
	}
	if len(wide) != 1 {
		t.Fatalf("wide agent rows = %d, want 1: %+v", len(wide), wide)
	}
	if got := wide[0]; got.Minute != 0 || got.Requests != 10 || got.Input != 510 ||
		got.Output != 1020 || got.CacheCreation != 15 || got.CacheRead != 104 ||
		got.LatencySum != 8000 || got.Failures != 3 {
		t.Errorf("wide agent bucket = %+v", got)
	}

	agentOnly, err := store.QueryAgents(0, 60, "claude-code", "", "", 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(agentOnly) != 1 || agentOnly[0].Requests != 7 {
		t.Errorf("agent filter = %+v", agentOnly)
	}
	providerModel, err := store.QueryAgents(0, 60, "", "other", "other", 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(providerModel) != 1 || providerModel[0].Requests != 11 {
		t.Errorf("provider/model filter = %+v", providerModel)
	}
}

// TestLoadCumulativeRangeWindowBoundaries pins the range aggregation behind
// the /api/tokens time selector: the from bound is inclusive (a bucket whose
// minute equals from counts), a to inside a minute includes that minute's
// bucket, either bound <= 0 is unbounded (both zero = all-time), a range
// beyond all data is empty, and an inverted range aggregates to empty.
func TestLoadCumulativeRangeWindowBoundaries(t *testing.T) {
	store := newTestStore(t, 0)
	key := Key{Provider: "zhipu", Model: "glm-5"}
	agentKey := AgentKey{Agent: "codex", Provider: "zhipu", Model: "glm-5"}
	if err := store.Flush(60, map[Key]Counters{
		key: {Requests: 1, Input: 10, Output: 1, CacheRead: 2, TokenRequests: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Flush(120, map[Key]Counters{
		key: {Requests: 1, Input: 20, Output: 2, CacheCreation: 3, CacheRead: 4, TokenRequests: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FlushAgents(60, map[AgentKey]AgentCounters{
		agentKey: {Requests: 1, Input: 5, Output: 1, CacheRead: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FlushAgents(120, map[AgentKey]AgentCounters{
		agentKey: {Requests: 1, Input: 7, Output: 2, CacheCreation: 1, CacheRead: 2},
	}); err != nil {
		t.Fatal(err)
	}

	// from exactly on a bucket boundary includes that bucket; a from inside
	// the previous minute resolves to the same set (storage is
	// minute-aligned).
	for _, from := range []int64{120, 61} {
		got, err := store.LoadCumulativeRange(from, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[key].Input != 20 || got[key].Output != 2 ||
			got[key].CacheCreation != 3 || got[key].CacheRead != 4 || got[key].TokenRequests != 1 {
			t.Errorf("LoadCumulativeRange(%d, 0) = %+v, want only the minute-120 bucket", from, got)
		}
		agents, err := store.LoadCumulativeAgentsRange(from, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(agents) != 1 || agents[agentKey].Input != 7 || agents[agentKey].CacheCreation != 1 ||
			agents[agentKey].CacheRead != 2 {
			t.Errorf("LoadCumulativeAgentsRange(%d, 0) = %+v, want only the minute-120 bucket", from, agents)
		}
	}

	// from <= 0 is all-time — identical to the cumulative loaders.
	all, err := store.LoadCumulativeRange(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if all[key].Input != 30 || all[key].CacheRead != 6 || all[key].TokenRequests != 2 {
		t.Errorf("LoadCumulativeRange(0, 0) = %+v, want all-time totals", all)
	}
	base, err := store.LoadCumulative()
	if err != nil {
		t.Fatal(err)
	}
	if base[key] != all[key] {
		t.Errorf("LoadCumulative() = %+v differs from LoadCumulativeRange(0, 0) = %+v", base[key], all[key])
	}
	allAgents, err := store.LoadCumulativeAgentsRange(-5, 0)
	if err != nil {
		t.Fatal(err)
	}
	if allAgents[agentKey].Input != 12 || allAgents[agentKey].CacheRead != 3 {
		t.Errorf("LoadCumulativeAgentsRange(-5, 0) = %+v, want all-time totals", allAgents)
	}

	// A window beyond all data aggregates to empty maps (no zero rows).
	empty, err := store.LoadCumulativeRange(1<<40, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty) != 0 {
		t.Errorf("LoadCumulativeRange(future) = %+v, want empty", empty)
	}
	emptyAgents, err := store.LoadCumulativeAgentsRange(1<<40, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(emptyAgents) != 0 {
		t.Errorf("LoadCumulativeAgentsRange(future) = %+v, want empty", emptyAgents)
	}

	// to bound: a to inside a minute includes that minute's bucket (bucket
	// start <= to); a to before it excludes it. Closed ranges work on both
	// sides, and an inverted range (from > to) aggregates to empty.
	closed, err := store.LoadCumulativeRange(0, 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(closed) != 1 || closed[key].Input != 10 || closed[key].CacheRead != 2 {
		t.Errorf("LoadCumulativeRange(0, 60) = %+v, want only the minute-60 bucket", closed)
	}
	closedInside, err := store.LoadCumulativeRange(0, 119)
	if err != nil {
		t.Fatal(err)
	}
	if len(closedInside) != 1 || closedInside[key].Input != 10 {
		t.Errorf("LoadCumulativeRange(0, 119) = %+v, want minute-60 only (120 not started by 119)", closedInside)
	}
	closedAgents, err := store.LoadCumulativeAgentsRange(120, 120)
	if err != nil {
		t.Fatal(err)
	}
	if len(closedAgents) != 1 || closedAgents[agentKey].Input != 7 {
		t.Errorf("LoadCumulativeAgentsRange(120, 120) = %+v, want only the minute-120 bucket", closedAgents)
	}
	inverted, err := store.LoadCumulativeRange(120, 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(inverted) != 0 {
		t.Errorf("LoadCumulativeRange(120, 60) = %+v, want empty for inverted range", inverted)
	}
	invertedAgents, err := store.LoadCumulativeAgentsRange(120, 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(invertedAgents) != 0 {
		t.Errorf("LoadCumulativeAgentsRange(120, 60) = %+v, want empty for inverted range", invertedAgents)
	}
}

func TestQueryPlansAvoidHeapScan(t *testing.T) {
	store := newTestStore(t, 0)
	now := time.Now().Unix() / 60 * 60
	if err := store.Flush(now, map[Key]Counters{{Provider: "z", Model: "m"}: {Requests: 1}}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		sql  string
		args []any
	}{
		{"range only", `SELECT minute FROM minute_buckets WHERE minute >= ? AND minute <= ?`, []any{now - 60, now + 60}},
		{"range provider", `SELECT minute FROM minute_buckets WHERE minute >= ? AND minute <= ? AND provider = ?`, []any{now - 60, now + 60, "z"}},
		{"key range", `SELECT minute FROM minute_buckets WHERE provider = ? AND model = ? AND minute >= ?`, []any{"z", "m", now - 60}},
		{"cumulative", `SELECT provider, model, SUM(requests), MAX(last_request_at) FROM minute_buckets GROUP BY provider, model`, nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			rows, err := store.db.Query(`EXPLAIN QUERY PLAN `+testCase.sql, testCase.args...)
			if err != nil {
				t.Fatal(err)
			}
			var plan strings.Builder
			for rows.Next() {
				var id, parent, unused int
				var detail string
				if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
					_ = rows.Close()
					t.Fatal(err)
				}
				fmt.Fprintf(&plan, "%d|%d|%s\n", id, parent, detail)
			}
			if err := rows.Close(); err != nil {
				t.Fatal(err)
			}
			for _, line := range strings.Split(plan.String(), "\n") {
				if strings.Contains(line, "SCAN minute_buckets") && !strings.Contains(line, "USING") {
					t.Errorf("heap scan in plan:\n%s", plan.String())
				}
			}
		})
	}
}

func TestJSONFieldContract(t *testing.T) {
	bucketJSON, err := json.Marshal(Bucket{})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		`"provider"`, `"model"`, `"minute"`, `"requests"`, `"failovers"`,
		`"rate_limited_429"`, `"failures"`, `"input"`, `"output"`,
		`"cache_creation"`, `"cache_read"`, `"token_requests"`,
		`"last_request_at"`, `"latency_ms_sum"`, `"ttft_ms_sum"`,
		`"avg_latency_ms"`, `"avg_ttft_ms"`,
	} {
		if !strings.Contains(string(bucketJSON), field) {
			t.Errorf("Bucket JSON missing %s: %s", field, bucketJSON)
		}
	}
	analyticsJSON, _ := json.Marshal(AnalyticsBucket{})
	for _, field := range []string{`"provider"`, `"model"`, `"bucket"`, `"requests"`, `"input"`, `"output"`, `"cache_creation"`, `"cache_read"`, `"last_request_at"`} {
		if !strings.Contains(string(analyticsJSON), field) {
			t.Errorf("AnalyticsBucket JSON missing %s: %s", field, analyticsJSON)
		}
	}
	agentJSON, _ := json.Marshal(AgentBucket{})
	for _, field := range []string{`"agent"`, `"provider"`, `"model"`, `"minute"`, `"requests"`, `"input"`, `"output"`, `"cache_creation"`, `"cache_read"`, `"latency_ms_sum"`, `"failures"`} {
		if !strings.Contains(string(agentJSON), field) {
			t.Errorf("AgentBucket JSON missing %s: %s", field, agentJSON)
		}
	}
}

func TestAverageMillisecondsAndLocalCalendarStartFailures(t *testing.T) {
	if got := averageMilliseconds(100, 0); got != 0 {
		t.Errorf("zero-request average = %v", got)
	}
	if got := averageMilliseconds(2, 3); got != 0.7 {
		t.Errorf("rounded average = %v, want 0.7", got)
	}
	if got := localCalendarStart("invalid-date", "day"); got != 0 {
		t.Errorf("invalid day = %d, want 0", got)
	}
	if got := localCalendarStart("x", "month"); got != 0 {
		t.Errorf("short invalid month = %d, want 0", got)
	}
}
