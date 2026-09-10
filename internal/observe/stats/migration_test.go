package stats

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestImportLegacyTokensOnce(t *testing.T) {
	store := newTestStore(t, 0)
	path := filepath.Join(t.TempDir(), "token_usage.json")
	legacy := map[string]legacyTokenUsage{
		"zhipu\x00glm-5": {
			Input:         42,
			Output:        8,
			CacheCreation: 2,
			CacheRead:     3,
			Requests:      4,
		},
		"provider-only": {Input: 5},
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	mtime := time.Unix(1_700_000_123, 0)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}

	imported, err := store.ImportLegacyTokens(path)
	if err != nil {
		t.Fatal(err)
	}
	if imported != 2 {
		t.Fatalf("imported = %d, want 2", imported)
	}
	cumulative, err := store.LoadCumulative()
	if err != nil {
		t.Fatal(err)
	}
	got := cumulative[Key{Provider: "zhipu", Model: "glm-5"}]
	if got.Input != 42 || got.Output != 8 || got.CacheCreation != 2 ||
		got.CacheRead != 3 || got.TokenRequests != 4 {
		t.Errorf("legacy counters = %+v", got)
	}
	if cumulative[Key{Provider: "provider-only", Model: ""}].Input != 5 {
		t.Errorf("unsplit legacy key = %+v", cumulative)
	}
	rows, err := store.QueryRange(mtime.Unix()-120, mtime.Unix()+120, "", "", 60)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Minute != mtime.Unix()/60*60 {
			t.Errorf("legacy minute = %d, want mtime floor %d", row.Minute, mtime.Unix()/60*60)
		}
	}

	again, err := store.ImportLegacyTokens(path)
	if err != nil {
		t.Fatal(err)
	}
	if again != 0 {
		t.Errorf("second import = %d, want 0", again)
	}
}

func TestImportLegacyTokensMissingInvalidAndNonEmpty(t *testing.T) {
	store := newTestStore(t, 0)
	if imported, err := store.ImportLegacyTokens(filepath.Join(t.TempDir(), "missing.json")); err != nil || imported != 0 {
		t.Fatalf("missing import = %d, %v; want 0, nil", imported, err)
	}
	invalid := filepath.Join(t.TempDir(), "invalid.json")
	if err := os.WriteFile(invalid, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ImportLegacyTokens(invalid); err == nil ||
		!strings.Contains(err.Error(), "parse legacy token_usage.json") {
		t.Fatalf("invalid import error = %v", err)
	}
	if err := store.Flush(60, map[Key]Counters{{Provider: "p", Model: "m"}: {Requests: 1}}); err != nil {
		t.Fatal(err)
	}
	if imported, err := store.ImportLegacyTokens(invalid); err != nil || imported != 0 {
		t.Fatalf("nonempty import = %d, %v; want 0, nil", imported, err)
	}
}

func TestAdditiveMigrationForLegacySchemasIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open(driver, "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE minute_buckets (
		provider TEXT NOT NULL,
		model TEXT NOT NULL,
		minute INTEGER NOT NULL,
		requests INTEGER NOT NULL DEFAULT 0,
		failovers INTEGER NOT NULL DEFAULT 0,
		rate_limited_429 INTEGER NOT NULL DEFAULT 0,
		failures INTEGER NOT NULL DEFAULT 0,
		input INTEGER NOT NULL DEFAULT 0,
		output INTEGER NOT NULL DEFAULT 0,
		cache_creation INTEGER NOT NULL DEFAULT 0,
		cache_read INTEGER NOT NULL DEFAULT 0,
		token_requests INTEGER NOT NULL DEFAULT 0,
		last_request_at INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (provider, model, minute)
	);
	CREATE TABLE agent_buckets (
		agent TEXT NOT NULL,
		provider TEXT NOT NULL,
		model TEXT NOT NULL,
		minute INTEGER NOT NULL,
		requests INTEGER NOT NULL DEFAULT 0,
		input INTEGER NOT NULL DEFAULT 0,
		output INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (agent, provider, model, minute)
	);
	INSERT INTO minute_buckets
		(provider, model, minute, requests, input, last_request_at)
		VALUES ('legacy-provider', 'legacy-model', 60, 2, 20, 100);
	INSERT INTO agent_buckets
		(agent, provider, model, minute, requests, input, output)
		VALUES ('legacy-agent', 'legacy-provider', 'legacy-model', 60, 3, 30, 10);`)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	assertColumns(t, store, "minute_buckets", "latency_ms_sum", "ttft_ms_sum")
	assertColumns(t, store, "agent_buckets", "latency_ms_sum", "failures", "cache_creation", "cache_read")

	statsRows, err := store.QueryRange(0, 120, "", "", 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(statsRows) != 1 || statsRows[0].Requests != 2 || statsRows[0].Input != 20 ||
		statsRows[0].LatencySum != 0 || statsRows[0].TTFTSum != 0 {
		t.Errorf("migrated minute row = %+v", statsRows)
	}
	agentRows, err := store.QueryAgents(0, 120, "", "", "", 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(agentRows) != 1 || agentRows[0].Requests != 3 || agentRows[0].Input != 30 ||
		agentRows[0].Output != 10 || agentRows[0].CacheCreation != 0 || agentRows[0].CacheRead != 0 ||
		agentRows[0].LatencySum != 0 || agentRows[0].Failures != 0 {
		t.Errorf("migrated agent row = %+v", agentRows)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(Options{Path: path})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	assertColumns(t, reopened, "minute_buckets", "latency_ms_sum", "ttft_ms_sum")
	assertColumns(t, reopened, "agent_buckets", "latency_ms_sum", "failures", "cache_creation", "cache_read")
	if err := reopened.Flush(60, map[Key]Counters{
		{Provider: "legacy-provider", Model: "legacy-model"}: {
			Requests: 1, Input: 5, LastRequestAt: 200, LatencySum: 400, TTFTSum: 100,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := reopened.FlushAgents(60, map[AgentKey]AgentCounters{
		{Agent: "legacy-agent", Provider: "legacy-provider", Model: "legacy-model"}: {
			Requests: 2, Input: 5, Output: 6, CacheCreation: 3, CacheRead: 7, LatencySum: 700, Failures: 1,
		},
	}); err != nil {
		t.Fatal(err)
	}
	statsRows, _ = reopened.QueryRange(0, 120, "", "", 60)
	if len(statsRows) != 1 || statsRows[0].Requests != 3 || statsRows[0].Input != 25 ||
		statsRows[0].LastRequestAt != 200 || statsRows[0].LatencySum != 400 ||
		statsRows[0].TTFTSum != 100 {
		t.Errorf("post-reopen minute upsert = %+v", statsRows)
	}
	agentRows, _ = reopened.QueryAgents(0, 120, "", "", "", 60)
	if len(agentRows) != 1 || agentRows[0].Requests != 5 || agentRows[0].Input != 35 ||
		agentRows[0].Output != 16 || agentRows[0].CacheCreation != 3 || agentRows[0].CacheRead != 7 ||
		agentRows[0].LatencySum != 700 || agentRows[0].Failures != 1 {
		t.Errorf("post-reopen agent upsert = %+v", agentRows)
	}
}

func assertColumns(t *testing.T, store *Store, table string, expected ...string) {
	t.Helper()
	columns, err := store.columnSet(table)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range expected {
		if !columns[name] {
			t.Errorf("%s missing column %s: %+v", table, name, columns)
		}
	}
}
