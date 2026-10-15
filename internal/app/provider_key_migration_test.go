package app

// provider_key_migration_test.go — regression coverage for the startup key
// migration: history recorded under a pooled parent's bare name (the
// pre-virtual-key single-account era) must follow the account onto the
// virtual "name#<accountID>" key — persisted stats buckets, the seeded
// in-memory counters, and the quota tracker's last-known-good file. The
// motivating bug: adding a second account to a provider made the first
// account's entire usage history invisible (rows stayed under the bare name
// while every view queried the new virtual keys).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"model-proxy/internal/accounts"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/observe/counters"
	observestats "model-proxy/internal/observe/stats"
	"model-proxy/internal/providerbuild"
)

func TestProviderKeyRenames_AttributesBareHistoryToEarliestAccount(t *testing.T) {
	renames := providerKeyRenames(map[string][]providerbuild.PoolAccount{
		// Single account: unambiguous.
		"single": {{ID: "aaa", AddedAt: "2026-07-15T00:00:00Z"}},
		// Multiple accounts: the earliest added_at wins (the bare history was
		// written while only that account existed).
		"multi": {
			{ID: "bbb", AddedAt: "2026-10-14T01:31:36Z"},
			{ID: "aaa", AddedAt: "2026-07-15T00:00:00Z"},
			{ID: "ccc", AddedAt: "2026-09-01T00:00:00Z"},
		},
		// Same added_at: the id is the deterministic tie-break.
		"tie": {
			{ID: "bbb", AddedAt: "2026-07-15T00:00:00Z"},
			{ID: "aaa", AddedAt: "2026-07-15T00:00:00Z"},
		},
		// Unparseable added_at counts as newest; the dated entry wins.
		"undated": {
			{ID: "aaa", AddedAt: "not-a-timestamp"},
			{ID: "bbb", AddedAt: "2026-07-15T00:00:00Z"},
		},
	})
	want := map[string]string{
		"single":  "single#aaa",
		"multi":   "multi#aaa",
		"tie":     "tie#aaa",
		"undated": "undated#bbb",
	}
	for parent, target := range want {
		if renames[parent] != target {
			t.Errorf("renames[%s] = %q, want %q", parent, renames[parent], target)
		}
	}
	if got := providerKeyRenames(nil); got != nil {
		t.Errorf("providerKeyRenames(nil) = %v, want nil", got)
	}
	if got := providerKeyRenames(map[string][]providerbuild.PoolAccount{"empty": {}}); len(got) != 0 {
		t.Errorf("providerKeyRenames with empty pool = %v, want no renames", got)
	}
}

func TestRenameQuotaStateProviderKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "quota_state.json")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	renames := map[string]string{"p": "p#acc1", "gone": "gone#acc9"}

	t.Run("renames bare providers section, preserves other sections", func(t *testing.T) {
		write(`{
  "providers": {"p": {"billing": 1, "remaining_pct": 0.5}, "kept": {"billing": 2}},
  "sticky": {"m1": {"provider": "p", "since": "2026-10-01T00:00:00Z"}},
  "health_fp": "abc",
  "wire_caps": {}
}`)
		if err := renameQuotaStateProviderKeys(path, renames); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var wrap map[string]json.RawMessage
		if err := json.Unmarshal(data, &wrap); err != nil {
			t.Fatal(err)
		}
		var providers map[string]json.RawMessage
		if err := json.Unmarshal(wrap["providers"], &providers); err != nil {
			t.Fatal(err)
		}
		if _, bare := providers["p"]; bare {
			t.Error("bare p entry still present after rename")
		}
		var moved struct {
			Billing      int     `json:"billing"`
			RemainingPct float64 `json:"remaining_pct"`
		}
		if err := json.Unmarshal(providers["p#acc1"], &moved); err != nil {
			t.Fatalf("renamed entry unreadable: %v", err)
		}
		if moved.Billing != 1 || moved.RemainingPct != 0.5 {
			t.Errorf("renamed entry = %+v, want billing=1 remaining=0.5", moved)
		}
		if _, kept := providers["kept"]; !kept {
			t.Error("unrelated provider entry dropped by the rename")
		}
		// Other sections round-trip untouched.
		for _, section := range []string{"sticky", "health_fp", "wire_caps"} {
			if len(wrap[section]) == 0 {
				t.Errorf("section %q lost by the rewrite", section)
			}
		}
	})

	t.Run("target wins when both entries exist", func(t *testing.T) {
		write(`{"providers": {"p": {"remaining_pct": 0.1}, "p#acc1": {"remaining_pct": 0.9}}}`)
		if err := renameQuotaStateProviderKeys(path, renames); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(path)
		var wrap struct {
			Providers map[string]struct {
				RemainingPct float64 `json:"remaining_pct"`
			} `json:"providers"`
		}
		if err := json.Unmarshal(data, &wrap); err != nil {
			t.Fatal(err)
		}
		if _, bare := wrap.Providers["p"]; bare {
			t.Error("stale bare entry kept alongside the fresher virtual one")
		}
		if got := wrap.Providers["p#acc1"].RemainingPct; got != 0.9 {
			t.Errorf("target entry = %v, want the existing 0.9 observation", got)
		}
	})

	t.Run("missing file, no providers section, or no hits leave the file alone", func(t *testing.T) {
		if err := renameQuotaStateProviderKeys(filepath.Join(dir, "absent.json"), renames); err != nil {
			t.Fatalf("missing file: %v", err)
		}
		write(`{"sticky": {}}`)
		before, _ := os.ReadFile(path)
		if err := renameQuotaStateProviderKeys(path, renames); err != nil {
			t.Fatal(err)
		}
		after, _ := os.ReadFile(path)
		if string(before) != string(after) {
			t.Error("no-hit rename rewrote the file")
		}
	})
}

// TestStartupProviderKeyMigration_MovesBareHistoryOntoVirtualKey is the
// end-to-end regression for the motivating bug: a provider whose stats and
// quota history live under the bare name (written before the pool keyed by
// virtual ids) must come back under "name#<accountID>" after startup — both in
// the persisted minute buckets and in the seeded cumulative counters.
func TestStartupProviderKeyMigration_MovesBareHistoryOntoVirtualKey(t *testing.T) {
	home := t.TempDir()
	setPoolHome(t, home)
	writePoolFile(t, "shopee", "static", "KEY-OLD", "KEY-NEW")
	// The old account (earliest added_at) owns the bare-name era. writePoolFile
	// stamps every account with the same fixed added_at, so rewrite the pool
	// with distinct timestamps to pin the attribution.
	poolPath := accounts.NewStore(home).PoolPath("shopee")
	poolData, err := os.ReadFile(poolPath)
	if err != nil {
		t.Fatal(err)
	}
	var pool accounts.Pool
	if err := json.Unmarshal(poolData, &pool); err != nil {
		t.Fatal(err)
	}
	oldID := accounts.AccountID("static", accounts.Credentials{APIKey: "KEY-OLD"})
	newID := accounts.AccountID("static", accounts.Credentials{APIKey: "KEY-NEW"})
	for i := range pool.Accounts {
		switch pool.Accounts[i].ID {
		case oldID:
			pool.Accounts[i].AddedAt = "2026-07-15T00:00:00Z"
		case newID:
			pool.Accounts[i].AddedAt = "2026-10-14T01:31:36Z"
		}
	}
	if err := accounts.NewStore(home).Save("shopee", "static", pool); err != nil {
		t.Fatal(err)
	}

	// Pre-migration state: stats + quota history under the BARE name, plus a
	// little new-era traffic already recorded under the old account's virtual
	// key (the moment between adding the second account and the restart).
	dbPath := filepath.Join(home, ".model-proxy", "stats.db")
	store, err := observestats.Open(observestats.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	bareKey := observestats.Key{Provider: "shopee", Model: "glm-5"}
	vidKey := observestats.Key{Provider: "shopee#" + oldID, Model: "glm-5"}
	if err := store.Flush(1791900000, map[observestats.Key]observestats.Counters{
		bareKey: {Requests: 82242, Input: 100, Output: 50, TokenRequests: 82242, LastRequestAt: 1791900060},
		vidKey:  {Requests: 3, Input: 7, Output: 4, TokenRequests: 3, LastRequestAt: 1791900070},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	quotaPath := filepath.Join(home, ".model-proxy", "quota_state.json")
	if err := os.WriteFile(quotaPath, []byte(`{"providers":{"shopee":{"billing":1,"remaining_pct":0.42,"as_of":"2026-10-13T00:00:00Z"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Startup: build → migrate → quota load → runtime services (stats
	// bootstrap). Same wiring as production (NewProxy → StartRuntimeServices).
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]configdomain.Provider{
			"shopee": {OpenAIBaseURL: "http://127.0.0.1:1", Provider: "static", Models: []string{"glm-5"}},
		},
		Stats: configdomain.StatsConfig{DBPath: dbPath},
	}
	p := newTestProxyAt(t, cfg, quotaPath)
	p.StartRuntimeServices(cfg)

	poolKey := "shopee#" + oldID
	// Persisted buckets: bare rows merged into the old account's virtual key
	// (additively with the new-era rows).
	rows, err := p.stats.QueryRange(0, time.Now().Add(time.Hour).Unix(), poolKey, "glm-5", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Provider != poolKey || rows[0].Requests != 82245 {
		t.Fatalf("migrated bucket rows = %+v, want one %s row with 82245 requests", rows, poolKey)
	}
	if bare, err := p.stats.QueryRange(0, time.Now().Add(time.Hour).Unix(), "shopee", "glm-5", 0); err != nil || len(bare) != 0 {
		t.Fatalf("bare-name rows survived the migration: %+v (err=%v)", bare, err)
	}
	// Seeded cumulative hot counters land on the virtual key.
	if snap := p.tokens.Snapshot()[counters.PMKey{Provider: poolKey, Model: "glm-5"}]; snap.Requests != 82245 {
		t.Fatalf("seeded cumulative counter = %+v, want 82245 requests under %s", snap, poolKey)
	}
	// Quota: the bare last-known-good snapshot survives the load under the
	// virtual key.
	quota := p.runtimeState.Quota(poolKey)
	if quota == nil || quota.RemainingPct != 0.42 {
		t.Fatalf("migrated quota snapshot = %+v, want remaining 0.42 under %s", quota, poolKey)
	}
	if leaked := p.runtimeState.Quota("shopee"); leaked != nil {
		t.Fatalf("bare quota key survived the migration: %+v", leaked)
	}
	// The new account's virtual key got no misplaced history.
	if leaked := p.runtimeState.Quota("shopee#" + newID); leaked != nil {
		t.Fatalf("quota attributed to the wrong account: %+v", leaked)
	}
}

// TestStartupProviderKeyMigration_NoPoolNoRename pins the safety boundary: a
// provider WITHOUT a plural pool (legacy singular / not logged in) keeps its
// bare-name history — the migration must never touch non-pooled keys.
func TestStartupProviderKeyMigration_NoPoolNoRename(t *testing.T) {
	home := t.TempDir()
	setPoolHome(t, home)
	if err := os.WriteFile(legacyPoolPath("zhipu"), []byte(`{"api_key":"SOLO"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(home, ".model-proxy", "stats.db")
	store, err := observestats.Open(observestats.Options{Path: dbPath})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Flush(1791900000, map[observestats.Key]observestats.Counters{
		{Provider: "zhipu", Model: "glm-5"}: {Requests: 5, TokenRequests: 5, LastRequestAt: 1791900060},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	cfg := &configdomain.Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]configdomain.Provider{
			"zhipu": {OpenAIBaseURL: "http://127.0.0.1:1", Provider: "zhipu", Models: []string{"glm-5"}},
		},
		Stats: configdomain.StatsConfig{DBPath: dbPath},
	}
	quotaPath := filepath.Join(home, ".model-proxy", "quota_state.json")
	p := newTestProxyAt(t, cfg, quotaPath)
	p.StartRuntimeServices(cfg)

	rows, err := p.stats.QueryRange(0, time.Now().Add(time.Hour).Unix(), "zhipu", "glm-5", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Provider != "zhipu" || rows[0].Requests != 5 {
		t.Fatalf("legacy-singular history must stay under the bare name: %+v", rows)
	}
}
