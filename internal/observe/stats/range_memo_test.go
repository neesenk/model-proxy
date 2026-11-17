package stats

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRangeQueriesServeRepeatsFromMemoUntilMutation pins the Status-tab
// economics: the Web token-usage selector re-requests the same cumulative
// range every 5s, and the tables only change through this Store's mutation
// methods (the daemon is the single writer), so a repeat of the same range is
// answered from the memo. An out-of-band edit that bypasses every Store method
// therefore stays invisible to the repeated range — while a different range is
// a different memo key that reads the table, and the next Store mutation
// retires the memo.
func TestRangeQueriesServeRepeatsFromMemoUntilMutation(t *testing.T) {
	store := newTestStore(t, 0)
	key := Key{Provider: "p", Model: "m"}
	agentKey := AgentKey{Agent: "a", Provider: "p", Model: "m"}
	if err := store.Flush(120, map[Key]Counters{key: {Requests: 2}}); err != nil {
		t.Fatal(err)
	}
	if err := store.FlushAgents(120, map[AgentKey]AgentCounters{agentKey: {Requests: 2}}); err != nil {
		t.Fatal(err)
	}
	providerRequests := func(from, to int64) uint64 {
		t.Helper()
		got, err := store.LoadCumulativeRange(from, to)
		if err != nil {
			t.Fatal(err)
		}
		return got[key].Requests
	}
	agentRequests := func(from, to int64) uint64 {
		t.Helper()
		got, err := store.LoadCumulativeAgentsRange(from, to)
		if err != nil {
			t.Fatal(err)
		}
		return got[agentKey].Requests
	}

	if got := providerRequests(60, 600); got != 2 {
		t.Fatalf("primed provider range = %d, want 2", got)
	}
	if got := agentRequests(60, 600); got != 2 {
		t.Fatalf("primed agent range = %d, want 2", got)
	}
	if _, err := store.db.Exec(`UPDATE minute_buckets SET requests = 40`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE agent_buckets SET requests = 40`); err != nil {
		t.Fatal(err)
	}
	if got := providerRequests(60, 600); got != 2 {
		t.Errorf("repeated provider range = %d, want the memoized 2", got)
	}
	if got := agentRequests(60, 600); got != 2 {
		t.Errorf("repeated agent range = %d, want the memoized 2", got)
	}
	if got := providerRequests(60, 660); got != 40 {
		t.Errorf("different provider range = %d, want 40 read from the table", got)
	}
	if got := agentRequests(60, 660); got != 40 {
		t.Errorf("different agent range = %d, want 40 read from the table", got)
	}

	if err := store.Flush(180, map[Key]Counters{key: {Requests: 1}}); err != nil {
		t.Fatal(err)
	}
	if got := providerRequests(60, 600); got != 41 {
		t.Errorf("provider range after Flush = %d, want 41 (memo retired by the mutation)", got)
	}
}

// TestRangeMemoRetiredByEveryStoreMutation primes both range memos and then
// runs each Store mutation method: every one must make its effect visible to
// the very same range keys. A mutation path that forgot to retire the memo
// would keep serving the primed pre-mutation aggregate.
func TestRangeMemoRetiredByEveryStoreMutation(t *testing.T) {
	key := Key{Provider: "p", Model: "m"}
	agentKey := AgentKey{Agent: "a", Provider: "p", Model: "m"}
	seed := func(t *testing.T, store *Store, minute int64) {
		t.Helper()
		if err := store.Flush(minute, map[Key]Counters{key: {Requests: 2, Input: 10}}); err != nil {
			t.Fatal(err)
		}
		if err := store.FlushAgents(minute, map[AgentKey]AgentCounters{agentKey: {Requests: 2, Input: 10}}); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now()
	recent := now.Add(-10*time.Minute).Unix() / 60 * 60
	old := now.Add(-2*time.Hour).Unix() / 60 * 60

	type snapshot struct {
		providers map[Key]Counters
		agents    map[AgentKey]AgentCounters
	}
	cases := []struct {
		name   string
		open   func(t *testing.T) *Store
		mutate func(t *testing.T, store *Store)
		check  func(t *testing.T, got snapshot)
	}{
		{
			name: "Flush",
			open: func(t *testing.T) *Store {
				store := newTestStore(t, 0)
				seed(t, store, recent)
				return store
			},
			mutate: func(t *testing.T, store *Store) {
				if err := store.Flush(recent, map[Key]Counters{key: {Requests: 3}}); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, got snapshot) {
				if got.providers[key].Requests != 5 {
					t.Errorf("providers after Flush = %+v, want 5 requests", got.providers)
				}
			},
		},
		{
			name: "FlushAgents",
			open: func(t *testing.T) *Store {
				store := newTestStore(t, 0)
				seed(t, store, recent)
				return store
			},
			mutate: func(t *testing.T, store *Store) {
				if err := store.FlushAgents(recent, map[AgentKey]AgentCounters{agentKey: {Requests: 3}}); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, got snapshot) {
				if got.agents[agentKey].Requests != 5 {
					t.Errorf("agents after FlushAgents = %+v, want 5 requests", got.agents)
				}
			},
		},
		{
			name: "Prune",
			open: func(t *testing.T) *Store {
				store := newTestStore(t, time.Hour)
				seed(t, store, old)
				seed(t, store, recent)
				return store
			},
			mutate: func(t *testing.T, store *Store) {
				if err := store.Prune(now); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, got snapshot) {
				if got.providers[key].Requests != 2 || got.agents[agentKey].Requests != 2 {
					t.Errorf("after Prune = %+v / %+v, want only the recent bucket (2 requests)", got.providers, got.agents)
				}
			},
		},
		{
			name: "Reset",
			open: func(t *testing.T) *Store {
				store := newTestStore(t, 0)
				seed(t, store, recent)
				return store
			},
			mutate: func(t *testing.T, store *Store) {
				if err := store.Reset(); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, got snapshot) {
				if len(got.providers) != 0 || len(got.agents) != 0 {
					t.Errorf("after Reset = %+v / %+v, want empty", got.providers, got.agents)
				}
			},
		},
		{
			name: "RenameProviderKey",
			open: func(t *testing.T) *Store {
				store := newTestStore(t, 0)
				seed(t, store, recent)
				return store
			},
			mutate: func(t *testing.T, store *Store) {
				if _, err := store.RenameProviderKey("p", "p#acct"); err != nil {
					t.Fatal(err)
				}
			},
			check: func(t *testing.T, got snapshot) {
				renamed := Key{Provider: "p#acct", Model: "m"}
				renamedAgent := AgentKey{Agent: "a", Provider: "p#acct", Model: "m"}
				if _, stale := got.providers[key]; stale || got.providers[renamed].Requests != 2 {
					t.Errorf("providers after rename = %+v, want history under %v", got.providers, renamed)
				}
				if _, stale := got.agents[agentKey]; stale || got.agents[renamedAgent].Requests != 2 {
					t.Errorf("agents after rename = %+v, want history under %v", got.agents, renamedAgent)
				}
			},
		},
		{
			name: "ImportLegacyTokens",
			open: func(t *testing.T) *Store { return newTestStore(t, 0) },
			mutate: func(t *testing.T, store *Store) {
				path := filepath.Join(t.TempDir(), "token_usage.json")
				data, err := json.Marshal(map[string]legacyTokenUsage{"p\x00m": {Input: 42, Requests: 4}})
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
				if imported, err := store.ImportLegacyTokens(path); err != nil || imported != 1 {
					t.Fatalf("ImportLegacyTokens = (%d, %v), want (1, nil)", imported, err)
				}
			},
			check: func(t *testing.T, got snapshot) {
				if got.providers[key].Input != 42 {
					t.Errorf("providers after legacy import = %+v, want input 42", got.providers)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := tc.open(t)
			// Prime both memos on the exact keys checked after the mutation.
			if _, err := store.LoadCumulativeRange(0, 0); err != nil {
				t.Fatal(err)
			}
			if _, err := store.LoadCumulativeAgentsRange(0, 0); err != nil {
				t.Fatal(err)
			}
			tc.mutate(t, store)
			providers, err := store.LoadCumulativeRange(0, 0)
			if err != nil {
				t.Fatal(err)
			}
			agents, err := store.LoadCumulativeAgentsRange(0, 0)
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, snapshot{providers: providers, agents: agents})
		})
	}

	t.Run("RebuildTokenLedger", func(t *testing.T) {
		store, idxPath := seedLedgerFixture(t)
		ledgerKey := Key{Provider: "deepseek", Model: "m"}
		primed, err := store.LoadCumulativeRange(0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if primed[ledgerKey].Input != 500 {
			t.Fatalf("primed ledger input = %d, want the committed 500", primed[ledgerKey].Input)
		}
		if _, _, err := store.RebuildTokenLedger(idxPath); err != nil {
			t.Fatal(err)
		}
		rebuilt, err := store.LoadCumulativeRange(0, 0)
		if err != nil {
			t.Fatal(err)
		}
		if rebuilt[ledgerKey].Input != 300 {
			t.Errorf("ledger input after rebuild = %d, want the rewritten 300", rebuilt[ledgerKey].Input)
		}
	})
}

// TestRangeQueriesReturnDetachedMaps: callers own the returned maps (the
// flusher's diff baseline is LoadCumulative's result and is updated in
// place), so editing one result must never leak into the next answer for the
// same range.
func TestRangeQueriesReturnDetachedMaps(t *testing.T) {
	store := newTestStore(t, 0)
	key := Key{Provider: "p", Model: "m"}
	agentKey := AgentKey{Agent: "a", Provider: "p", Model: "m"}
	if err := store.Flush(120, map[Key]Counters{key: {Requests: 2}}); err != nil {
		t.Fatal(err)
	}
	if err := store.FlushAgents(120, map[AgentKey]AgentCounters{agentKey: {Requests: 2}}); err != nil {
		t.Fatal(err)
	}

	providers, err := store.LoadCumulativeRange(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	providers[key] = Counters{Requests: 99}
	providers[Key{Provider: "intruder"}] = Counters{Requests: 1}
	again, err := store.LoadCumulativeRange(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 || again[key].Requests != 2 {
		t.Errorf("second provider answer = %+v, want only %v with 2 requests", again, key)
	}

	agents, err := store.LoadCumulativeAgentsRange(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	delete(agents, agentKey)
	agentsAgain, err := store.LoadCumulativeAgentsRange(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if agentsAgain[agentKey].Requests != 2 {
		t.Errorf("second agent answer = %+v, want %v with 2 requests", agentsAgain, agentKey)
	}
}
