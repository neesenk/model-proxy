package stats

import (
	"testing"
	"time"

	obscounters "model-proxy/internal/observe/counters"
)

func TestFlushMCPBucketsUpsertsInSameMinute(t *testing.T) {
	store := newTestStore(t, 0)
	minute := int64(60)
	first := []MCPBucketDelta{
		{Name: "web-search", Kind: MCPKindServer, Calls: 2, Errors: 1, LatencyMsSum: 200, LastCallAt: 100},
		{Name: "exa", Kind: MCPKindRoute, Calls: 1, LatencyMsSum: 50, LastCallAt: 90},
	}
	second := []MCPBucketDelta{
		{Name: "web-search", Kind: MCPKindServer, Calls: 3, Errors: 0, LatencyMsSum: 400, LastCallAt: 150},
	}
	if err := store.FlushMCPBuckets(minute, first); err != nil {
		t.Fatal(err)
	}
	if err := store.FlushMCPBuckets(minute, second); err != nil {
		t.Fatal(err)
	}

	rows, err := store.QueryMCPBuckets(minute, minute, "minute")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2: %+v", len(rows), rows)
	}
	byName := map[string]MCPBucketRow{}
	for _, r := range rows {
		byName[r.Name] = r
	}
	ws := byName["web-search"]
	if ws.Calls != 5 || ws.Errors != 1 || ws.LatencyMsSum != 600 || ws.LastCallAt != 150 {
		t.Errorf("web-search = %+v", ws)
	}
	exa := byName["exa"]
	if exa.Calls != 1 || exa.Kind != "route" || exa.LastCallAt != 90 {
		t.Errorf("exa = %+v", exa)
	}
}

func TestQueryMCPBucketsAggregatesByGranularity(t *testing.T) {
	store := newTestStore(t, 0)
	// Two consecutive hours in local time; use a fixed reference minute far from
	// any DST boundary to keep calendar math deterministic.
	base := time.Date(2026, 9, 20, 0, 0, 0, 0, time.Local).Unix()
	minutes := []int64{base, base + 3600, base + 7200}
	for _, m := range minutes {
		if err := store.FlushMCPBuckets(m, []MCPBucketDelta{
			{Name: "srv", Kind: MCPKindServer, Calls: 1, LatencyMsSum: 100, LastCallAt: m},
		}); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		granularity string
		wantBuckets int
	}{
		{"minute", 3},
		{"hour", 3},
		{"day", 1},
	}
	for _, c := range cases {
		rows, err := store.QueryMCPBuckets(base, base+7200, c.granularity)
		if err != nil {
			t.Fatalf("%s: %v", c.granularity, err)
		}
		if len(rows) != c.wantBuckets {
			t.Errorf("%s: buckets = %d, want %d", c.granularity, len(rows), c.wantBuckets)
			continue
		}
		var total uint64
		for _, r := range rows {
			total += r.Calls
			if r.Name != "srv" || r.Kind != "server" {
				t.Errorf("%s: row = %+v", c.granularity, r)
			}
		}
		if total != 3 {
			t.Errorf("%s: total calls = %d, want 3", c.granularity, total)
		}
	}
}

func TestQueryMCPBucketsInvalidGranularity(t *testing.T) {
	store := newTestStore(t, 0)
	if _, err := store.QueryMCPBuckets(0, 60, "year"); err == nil {
		t.Fatal("expected error for invalid granularity")
	}
}

func TestQueryMCPBucketsEmptyRange(t *testing.T) {
	store := newTestStore(t, 0)
	if err := store.FlushMCPBuckets(60, []MCPBucketDelta{
		{Name: "srv", Kind: MCPKindServer, Calls: 1, LatencyMsSum: 10, LastCallAt: 60},
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := store.QueryMCPBuckets(120, 180, "minute")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("empty range returned %d rows", len(rows))
	}
}

func TestPruneRemovesMCPBuckets(t *testing.T) {
	now := time.Now()
	old := now.Add(-2*time.Hour).Unix() / 60 * 60
	recent := now.Unix() / 60 * 60
	store := newTestStore(t, time.Hour)
	if err := store.FlushMCPBuckets(old, []MCPBucketDelta{
		{Name: "old", Kind: MCPKindServer, Calls: 1, LatencyMsSum: 10, LastCallAt: old},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FlushMCPBuckets(recent, []MCPBucketDelta{
		{Name: "new", Kind: MCPKindRoute, Calls: 2, LatencyMsSum: 20, LastCallAt: recent},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Prune(now); err != nil {
		t.Fatal(err)
	}
	rows, err := store.QueryMCPBuckets(0, recent+60, "minute")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Name != "new" {
		t.Errorf("pruned rows = %+v", rows)
	}
}

func TestDiffMCPCountersAndReset(t *testing.T) {
	kind := func(name string) (MCPKind, bool) {
		if name == "srv" {
			return MCPKindServer, true
		}
		return MCPKindRoute, true
	}
	prev := map[string]obscounters.MCPStatRaw{
		"srv": {Calls: 10, Errors: 2, LatencySum: 1000, LastCallAt: 100},
	}
	cur := map[string]obscounters.MCPStatRaw{
		"srv": {Calls: 13, Errors: 3, LatencySum: 1300, LastCallAt: 200},
		"rt":  {Calls: 5, Errors: 1, LatencySum: 500, LastCallAt: 150},
	}
	deltas := DiffMCP(cur, prev, kind)
	byName := map[string]MCPBucketDelta{}
	for _, d := range deltas {
		byName[d.Name] = d
	}
	if len(byName) != 2 {
		t.Fatalf("deltas = %+v", deltas)
	}
	if s := byName["srv"]; s.Calls != 3 || s.Errors != 1 || s.LatencyMsSum != 300 || s.LastCallAt != 200 {
		t.Errorf("srv delta = %+v", s)
	}
	if r := byName["rt"]; r.Calls != 5 || r.Kind != MCPKindRoute {
		t.Errorf("rt delta = %+v", r)
	}

	// Simulate a counter reset: current is lower than previous. The delta should
	// be the current value, not a clamped zero.
	reset := map[string]obscounters.MCPStatRaw{
		"srv": {Calls: 2, Errors: 1, LatencySum: 150, LastCallAt: 300},
	}
	deltas = DiffMCP(reset, prev, kind)
	if len(deltas) != 1 {
		t.Fatalf("reset deltas = %+v", deltas)
	}
	if d := deltas[0]; d.Calls != 2 || d.Errors != 1 || d.LatencyMsSum != 150 {
		t.Errorf("reset delta = %+v", d)
	}
}

func TestDiffMCPUnknownName(t *testing.T) {
	kind := func(string) (MCPKind, bool) { return "", false }
	cur := map[string]obscounters.MCPStatRaw{"ghost": {Calls: 1}}
	if deltas := DiffMCP(cur, nil, kind); len(deltas) != 0 {
		t.Errorf("unknown name should be skipped, got %+v", deltas)
	}
}

func TestResetCoversMCPBuckets(t *testing.T) {
	store := newTestStore(t, 0)
	if err := store.FlushMCPBuckets(60, []MCPBucketDelta{
		{Name: "srv", Kind: MCPKindServer, Calls: 1, LatencyMsSum: 10, LastCallAt: 60},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Reset(); err != nil {
		t.Fatal(err)
	}
	rows, err := store.QueryMCPBuckets(0, 120, "minute")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("after Reset: %d MCP rows", len(rows))
	}
}

func TestFlushMCPToolBucketsUpsertsInSameMinute(t *testing.T) {
	store := newTestStore(t, 0)
	minute := int64(60)
	if err := store.FlushMCPToolBuckets(minute, []MCPToolBucketDelta{
		{Name: "web-search", Tool: "search", Kind: MCPKindServer, Calls: 2, Errors: 1, LatencyMsSum: 200, LastCallAt: 100},
		{Name: "web-search", Tool: "fetch", Kind: MCPKindServer, Calls: 1, LatencyMsSum: 50, LastCallAt: 90},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FlushMCPToolBuckets(minute, []MCPToolBucketDelta{
		{Name: "web-search", Tool: "search", Kind: MCPKindServer, Calls: 3, Errors: 0, LatencyMsSum: 400, LastCallAt: 150},
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := store.QueryMCPToolBuckets(minute, minute, "minute")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2: %+v", len(rows), rows)
	}
	byTool := map[string]MCPToolBucketRow{}
	for _, r := range rows {
		byTool[r.Tool] = r
	}
	search := byTool["search"]
	if search.Calls != 5 || search.Errors != 1 || search.LatencyMsSum != 600 || search.LastCallAt != 150 || search.Kind != "server" {
		t.Errorf("search = %+v", search)
	}
	if fetch := byTool["fetch"]; fetch.Calls != 1 || fetch.AvgLatencyMs != 50 {
		t.Errorf("fetch = %+v", fetch)
	}
}

// TestFlushMCPToolBucketsUpdatesKindOnConflict: kind is not part of the
// (name, tool, minute) primary key, so a same-key flush after a reload
// reclassified the exposed name (server ↔ route) must move the row's kind to
// the current classification instead of keeping the stale one.
func TestFlushMCPToolBucketsUpdatesKindOnConflict(t *testing.T) {
	store := newTestStore(t, 0)
	minute := int64(60)
	if err := store.FlushMCPToolBuckets(minute, []MCPToolBucketDelta{
		{Name: "web-search", Tool: "search", Kind: MCPKindServer, Calls: 2, LatencyMsSum: 100, LastCallAt: 60},
	}); err != nil {
		t.Fatal(err)
	}
	// Same (name, tool, minute), kind reclassified server → route (cross-reload).
	if err := store.FlushMCPToolBuckets(minute, []MCPToolBucketDelta{
		{Name: "web-search", Tool: "search", Kind: MCPKindRoute, Calls: 1, LatencyMsSum: 50, LastCallAt: 90},
	}); err != nil {
		t.Fatal(err)
	}

	rows, err := store.QueryMCPToolBuckets(minute, minute, "minute")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %+v, want the same-key row upserted in place", rows)
	}
	row := rows[0]
	if row.Kind != "route" {
		t.Fatalf("kind = %q, want route (reclassified by the later flush)", row.Kind)
	}
	if row.Calls != 3 || row.LatencyMsSum != 150 || row.LastCallAt != 90 {
		t.Fatalf("counters = %+v, want accumulated calls=3 latency_ms_sum=150 last_call_at=90", row)
	}
}

func TestQueryMCPToolBucketsAggregatesByGranularity(t *testing.T) {
	store := newTestStore(t, 0)
	base := time.Date(2026, 9, 20, 0, 0, 0, 0, time.Local).Unix()
	for _, m := range []int64{base, base + 3600, base + 7200} {
		if err := store.FlushMCPToolBuckets(m, []MCPToolBucketDelta{
			{Name: "srv", Tool: "search", Kind: MCPKindServer, Calls: 1, LatencyMsSum: 100, LastCallAt: m},
		}); err != nil {
			t.Fatal(err)
		}
	}

	cases := []struct {
		granularity string
		wantBuckets int
	}{
		{"minute", 3},
		{"hour", 3},
		{"day", 1},
	}
	for _, c := range cases {
		rows, err := store.QueryMCPToolBuckets(base, base+7200, c.granularity)
		if err != nil {
			t.Fatalf("%s: %v", c.granularity, err)
		}
		if len(rows) != c.wantBuckets {
			t.Errorf("%s: buckets = %d, want %d", c.granularity, len(rows), c.wantBuckets)
			continue
		}
		var total uint64
		for _, r := range rows {
			total += r.Calls
			if r.Name != "srv" || r.Tool != "search" || r.Kind != "server" {
				t.Errorf("%s: row = %+v", c.granularity, r)
			}
		}
		if total != 3 {
			t.Errorf("%s: total calls = %d, want 3", c.granularity, total)
		}
	}
}

func TestQueryMCPToolBucketsInvalidGranularity(t *testing.T) {
	store := newTestStore(t, 0)
	if _, err := store.QueryMCPToolBuckets(0, 60, "year"); err == nil {
		t.Fatal("expected error for invalid granularity")
	}
}

func TestDiffMCPTools(t *testing.T) {
	kind := func(name string) (MCPKind, bool) {
		if name == "srv" {
			return MCPKindServer, true
		}
		return MCPKindRoute, true
	}
	prev := map[obscounters.MCPToolKey]obscounters.MCPStatRaw{
		{Name: "srv", Tool: "search"}: {Calls: 10, Errors: 2, LatencySum: 1000, LastCallAt: 100},
	}
	cur := map[obscounters.MCPToolKey]obscounters.MCPStatRaw{
		{Name: "srv", Tool: "search"}: {Calls: 13, Errors: 3, LatencySum: 1300, LastCallAt: 200},
		{Name: "rt", Tool: "lookup"}:  {Calls: 5, Errors: 1, LatencySum: 500, LastCallAt: 150},
	}
	deltas := DiffMCPTools(cur, prev, kind)
	if len(deltas) != 2 {
		t.Fatalf("deltas = %+v", deltas)
	}
	byKey := map[[2]string]MCPToolBucketDelta{}
	for _, d := range deltas {
		byKey[[2]string{d.Name, d.Tool}] = d
	}
	if s := byKey[[2]string{"srv", "search"}]; s.Calls != 3 || s.Errors != 1 || s.LatencyMsSum != 300 || s.LastCallAt != 200 || s.Kind != MCPKindServer {
		t.Errorf("srv/search delta = %+v", s)
	}
	if r := byKey[[2]string{"rt", "lookup"}]; r.Calls != 5 || r.Kind != MCPKindRoute {
		t.Errorf("rt/lookup delta = %+v", r)
	}

	// Counter reset: current lower than previous — delta is the current value.
	reset := map[obscounters.MCPToolKey]obscounters.MCPStatRaw{
		{Name: "srv", Tool: "search"}: {Calls: 2, Errors: 1, LatencySum: 150, LastCallAt: 300},
	}
	deltas = DiffMCPTools(reset, prev, kind)
	if len(deltas) != 1 {
		t.Fatalf("reset deltas = %+v", deltas)
	}
	if d := deltas[0]; d.Calls != 2 || d.Errors != 1 || d.LatencyMsSum != 150 {
		t.Errorf("reset delta = %+v", d)
	}
}

func TestPruneCoversMCPToolBuckets(t *testing.T) {
	now := time.Now()
	old := now.Add(-2*time.Hour).Unix() / 60 * 60
	recent := now.Unix() / 60 * 60
	store := newTestStore(t, time.Hour)
	if err := store.FlushMCPToolBuckets(old, []MCPToolBucketDelta{
		{Name: "old", Tool: "search", Kind: MCPKindServer, Calls: 1, LatencyMsSum: 10, LastCallAt: old},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FlushMCPToolBuckets(recent, []MCPToolBucketDelta{
		{Name: "new", Tool: "search", Kind: MCPKindServer, Calls: 2, LatencyMsSum: 20, LastCallAt: recent},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Prune(now); err != nil {
		t.Fatal(err)
	}
	rows, err := store.QueryMCPToolBuckets(0, recent+60, "minute")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Name != "new" {
		t.Errorf("pruned tool rows = %+v", rows)
	}
}

func TestResetCoversMCPToolBuckets(t *testing.T) {
	store := newTestStore(t, 0)
	if err := store.FlushMCPToolBuckets(60, []MCPToolBucketDelta{
		{Name: "srv", Tool: "search", Kind: MCPKindServer, Calls: 1, LatencyMsSum: 10, LastCallAt: 60},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Reset(); err != nil {
		t.Fatal(err)
	}
	rows, err := store.QueryMCPToolBuckets(0, 120, "minute")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("after Reset: %d MCP tool rows", len(rows))
	}
}
