package stats

import (
	"context"
	"fmt"
)

// RenameProviderKey re-keys one provider's persisted history from one runtime
// provider key to another (bare pool-era name → virtual "name#<accountID>"),
// merging additively when the target already holds rows for the same bucket —
// the same merge semantics as the Flush upserts (counters sum, last_request_at
// keeps the max). Atomic per call: both tables move inside one transaction, so
// a crash leaves either the old or the new key fully owning the history, never
// a half-split. Idempotent: no source rows means a no-op returning 0.
//
// The stats package knows nothing about pools; the composition root derives
// the from→to pairs (see internal/app provider-key migration) and applies them
// BEFORE Bootstrap restores the cumulative baseline, so both the persisted
// buckets and the seeded in-memory counters land on the new key.
func (s *Store) RenameProviderKey(from, to string) (int64, error) {
	if s == nil || s.db == nil || from == "" || to == "" || from == to {
		return 0, nil
	}
	moved, err := s.renameProviderKeyContext(context.Background(), from, to)
	if err != nil {
		return 0, fmt.Errorf("rename provider key %s→%s: %w", from, to, err)
	}
	return moved, nil
}

func (s *Store) renameProviderKeyContext(ctx context.Context, from, to string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	// INSERT..SELECT upsert needs the SELECT's WHERE clause to keep SQLite's
	// parser from reading the trailing ON as a join keyword (upsert docs).
	if _, err := tx.ExecContext(ctx, `INSERT INTO minute_buckets
		(provider, model, minute, requests, failovers, rate_limited_429, failures,
		 input, output, cache_creation, cache_read, token_requests, last_request_at,
		 latency_ms_sum, ttft_ms_sum, duration_ms_sum)
		SELECT ?2, model, minute, requests, failovers, rate_limited_429, failures,
		 input, output, cache_creation, cache_read, token_requests, last_request_at,
		 latency_ms_sum, ttft_ms_sum, duration_ms_sum
		FROM minute_buckets WHERE provider = ?1
		ON CONFLICT(provider, model, minute) DO UPDATE SET
			requests = requests + excluded.requests,
			failovers = failovers + excluded.failovers,
			rate_limited_429 = rate_limited_429 + excluded.rate_limited_429,
			failures = failures + excluded.failures,
			input = input + excluded.input,
			output = output + excluded.output,
			cache_creation = cache_creation + excluded.cache_creation,
			cache_read = cache_read + excluded.cache_read,
			token_requests = token_requests + excluded.token_requests,
			last_request_at = MAX(last_request_at, excluded.last_request_at),
			latency_ms_sum = latency_ms_sum + excluded.latency_ms_sum,
			ttft_ms_sum = ttft_ms_sum + excluded.ttft_ms_sum,
			duration_ms_sum = duration_ms_sum + excluded.duration_ms_sum`,
		from, to); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM minute_buckets WHERE provider = ?1`, from)
	if err != nil {
		return 0, err
	}
	moved, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO agent_buckets
		(agent, provider, model, minute, requests, input, output, cache_creation, cache_read, latency_ms_sum, ttft_ms_sum, duration_ms_sum, failures)
		SELECT agent, ?2, model, minute, requests, input, output, cache_creation, cache_read, latency_ms_sum, ttft_ms_sum, duration_ms_sum, failures
		FROM agent_buckets WHERE provider = ?1
		ON CONFLICT(agent, provider, model, minute) DO UPDATE SET
			requests = requests + excluded.requests,
			input = input + excluded.input,
			output = output + excluded.output,
			cache_creation = cache_creation + excluded.cache_creation,
			cache_read = cache_read + excluded.cache_read,
			latency_ms_sum = latency_ms_sum + excluded.latency_ms_sum,
			ttft_ms_sum = ttft_ms_sum + excluded.ttft_ms_sum,
			duration_ms_sum = duration_ms_sum + excluded.duration_ms_sum,
			failures = failures + excluded.failures`,
		from, to); err != nil {
		return 0, err
	}
	res, err = tx.ExecContext(ctx,
		`DELETE FROM agent_buckets WHERE provider = ?1`, from)
	if err != nil {
		return 0, err
	}
	agentMoved, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return moved + agentMoved, tx.Commit()
}

// RenameProviderKeyLive is the runtime (reload-time) variant of the startup
// rename: it moves one provider's durable rows AND, under the flusher's lock,
// the three in-memory projections the startup path rebuilds via Bootstrap —
// cumulative counters, diff baselines, and pending batches — so the
// /api/tokens cumulative view, minute-range queries, and subsequent flush
// diffs all agree on the new key WITHOUT a restart.
//
// The durable rename runs first under f.mu: on failure nothing in-memory has
// moved and the old key fully owns its history (the documented degradation of
// the startup pass). Counter adoption may lose a straggler increment on an
// already-detached from-entry — callers run it after the generation swap
// stopped from-key writers. Pending batches carry per-minute deltas keyed at
// diff time; rewriting their keys makes the retained backlog flush
// additively into the renamed history, exactly what the durable merge
// produces for already-persisted rows.
func (f *Flusher) RenameProviderKeyLive(store *Store, from, to string) (int64, error) {
	if f == nil || store == nil || from == "" || to == "" || from == to {
		return 0, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	moved, err := store.RenameProviderKey(from, to)
	if err != nil {
		return 0, err
	}
	if f.metrics != nil {
		f.metrics.AdoptProviderKey(from, to)
	}
	if f.tokens != nil {
		f.tokens.AdoptProviderKey(from, to)
	}
	if f.agents != nil {
		f.agents.AdoptProviderKey(from, to)
	}

	for k, v := range f.prev {
		if k.Provider != from {
			continue
		}
		delete(f.prev, k)
		if v == (Counters{}) {
			continue
		}
		dstKey := Key{Provider: to, Model: k.Model}
		dst := f.prev[dstKey]
		dst.Requests += v.Requests
		dst.Failovers += v.Failovers
		dst.RateLimited429 += v.RateLimited429
		dst.Failures += v.Failures
		dst.Input += v.Input
		dst.Output += v.Output
		dst.CacheCreation += v.CacheCreation
		dst.CacheRead += v.CacheRead
		dst.TokenRequests += v.TokenRequests
		if v.LastRequestAt > dst.LastRequestAt {
			dst.LastRequestAt = v.LastRequestAt
		}
		dst.LatencySum += v.LatencySum
		dst.TTFTSum += v.TTFTSum
		dst.DurationSum += v.DurationSum
		f.prev[dstKey] = dst
	}
	for k, v := range f.agentPrev {
		if k.Provider != from {
			continue
		}
		delete(f.agentPrev, k)
		if v == (AgentCounters{}) {
			continue
		}
		dstKey := AgentKey{Agent: k.Agent, Provider: to, Model: k.Model}
		dst := f.agentPrev[dstKey]
		dst.Requests += v.Requests
		dst.Input += v.Input
		dst.Output += v.Output
		dst.CacheCreation += v.CacheCreation
		dst.CacheRead += v.CacheRead
		dst.LatencySum += v.LatencySum
		dst.TTFTSum += v.TTFTSum
		dst.DurationSum += v.DurationSum
		dst.Failures += v.Failures
		f.agentPrev[dstKey] = dst
	}

	// Retained backlog: rewrite per-minute deltas so a later (possibly
	// out-of-order) drain lands under the new key instead of resurrecting
	// bare-name rows after the durable rename.
	for i := range f.pending {
		deltas := f.pending[i].deltas
		for k, v := range deltas {
			if k.Provider != from {
				continue
			}
			delete(deltas, k)
			MergeDeltas(deltas, map[Key]Counters{{Provider: to, Model: k.Model}: v})
		}
	}
	for i := range f.agentQueue {
		deltas := f.agentQueue[i].deltas
		for k, v := range deltas {
			if k.Provider != from {
				continue
			}
			delete(deltas, k)
			MergeAgentDeltas(deltas, map[AgentKey]AgentCounters{{Agent: k.Agent, Provider: to, Model: k.Model}: v})
		}
	}
	return moved, nil
}
