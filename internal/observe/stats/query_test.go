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
	if _, err := store.QueryAnalytics(0, 1, "", "", "year"); err == nil ||
		!strings.Contains(err.Error(), "minute, hour, day, week or month") {
		t.Fatalf("invalid granularity error = %v", err)
	}
}

func TestQueryAnalyticsHourBucketsAndWidenedCounters(t *testing.T) {
	store := newTestStore(t, 0)
	now := time.Now().In(time.Local)
	// Anchor to a whole local hour so the label round-trip is exact.
	hourStart := now.Truncate(time.Hour)
	first := hourStart.Add(10*time.Minute).Unix() / 60 * 60
	second := hourStart.Add(20*time.Minute).Unix() / 60 * 60
	nextHour := hourStart.Add(time.Hour).Add(5*time.Minute).Unix() / 60 * 60
	key := Key{Provider: "p", Model: "m"}
	for minute, counters := range map[int64]Counters{
		first:    {Requests: 2, Failovers: 1, RateLimited429: 1, Failures: 1, Input: 10, Output: 20, LatencySum: 300, TTFTSum: 30, DurationSum: 1000, LastRequestAt: 100},
		second:   {Requests: 2, Failovers: 2, RateLimited429: 0, Failures: 0, Input: 30, Output: 40, LatencySum: 500, TTFTSum: 50, DurationSum: 1400, LastRequestAt: 300},
		nextHour: {Requests: 1, Input: 50, Output: 60, LatencySum: 800, TTFTSum: 80, LastRequestAt: 200},
	} {
		if err := store.Flush(minute, map[Key]Counters{key: counters}); err != nil {
			t.Fatal(err)
		}
	}

	buckets, err := store.QueryAnalytics(first-60, nextHour+60, "p", "m", "hour")
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 2 {
		t.Fatalf("hour buckets = %d, want 2: %+v", len(buckets), buckets)
	}
	if buckets[0].Bucket != hourStart.Unix() {
		t.Errorf("first hour bucket = %d, want %d (local hour start)", buckets[0].Bucket, hourStart.Unix())
	}
	if buckets[0].Requests != 4 || buckets[0].Failovers != 3 || buckets[0].RateLimited429 != 1 ||
		buckets[0].Failures != 1 || buckets[0].Input != 40 || buckets[0].Output != 60 ||
		buckets[0].LastRequestAt != 300 {
		t.Errorf("first hour counters = %+v", buckets[0])
	}
	// Averages are requests-weighted over the whole bucket: (300+500)/4 = 200
	// latency, (1000+1400)/4 = 600 full-call duration (the tok/s denominator).
	if buckets[0].AvgLatencyMs != 200 || buckets[0].AvgTtftMs != 20 || buckets[0].AvgDurationMs != 600 {
		t.Errorf("hour averages = %.1f/%.1f/%.1f, want 200/20/600", buckets[0].AvgLatencyMs, buckets[0].AvgTtftMs, buckets[0].AvgDurationMs)
	}
	if buckets[1].Bucket != hourStart.Add(time.Hour).Unix() || buckets[1].Requests != 1 {
		t.Errorf("second hour bucket = %+v", buckets[1])
	}
}

func TestQueryAnalyticsMinuteAndWeekBuckets(t *testing.T) {
	store := newTestStore(t, 0)
	now := time.Now().In(time.Local)
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	// Anchor on THIS week's Monday (local weeks start Monday): Wednesday
	// 12:34 and the same week's Friday 01:00 must fold into one bucket; the
	// previous Wednesday belongs to the week before.
	monday := dayStart.AddDate(0, 0, -((int(dayStart.Weekday()) + 6) % 7))
	wed := monday.AddDate(0, 0, 2).Add(12*time.Hour + 34*time.Minute)
	fri := monday.AddDate(0, 0, 4).Add(time.Hour)
	prevWed := wed.AddDate(0, 0, -7)
	min := func(ts time.Time) int64 { return ts.Unix() / 60 * 60 }
	key := Key{Provider: "p", Model: "m"}
	for minute, counters := range map[int64]Counters{
		min(wed):     {Requests: 1, Input: 10, LatencySum: 100},
		min(fri):     {Requests: 2, Input: 20, LatencySum: 200},
		min(prevWed): {Requests: 4, Input: 40, LatencySum: 400},
	} {
		if err := store.Flush(minute, map[Key]Counters{key: counters}); err != nil {
			t.Fatal(err)
		}
	}

	weeks, err := store.QueryAnalytics(min(prevWed)-60, min(fri)+3600, "p", "m", "week")
	if err != nil {
		t.Fatal(err)
	}
	if len(weeks) != 2 {
		t.Fatalf("week buckets = %d, want 2: %+v", len(weeks), weeks)
	}
	// Local weeks start Monday.
	wantMonday := func(ts time.Time) time.Time {
		off := (int(ts.Weekday()) + 6) % 7 // Sunday→6
		return time.Date(ts.Year(), ts.Month(), ts.Day()-off, 0, 0, 0, 0, time.Local)
	}
	if weeks[0].Bucket != wantMonday(prevWed).Unix() || weeks[0].Requests != 4 {
		t.Errorf("prev week = %+v, want Monday %d with 4 reqs", weeks[0], wantMonday(prevWed).Unix())
	}
	if weeks[1].Bucket != wantMonday(wed).Unix() || weeks[1].Requests != 3 || weeks[1].Input != 30 || weeks[1].AvgLatencyMs != 100 {
		t.Errorf("this week = %+v", weeks[1])
	}

	minutes, err := store.QueryAnalytics(min(wed), min(wed)+60, "p", "m", "minute")
	if err != nil {
		t.Fatal(err)
	}
	if len(minutes) != 1 || minutes[0].Bucket != min(wed) || minutes[0].Requests != 1 {
		t.Errorf("minute buckets = %+v", minutes)
	}
	if _, err := store.QueryAnalytics(0, 1, "", "", "year"); err == nil ||
		!strings.Contains(err.Error(), "minute, hour, day, week or month") {
		t.Fatalf("invalid granularity error = %v", err)
	}
}

func TestQueryAnalyticsAgents(t *testing.T) {
	store := newTestStore(t, 0)
	now := time.Now().In(time.Local)
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	minute := dayStart.Add(3*time.Hour).Unix() / 60 * 60
	if err := store.FlushAgents(minute, map[AgentKey]AgentCounters{
		{Agent: "codex", Provider: "zhipu", Model: "glm-5"}:       {Requests: 3, Input: 100, Output: 200, CacheCreation: 5, CacheRead: 40, LatencySum: 1500, TTFTSum: 150, Failures: 1},
		{Agent: "claude-code", Provider: "zhipu", Model: "glm-5"}: {Requests: 7, Input: 10, Output: 20},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FlushAgents(minute+60, map[AgentKey]AgentCounters{
		{Agent: "codex", Provider: "zhipu", Model: "glm-5"}: {Requests: 1, Input: 50, Output: 60, LatencySum: 500, TTFTSum: 50},
	}); err != nil {
		t.Fatal(err)
	}

	buckets, err := store.QueryAnalyticsAgents(minute-60, minute+120, "", "zhipu", "", "day")
	if err != nil {
		t.Fatal(err)
	}
	if len(buckets) != 2 {
		t.Fatalf("agent day buckets = %d, want 2: %+v", len(buckets), buckets)
	}
	codex, claude := buckets[0], buckets[1]
	if codex.Agent != "claude-code" || claude.Agent != "codex" {
		t.Fatalf("agent order = %s/%s, want claude-code first", codex.Agent, claude.Agent)
	}
	if codex.Requests != 7 || codex.Input != 10 || codex.Output != 20 || codex.Failures != 0 {
		t.Errorf("claude-code day = %+v", codex)
	}
	if claude.Requests != 4 || claude.Input != 150 || claude.Output != 260 || claude.CacheRead != 40 ||
		claude.Failures != 1 || claude.LatencySum != 2000 || claude.AvgLatencyMs != 500 ||
		claude.TTFTSum != 200 || claude.AvgTtftMs != 50 {
		t.Errorf("codex day = %+v", claude)
	}
	// agent_buckets has no failover/429 columns — those stay zero (ttft is
	// now recorded; asserted above).
	if claude.Failovers != 0 || claude.RateLimited429 != 0 {
		t.Errorf("agent dimension must not fabricate failover/429: %+v", claude)
	}
	if claude.Bucket != dayStart.Unix() {
		t.Errorf("agent day bucket = %d, want %d", claude.Bucket, dayStart.Unix())
	}

	filtered, err := store.QueryAnalyticsAgents(minute-60, minute+120, "codex", "zhipu", "", "day")
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].Agent != "codex" {
		t.Fatalf("agent filter = %+v", filtered)
	}
	if _, err := store.QueryAnalyticsAgents(0, 1, "", "", "", "year"); err == nil ||
		!strings.Contains(err.Error(), "minute, hour, day, week or month") {
		t.Fatalf("invalid granularity error = %v", err)
	}
}

func TestFlushAndQueryAgentsRawWideAndFilters(t *testing.T) {
	store := newTestStore(t, 0)
	key := AgentKey{Agent: "codex", Provider: "zhipu", Model: "glm-5"}
	if err := store.FlushAgents(0, map[AgentKey]AgentCounters{
		key: {Requests: 3, Input: 100, Output: 200, CacheCreation: 5, CacheRead: 40, LatencySum: 1500, TTFTSum: 150, Failures: 0},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FlushAgents(60, map[AgentKey]AgentCounters{
		key: {Requests: 5, Input: 400, Output: 800, CacheCreation: 10, CacheRead: 60, LatencySum: 6000, TTFTSum: 600, Failures: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FlushAgents(60, map[AgentKey]AgentCounters{
		key: {Requests: 2, Input: 10, Output: 20, CacheRead: 4, LatencySum: 500, TTFTSum: 50, Failures: 2},
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
		raw[1].LatencySum != 6500 || raw[1].TTFTSum != 650 || raw[1].Failures != 3 {
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
		got.LatencySum != 8000 || got.TTFTSum != 800 || got.Failures != 3 {
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
	for _, field := range []string{`"provider"`, `"model"`, `"bucket"`, `"requests"`, `"failovers"`, `"rate_limited_429"`, `"failures"`, `"input"`, `"output"`, `"cache_creation"`, `"cache_read"`, `"latency_ms_sum"`, `"ttft_ms_sum"`, `"last_request_at"`, `"avg_latency_ms"`, `"avg_ttft_ms"`} {
		if !strings.Contains(string(analyticsJSON), field) {
			t.Errorf("AnalyticsBucket JSON missing %s: %s", field, analyticsJSON)
		}
	}
	// The agent dimension rides the same struct: the field is omitempty, so
	// assert it on a bucket that actually carries an agent.
	if agentAnalytics, _ := json.Marshal(AnalyticsBucket{Agent: "codex"}); !strings.Contains(string(agentAnalytics), `"agent":"codex"`) {
		t.Errorf("AnalyticsBucket JSON missing agent: %s", agentAnalytics)
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
	if got := localCalendarStart("invalid-date", "2006-01-02", "day"); got != 0 {
		t.Errorf("invalid day = %d, want 0", got)
	}
	if got := localCalendarStart("x", "2006-01", "month"); got != 0 {
		t.Errorf("short invalid month = %d, want 0", got)
	}
}

func TestQueryAgentNamesWindowAndFilters(t *testing.T) {
	store := newTestStore(t, 0)
	mon := time.Date(2026, 1, 5, 9, 30, 0, 0, time.Local)
	min := func(ts time.Time) int64 { return ts.Unix() / 60 * 60 }
	if err := store.FlushAgents(min(mon), map[AgentKey]AgentCounters{
		{Agent: "codex", Provider: "p", Model: "m"}:  {Requests: 2, Input: 10, Output: 5},
		{Agent: "pi", Provider: "other", Model: "m"}: {Requests: 3, Input: 7},
	}); err != nil {
		t.Fatal(err)
	}

	names, err := store.QueryAgentNames(min(mon)-60, min(mon)+60, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "codex" || names[1] != "pi" {
		t.Fatalf("agent names = %+v, want [codex pi]", names)
	}
	filtered, err := store.QueryAgentNames(min(mon)-60, min(mon)+60, "p", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0] != "codex" {
		t.Fatalf("provider-filtered names = %+v", filtered)
	}
	outOfRange, err := store.QueryAgentNames(min(mon)+120, min(mon)+180, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(outOfRange) != 0 {
		t.Fatalf("out-of-range names = %+v", outOfRange)
	}
}

// TestEarliestMinuteAllCoversMCPTables pins the all-tables all-time anchor:
// EarliestMinute stays LLM-only (minute_buckets/agent_buckets — the /api/tokens
// "Since" label and the LLM analytics clamp must not shift when MCP usage
// predates the first LLM bucket), while EarliestMinuteAll additionally spans
// the MCP pair (mcp_buckets AND mcp_tool_buckets) so from=0 windows on MCP
// surfaces keep MCP history that predates the first LLM bucket.
func TestEarliestMinuteAllCoversMCPTables(t *testing.T) {
	store := newTestStore(t, 0)
	if got := store.EarliestMinute(); got != 0 {
		t.Fatalf("empty store EarliestMinute = %d, want 0", got)
	}
	if got := store.EarliestMinuteAll(); got != 0 {
		t.Fatalf("empty store EarliestMinuteAll = %d, want 0", got)
	}

	// Oldest persisted bucket is a per-tool MCP row; the per-name MCP row and
	// the first LLM usage come later.
	const (
		toolMinute = int64(60)
		nameMinute = int64(120)
		llmMinute  = int64(3600)
	)
	if err := store.FlushMCPToolBuckets(toolMinute, []MCPToolBucketDelta{
		{Name: "web-search", Tool: "search", Kind: MCPKindServer, Calls: 1, LatencyMsSum: 10, LastCallAt: toolMinute},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FlushMCPBuckets(nameMinute, []MCPBucketDelta{
		{Name: "web-search", Kind: MCPKindServer, Calls: 1, LatencyMsSum: 10, LastCallAt: nameMinute},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Flush(llmMinute, map[Key]Counters{
		{Provider: "z", Model: "m"}: {Requests: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FlushAgents(llmMinute, map[AgentKey]AgentCounters{
		{Agent: "codex"}: {Requests: 1},
	}); err != nil {
		t.Fatal(err)
	}

	if got := store.EarliestMinute(); got != llmMinute {
		t.Fatalf("EarliestMinute = %d, want %d (LLM tables only)", got, llmMinute)
	}
	if got := store.EarliestMinuteAll(); got != toolMinute {
		t.Fatalf("EarliestMinuteAll = %d, want %d (the MCP tool bucket is the oldest persisted bucket)", got, toolMinute)
	}
}
