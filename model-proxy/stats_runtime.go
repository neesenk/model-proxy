package main

import (
	"context"
	"log"
	"path/filepath"
	"sync"
	"time"

	observestats "model-proxy/internal/observe/stats"
)

const (
	// A Store outage may retain at most six hours of exact minute batches per
	// pipeline. Beyond that, the two oldest batches are folded together: totals
	// remain lossless while only the oldest minute resolution is degraded.
	maxPendingStatsBatches = 360
	// Bound SQLite transactions and flusher.mu hold time during recovery.
	maxStatsBatchesPerFlush    = 30
	statsShutdownFlushTimeout  = 2 * time.Second
	statsShutdownRetryInterval = 25 * time.Millisecond
)

// statsSink is the durable half of the runtime stats projection. Keeping this
// narrow port at the composition boundary makes retry/reset ordering testable
// without exposing SQLite internals to the root package.
type statsSink interface {
	FlushContext(context.Context, int64, map[observestats.Key]observestats.Counters) error
	FlushAgentsContext(context.Context, int64, map[observestats.AgentKey]observestats.AgentCounters) error
	PruneContext(context.Context, time.Time) error
	Reset() error
}

// statsFlusher projects detached cumulative snapshots from the three hot-path
// counter owners into minute deltas. It is application orchestration, not part
// of the SQLite Store: reset must coordinate runtime counters and the durable
// baseline under the same lock.
type statsFlusher struct {
	stats   statsSink
	metrics *metricsStore
	tokens  *tokenCounter
	agents  *agentCounter

	mu         sync.Mutex
	prev       map[observestats.Key]observestats.Counters
	agentPrev  map[observestats.AgentKey]observestats.AgentCounters
	pending    []statsBatch
	agentQueue []agentStatsBatch
	lastBucket int64

	flushFailures      uint64
	agentFlushFailures uint64
	pruneFailures      uint64
	coalesced          uint64
	agentCoalesced     uint64
}

type statsBatch struct {
	minute int64
	deltas map[observestats.Key]observestats.Counters
}

type agentStatsBatch struct {
	minute int64
	deltas map[observestats.AgentKey]observestats.AgentCounters
}

func newStatsFlusher(
	stats statsSink,
	metrics *metricsStore,
	tokens *tokenCounter,
	agents *agentCounter,
	baseline map[observestats.Key]observestats.Counters,
) *statsFlusher {
	return &statsFlusher{
		stats: stats, metrics: metrics, tokens: tokens, agents: agents, prev: baseline,
	}
}

func (f *statsFlusher) collect() map[observestats.Key]observestats.Counters {
	snapshot := map[observestats.Key]observestats.Counters{}
	if f.metrics != nil {
		for key, metrics := range f.metrics.snapshot() {
			statsKey := observestats.Key{Provider: key.Provider, Model: key.Model}
			counters := snapshot[statsKey]
			counters.Requests = metrics.Requests
			counters.Failovers = metrics.Failovers
			counters.RateLimited429 = metrics.RateLimited429
			counters.Failures = metrics.Failures
			counters.LastRequestAt = metrics.LastRequestAt
			counters.LatencySum = metrics.LatencySum
			counters.TTFTSum = metrics.TTFTSum
			snapshot[statsKey] = counters
		}
	}
	if f.tokens != nil {
		for key, usage := range f.tokens.snapshot() {
			statsKey := observestats.Key{Provider: key.Provider, Model: key.Model}
			counters := snapshot[statsKey]
			counters.Input = usage.Input
			counters.Output = usage.Output
			counters.CacheCreation = usage.CacheCreation
			counters.CacheRead = usage.CacheRead
			counters.TokenRequests = usage.Requests
			snapshot[statsKey] = counters
		}
	}
	return snapshot
}

func (f *statsFlusher) collectAgents() map[observestats.AgentKey]observestats.AgentCounters {
	if f.agents == nil {
		return nil
	}
	source := f.agents.snapshot()
	snapshot := make(map[observestats.AgentKey]observestats.AgentCounters, len(source))
	for key, counters := range source {
		snapshot[observestats.AgentKey{
			Agent: key.Agent, Provider: key.Provider, Model: key.Model,
		}] = observestats.AgentCounters{
			Requests: counters.Requests, Input: counters.Input, Output: counters.Output,
			LatencySum: counters.LatencySum, Failures: counters.Failures,
		}
	}
	return snapshot
}

// diffCounters returns cur-prev, clamped at zero. LastRequestAt is carried as a
// cumulative maximum because Store merges it with MAX rather than addition.
func diffCounters(
	current, previous map[observestats.Key]observestats.Counters,
) map[observestats.Key]observestats.Counters {
	deltas := map[observestats.Key]observestats.Counters{}
	for key, currentCounters := range current {
		previousCounters := previous[key]
		delta := observestats.Counters{
			Requests:       subtractCounter(currentCounters.Requests, previousCounters.Requests),
			Failovers:      subtractCounter(currentCounters.Failovers, previousCounters.Failovers),
			RateLimited429: subtractCounter(currentCounters.RateLimited429, previousCounters.RateLimited429),
			Failures:       subtractCounter(currentCounters.Failures, previousCounters.Failures),
			Input:          subtractCounter(currentCounters.Input, previousCounters.Input),
			Output:         subtractCounter(currentCounters.Output, previousCounters.Output),
			CacheCreation:  subtractCounter(currentCounters.CacheCreation, previousCounters.CacheCreation),
			CacheRead:      subtractCounter(currentCounters.CacheRead, previousCounters.CacheRead),
			TokenRequests:  subtractCounter(currentCounters.TokenRequests, previousCounters.TokenRequests),
			LastRequestAt:  currentCounters.LastRequestAt,
			LatencySum:     subtractCounter(currentCounters.LatencySum, previousCounters.LatencySum),
			TTFTSum:        subtractCounter(currentCounters.TTFTSum, previousCounters.TTFTSum),
		}
		if delta.Requests == 0 && delta.Failovers == 0 &&
			delta.RateLimited429 == 0 && delta.Failures == 0 &&
			delta.Input == 0 && delta.Output == 0 &&
			delta.CacheCreation == 0 && delta.CacheRead == 0 &&
			delta.TokenRequests == 0 && delta.LatencySum == 0 &&
			delta.TTFTSum == 0 {
			continue
		}
		deltas[key] = delta
	}
	return deltas
}

func diffAgent(
	current, previous map[observestats.AgentKey]observestats.AgentCounters,
) map[observestats.AgentKey]observestats.AgentCounters {
	deltas := map[observestats.AgentKey]observestats.AgentCounters{}
	for key, currentCounters := range current {
		previousCounters := previous[key]
		delta := observestats.AgentCounters{
			Requests:   subtractCounter(currentCounters.Requests, previousCounters.Requests),
			Input:      subtractCounter(currentCounters.Input, previousCounters.Input),
			Output:     subtractCounter(currentCounters.Output, previousCounters.Output),
			LatencySum: subtractCounter(currentCounters.LatencySum, previousCounters.LatencySum),
			Failures:   subtractCounter(currentCounters.Failures, previousCounters.Failures),
		}
		if delta.Requests == 0 && delta.Input == 0 && delta.Output == 0 &&
			delta.LatencySum == 0 && delta.Failures == 0 {
			continue
		}
		deltas[key] = delta
	}
	return deltas
}

func subtractCounter(current, previous uint64) uint64 {
	if current <= previous {
		return 0
	}
	return current - previous
}

// flush performs one snapshot/diff/persist/prune cycle. New deltas are first
// assigned to their completed-minute batch, then each pipeline drains its
// oldest pending batches in order. A transient Store failure therefore retains
// both the counters and their original minute instead of shifting accumulated
// data into a later bucket. It reports whether any batch was durably written.
func (f *statsFlusher) flush(now time.Time) bool {
	return f.flushContext(context.Background(), now)
}

func (f *statsFlusher) flushContext(ctx context.Context, now time.Time) bool {
	if !f.lockContext(ctx) {
		return false
	}
	defer f.mu.Unlock()

	current := f.collect()
	deltas := diffCounters(current, f.prev)
	agentCurrent := f.collectAgents()
	agentDeltas := diffAgent(agentCurrent, f.agentPrev)

	if len(deltas) > 0 || len(agentDeltas) > 0 {
		minute := now.Unix()/60*60 - 60
		if minute <= f.lastBucket {
			minute = f.lastBucket + 60
		}
		f.lastBucket = minute
		if len(deltas) > 0 {
			f.enqueue(statsBatch{minute: minute, deltas: deltas})
		}
		if len(agentDeltas) > 0 {
			f.enqueueAgent(agentStatsBatch{minute: minute, deltas: agentDeltas})
		}
	}
	// The detached batches now own every observed delta, so baselines can move
	// even if SQLite is temporarily unavailable. Reset clears both queues under
	// this same mutex.
	f.prev = current
	f.agentPrev = agentCurrent

	wrote := f.flushPending(ctx, maxStatsBatchesPerFlush)
	f.prune(ctx, now)
	return wrote
}

func (f *statsFlusher) lockContext(ctx context.Context) bool {
	if ctx == nil || ctx.Done() == nil {
		f.mu.Lock()
		return true
	}
	if f.mu.TryLock() {
		return true
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
			if f.mu.TryLock() {
				return true
			}
		}
	}
}

func (f *statsFlusher) enqueue(batch statsBatch) {
	f.pending = append(f.pending, batch)
	if len(f.pending) <= maxPendingStatsBatches {
		return
	}
	mergeStatsDeltas(f.pending[1].deltas, f.pending[0].deltas)
	f.pending[1].minute = f.pending[0].minute
	f.pending = f.pending[1:]
	f.coalesced++
	if f.coalesced == 1 || f.coalesced%60 == 0 {
		log.Printf(
			"[stats] provider backlog exceeded %d batches; coalesced %d old minute batches",
			maxPendingStatsBatches,
			f.coalesced,
		)
	}
}

func (f *statsFlusher) enqueueAgent(batch agentStatsBatch) {
	f.agentQueue = append(f.agentQueue, batch)
	if len(f.agentQueue) <= maxPendingStatsBatches {
		return
	}
	mergeAgentStatsDeltas(f.agentQueue[1].deltas, f.agentQueue[0].deltas)
	f.agentQueue[1].minute = f.agentQueue[0].minute
	f.agentQueue = f.agentQueue[1:]
	f.agentCoalesced++
	if f.agentCoalesced == 1 || f.agentCoalesced%60 == 0 {
		log.Printf(
			"[stats] agent backlog exceeded %d batches; coalesced %d old minute batches",
			maxPendingStatsBatches,
			f.agentCoalesced,
		)
	}
}

func mergeStatsDeltas(
	destination, source map[observestats.Key]observestats.Counters,
) {
	for key, add := range source {
		current := destination[key]
		current.Requests += add.Requests
		current.Failovers += add.Failovers
		current.RateLimited429 += add.RateLimited429
		current.Failures += add.Failures
		current.Input += add.Input
		current.Output += add.Output
		current.CacheCreation += add.CacheCreation
		current.CacheRead += add.CacheRead
		current.TokenRequests += add.TokenRequests
		if add.LastRequestAt > current.LastRequestAt {
			current.LastRequestAt = add.LastRequestAt
		}
		current.LatencySum += add.LatencySum
		current.TTFTSum += add.TTFTSum
		destination[key] = current
	}
}

func mergeAgentStatsDeltas(
	destination, source map[observestats.AgentKey]observestats.AgentCounters,
) {
	for key, add := range source {
		current := destination[key]
		current.Requests += add.Requests
		current.Input += add.Input
		current.Output += add.Output
		current.LatencySum += add.LatencySum
		current.Failures += add.Failures
		destination[key] = current
	}
}

func (f *statsFlusher) flushPending(ctx context.Context, limit int) bool {
	wrote := false
	for attempts := 0; len(f.pending) > 0 && attempts < limit; attempts++ {
		batch := f.pending[0]
		if err := f.stats.FlushContext(ctx, batch.minute, batch.deltas); err != nil {
			f.flushFailures++
			if f.flushFailures == 1 || f.flushFailures%60 == 0 {
				log.Printf(
					"[stats] flush failed: %v (minute %d retained; attempt %d)",
					err,
					batch.minute,
					f.flushFailures,
				)
			}
			break
		}
		f.flushFailures = 0
		f.pending = f.pending[1:]
		wrote = true
	}
	for attempts := 0; len(f.agentQueue) > 0 && attempts < limit; attempts++ {
		batch := f.agentQueue[0]
		if err := f.stats.FlushAgentsContext(ctx, batch.minute, batch.deltas); err != nil {
			f.agentFlushFailures++
			if f.agentFlushFailures == 1 || f.agentFlushFailures%60 == 0 {
				log.Printf(
					"[stats] agent flush failed: %v (minute %d retained; attempt %d)",
					err,
					batch.minute,
					f.agentFlushFailures,
				)
			}
			break
		}
		f.agentFlushFailures = 0
		f.agentQueue = f.agentQueue[1:]
		wrote = true
	}
	return wrote
}

func (f *statsFlusher) pendingCounts() (provider, agent int) {
	provider, agent, _ = f.pendingCountsContext(context.Background())
	return provider, agent
}

func (f *statsFlusher) pendingCountsContext(
	ctx context.Context,
) (provider, agent int, locked bool) {
	if !f.lockContext(ctx) {
		return 0, 0, false
	}
	defer f.mu.Unlock()
	return len(f.pending), len(f.agentQueue), true
}

// flushForShutdown gives transient Store failures a best-effort retry window.
// Context-aware calls and the Store's short SQLite busy timeout bound normal
// lock contention; an underlying filesystem stall remains outside Go's hard
// cancellation guarantees. Remaining batch counts are logged before close.
func (f *statsFlusher) flushForShutdown(timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	f.flushContext(ctx, time.Now())
	for {
		providerPending, agentPending, locked := f.pendingCountsContext(ctx)
		if !locked {
			log.Printf("[stats] shutdown retry window exhausted while waiting for the flusher lock")
			return
		}
		if providerPending == 0 && agentPending == 0 {
			return
		}
		if ctx.Err() != nil {
			log.Printf(
				"[stats] shutdown retry window exhausted with %d provider and %d agent batches pending",
				providerPending,
				agentPending,
			)
			return
		}
		timer := time.NewTimer(statsShutdownRetryInterval)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			continue
		}
		f.flushContext(ctx, time.Now())
	}
}

func (f *statsFlusher) prune(ctx context.Context, now time.Time) {
	if err := f.stats.PruneContext(ctx, now); err != nil {
		f.pruneFailures++
		if f.pruneFailures == 1 || f.pruneFailures%60 == 0 {
			log.Printf("[stats] prune failed: %v (attempt %d)", err, f.pruneFailures)
		}
		return
	}
	f.pruneFailures = 0
}

// reset atomically clears durable history, the three runtime owners, and both
// diff baselines relative to a concurrent flush. Durable reset runs first: on
// failure the live counters remain intact rather than being resurrected from an
// uncleared database at the next process start.
func (f *statsFlusher) reset() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.stats.Reset(); err != nil {
		return err
	}
	if f.metrics != nil {
		f.metrics.reset()
	}
	if f.tokens != nil {
		f.tokens.reset()
	}
	if f.agents != nil {
		f.agents.reset()
	}
	f.prev = f.collect()
	f.agentPrev = f.collectAgents()
	f.pending = nil
	f.agentQueue = nil
	f.lastBucket = 0
	f.flushFailures = 0
	f.agentFlushFailures = 0
	f.pruneFailures = 0
	f.coalesced = 0
	f.agentCoalesced = 0
	return nil
}

func (p *Proxy) statsFlushLoop(stop <-chan struct{}) {
	if p.flusher == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-stop:
			cancel()
		case <-ctx.Done():
		}
	}()
	for {
		timer := time.NewTimer(untilNextMinute(time.Now()))
		select {
		case <-timer.C:
			p.flusher.flushContext(ctx, time.Now())
			if ctx.Err() != nil {
				return
			}
		case <-stop:
			timer.Stop()
			return
		}
	}
}

func untilNextMinute(now time.Time) time.Duration {
	next := now.Truncate(time.Minute).Add(time.Minute)
	duration := next.Sub(now) + 200*time.Millisecond
	if duration < 50*time.Millisecond {
		return 50 * time.Millisecond
	}
	return duration
}

func legacyTokensPath() string {
	return filepath.Join(homeDir(), ".model-proxy", "token_usage.json")
}

// initStats binds startup-only config to the long-lived Store, imports the
// legacy token file once, restores cumulative hot counters, and seeds the diff
// baseline. Stats deliberately survives config reload generations.
func (p *Proxy) initStats(config StatsConfig) {
	path := config.ResolvedDBPath()
	store, err := observestats.Open(observestats.Options{
		Path: path, Retention: config.RetentionDuration(),
	})
	if err != nil {
		log.Printf("[stats] open failed (%s): %v - running without persisted stats", path, err)
		return
	}
	p.stats = store

	if count, err := store.ImportLegacyTokens(legacyTokensPath()); err != nil {
		log.Printf("[stats] legacy token_usage.json migration failed: %v", err)
	} else if count > 0 {
		log.Printf("[stats] imported %d entries from legacy token_usage.json", count)
	}

	baseline, err := store.LoadCumulative()
	if err != nil {
		log.Printf("[stats] load baseline failed: %v", err)
		baseline = map[observestats.Key]observestats.Counters{}
	}
	for key, counters := range baseline {
		runtimeKey := pmKey{Provider: key.Provider, Model: key.Model}
		p.metrics.seed(runtimeKey, providerMetricsSnapshot{
			Requests: counters.Requests, Failovers: counters.Failovers,
			RateLimited429: counters.RateLimited429, Failures: counters.Failures,
			LastRequestAt: counters.LastRequestAt, LatencySum: counters.LatencySum,
			TTFTSum: counters.TTFTSum,
		})
		p.tokens.seed(runtimeKey, tokenUsage{
			Input: counters.Input, Output: counters.Output,
			CacheCreation: counters.CacheCreation, CacheRead: counters.CacheRead,
			Requests: counters.TokenRequests,
		})
	}
	p.flusher = newStatsFlusher(store, p.metrics, p.tokens, p.agents, baseline)
}
