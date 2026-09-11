// flusher.go owns the runtime stats projection: detached cumulative counter
// snapshots are diffed into per-minute batches and durably flushed into the
// Store, with bounded backlog coalescing and a shutdown retry window.
package stats

import (
	"context"
	"model-proxy/internal/observe/logx"
	"path/filepath"
	"sync"
	"time"

	obscounters "model-proxy/internal/observe/counters"
)

const (
	// A Store outage may retain at most six hours of exact minute batches per
	// pipeline. Beyond that, the two oldest batches are folded together: totals
	// remain lossless while only the oldest minute resolution is degraded.
	MaxPendingStatsBatches = 360
	// Bound SQLite transactions and flusher.mu hold time during recovery.
	MaxStatsBatchesPerFlush    = 30
	StatsShutdownFlushTimeout  = 2 * time.Second
	StatsShutdownRetryInterval = 25 * time.Millisecond
)

// Sink is the durable half of the runtime stats projection. Keeping this
// narrow port at the composition boundary makes retry/reset ordering testable
// without exposing SQLite internals to the root package.
type Sink interface {
	FlushContext(context.Context, int64, map[Key]Counters) error
	FlushAgentsContext(context.Context, int64, map[AgentKey]AgentCounters) error
	PruneContext(context.Context, time.Time) error
	Reset() error
}

// Flusher projects detached cumulative snapshots from the three hot-path
// counter owners into minute deltas. It is application orchestration, not part
// of the SQLite Store: reset must coordinate runtime counters and the durable
// baseline under the same lock.
type Flusher struct {
	stats   Sink
	metrics *obscounters.MetricsStore
	tokens  *obscounters.TokenCounter
	agents  *obscounters.AgentCounter

	mu         sync.Mutex
	prev       map[Key]Counters
	agentPrev  map[AgentKey]AgentCounters
	pending    []Batch
	agentQueue []AgentBatch
	lastBucket int64

	flushFailures      uint64
	agentFlushFailures uint64
	pruneFailures      uint64
	coalesced          uint64
	agentCoalesced     uint64
}

type Batch struct {
	minute int64
	deltas map[Key]Counters
}

type AgentBatch struct {
	minute int64
	deltas map[AgentKey]AgentCounters
}

func NewFlusher(
	stats Sink,
	metrics *obscounters.MetricsStore,
	tokens *obscounters.TokenCounter,
	agents *obscounters.AgentCounter,
	baseline map[Key]Counters,
	agentBaseline map[AgentKey]AgentCounters,
) *Flusher {
	return &Flusher{
		stats: stats, metrics: metrics, tokens: tokens, agents: agents, prev: baseline,
		agentPrev: agentBaseline,
	}
}

func (f *Flusher) collect() map[Key]Counters {
	snapshot := map[Key]Counters{}
	if f.metrics != nil {
		for key, metrics := range f.metrics.Snapshot() {
			statsKey := Key{Provider: key.Provider, Model: key.Model}
			counters := snapshot[statsKey]
			counters.Requests = metrics.Requests
			counters.Failovers = metrics.Failovers
			counters.RateLimited429 = metrics.RateLimited429
			counters.Failures = metrics.Failures
			counters.LastRequestAt = metrics.LastRequestAt
			counters.LatencySum = metrics.LatencySum
			counters.TTFTSum = metrics.TTFTSum
			counters.DurationSum = metrics.DurationSum
			snapshot[statsKey] = counters
		}
	}
	if f.tokens != nil {
		for key, usage := range f.tokens.Snapshot() {
			statsKey := Key{Provider: key.Provider, Model: key.Model}
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

func (f *Flusher) collectAgents() map[AgentKey]AgentCounters {
	if f.agents == nil {
		return nil
	}
	source := f.agents.Snapshot()
	snapshot := make(map[AgentKey]AgentCounters, len(source))
	for key, counters := range source {
		snapshot[AgentKey{
			Agent: key.Agent, Provider: key.Provider, Model: key.Model,
		}] = AgentCounters{
			Requests: counters.Requests, Input: counters.Input, Output: counters.Output,
			CacheCreation: counters.CacheCreation, CacheRead: counters.CacheRead,
			LatencySum: counters.LatencySum, TTFTSum: counters.TTFTSum,
			DurationSum: counters.DurationSum, Failures: counters.Failures,
		}
	}
	return snapshot
}

// DiffCounters returns cur-prev, clamped at zero. LastRequestAt is carried as a
// cumulative maximum because Store merges it with MAX rather than addition.
func DiffCounters(
	current, previous map[Key]Counters,
) map[Key]Counters {
	deltas := map[Key]Counters{}
	for key, currentCounters := range current {
		previousCounters := previous[key]
		delta := Counters{
			Requests:       SubtractCounter(currentCounters.Requests, previousCounters.Requests),
			Failovers:      SubtractCounter(currentCounters.Failovers, previousCounters.Failovers),
			RateLimited429: SubtractCounter(currentCounters.RateLimited429, previousCounters.RateLimited429),
			Failures:       SubtractCounter(currentCounters.Failures, previousCounters.Failures),
			Input:          SubtractCounter(currentCounters.Input, previousCounters.Input),
			Output:         SubtractCounter(currentCounters.Output, previousCounters.Output),
			CacheCreation:  SubtractCounter(currentCounters.CacheCreation, previousCounters.CacheCreation),
			CacheRead:      SubtractCounter(currentCounters.CacheRead, previousCounters.CacheRead),
			TokenRequests:  SubtractCounter(currentCounters.TokenRequests, previousCounters.TokenRequests),
			LastRequestAt:  currentCounters.LastRequestAt,
			LatencySum:     SubtractCounter(currentCounters.LatencySum, previousCounters.LatencySum),
			TTFTSum:        SubtractCounter(currentCounters.TTFTSum, previousCounters.TTFTSum),
			DurationSum:    SubtractCounter(currentCounters.DurationSum, previousCounters.DurationSum),
		}
		if delta.Requests == 0 && delta.Failovers == 0 &&
			delta.RateLimited429 == 0 && delta.Failures == 0 &&
			delta.Input == 0 && delta.Output == 0 &&
			delta.CacheCreation == 0 && delta.CacheRead == 0 &&
			delta.TokenRequests == 0 && delta.LatencySum == 0 &&
			delta.TTFTSum == 0 && delta.DurationSum == 0 {
			continue
		}
		deltas[key] = delta
	}
	return deltas
}

func DiffAgent(
	current, previous map[AgentKey]AgentCounters,
) map[AgentKey]AgentCounters {
	deltas := map[AgentKey]AgentCounters{}
	for key, currentCounters := range current {
		previousCounters := previous[key]
		delta := AgentCounters{
			Requests:      SubtractCounter(currentCounters.Requests, previousCounters.Requests),
			Input:         SubtractCounter(currentCounters.Input, previousCounters.Input),
			Output:        SubtractCounter(currentCounters.Output, previousCounters.Output),
			CacheCreation: SubtractCounter(currentCounters.CacheCreation, previousCounters.CacheCreation),
			CacheRead:     SubtractCounter(currentCounters.CacheRead, previousCounters.CacheRead),
			LatencySum:    SubtractCounter(currentCounters.LatencySum, previousCounters.LatencySum),
			TTFTSum:       SubtractCounter(currentCounters.TTFTSum, previousCounters.TTFTSum),
			DurationSum:   SubtractCounter(currentCounters.DurationSum, previousCounters.DurationSum),
			Failures:      SubtractCounter(currentCounters.Failures, previousCounters.Failures),
		}
		if delta.Requests == 0 && delta.Input == 0 && delta.Output == 0 &&
			delta.CacheCreation == 0 && delta.CacheRead == 0 &&
			delta.LatencySum == 0 && delta.TTFTSum == 0 &&
			delta.DurationSum == 0 && delta.Failures == 0 {
			continue
		}
		deltas[key] = delta
	}
	return deltas
}

func SubtractCounter(current, previous uint64) uint64 {
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
// Flush runs one snapshot/diff/persist/prune cycle with a background context.
func (f *Flusher) Flush(now time.Time) bool {
	return f.FlushContextCycle(context.Background(), now)
}

func (f *Flusher) FlushContextCycle(ctx context.Context, now time.Time) bool {
	if !f.lockContext(ctx) {
		return false
	}
	defer f.mu.Unlock()

	current := f.collect()
	deltas := DiffCounters(current, f.prev)
	agentCurrent := f.collectAgents()
	agentDeltas := DiffAgent(agentCurrent, f.agentPrev)

	if len(deltas) > 0 || len(agentDeltas) > 0 {
		minute := now.Unix()/60*60 - 60
		if minute <= f.lastBucket {
			minute = f.lastBucket + 60
		}
		f.lastBucket = minute
		if len(deltas) > 0 {
			f.enqueue(Batch{minute: minute, deltas: deltas})
		}
		if len(agentDeltas) > 0 {
			f.enqueueAgent(AgentBatch{minute: minute, deltas: agentDeltas})
		}
	}
	// The detached batches now own every observed delta, so baselines can move
	// even if SQLite is temporarily unavailable. Reset clears both queues under
	// this same mutex.
	f.prev = current
	f.agentPrev = agentCurrent

	wrote := f.flushPending(ctx, MaxStatsBatchesPerFlush)
	f.prune(ctx, now)
	return wrote
}

func (f *Flusher) lockContext(ctx context.Context) bool {
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

func (f *Flusher) enqueue(batch Batch) {
	f.pending = append(f.pending, batch)
	if len(f.pending) <= MaxPendingStatsBatches {
		return
	}
	MergeDeltas(f.pending[1].deltas, f.pending[0].deltas)
	f.pending[1].minute = f.pending[0].minute
	f.pending = f.pending[1:]
	f.coalesced++
	if f.coalesced == 1 || f.coalesced%60 == 0 {
		logx.Warnf(
			"[stats] provider backlog exceeded %d batches; coalesced %d old minute batches",
			MaxPendingStatsBatches,
			f.coalesced,
		)
	}
}

func (f *Flusher) enqueueAgent(batch AgentBatch) {
	f.agentQueue = append(f.agentQueue, batch)
	if len(f.agentQueue) <= MaxPendingStatsBatches {
		return
	}
	MergeAgentDeltas(f.agentQueue[1].deltas, f.agentQueue[0].deltas)
	f.agentQueue[1].minute = f.agentQueue[0].minute
	f.agentQueue = f.agentQueue[1:]
	f.agentCoalesced++
	if f.agentCoalesced == 1 || f.agentCoalesced%60 == 0 {
		logx.Warnf(
			"[stats] agent backlog exceeded %d batches; coalesced %d old minute batches",
			MaxPendingStatsBatches,
			f.agentCoalesced,
		)
	}
}

func MergeDeltas(
	destination, source map[Key]Counters,
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

func MergeAgentDeltas(
	destination, source map[AgentKey]AgentCounters,
) {
	for key, add := range source {
		current := destination[key]
		current.Requests += add.Requests
		current.Input += add.Input
		current.Output += add.Output
		current.CacheCreation += add.CacheCreation
		current.CacheRead += add.CacheRead
		current.LatencySum += add.LatencySum
		current.Failures += add.Failures
		destination[key] = current
	}
}

func (f *Flusher) flushPending(ctx context.Context, limit int) bool {
	wrote := false
	for attempts := 0; len(f.pending) > 0 && attempts < limit; attempts++ {
		batch := f.pending[0]
		if err := f.stats.FlushContext(ctx, batch.minute, batch.deltas); err != nil {
			f.flushFailures++
			if f.flushFailures == 1 || f.flushFailures%60 == 0 {
				logx.Warnf(
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
				logx.Warnf(
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

func (f *Flusher) PendingCounts() (provider, agent int) {
	provider, agent, _ = f.PendingCountsContext(context.Background())
	return provider, agent
}

func (f *Flusher) PendingCountsContext(
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
func (f *Flusher) FlushForShutdown(timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	f.FlushContextCycle(ctx, time.Now())
	for {
		providerPending, agentPending, locked := f.PendingCountsContext(ctx)
		if !locked {
			logx.Warnf("[stats] shutdown retry window exhausted while waiting for the flusher lock")
			return
		}
		if providerPending == 0 && agentPending == 0 {
			return
		}
		if ctx.Err() != nil {
			logx.Warnf(
				"[stats] shutdown retry window exhausted with %d provider and %d agent batches pending",
				providerPending,
				agentPending,
			)
			return
		}
		timer := time.NewTimer(StatsShutdownRetryInterval)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			continue
		}
		f.FlushContextCycle(ctx, time.Now())
	}
}

func (f *Flusher) prune(ctx context.Context, now time.Time) {
	if err := f.stats.PruneContext(ctx, now); err != nil {
		f.pruneFailures++
		if f.pruneFailures == 1 || f.pruneFailures%60 == 0 {
			logx.Warnf("[stats] prune failed: %v (attempt %d)", err, f.pruneFailures)
		}
		return
	}
	f.pruneFailures = 0
}

// reset atomically clears durable history, the three runtime owners, and both
// diff baselines relative to a concurrent flush. Durable reset runs first: on
// failure the live counters remain intact rather than being resurrected from an
// uncleared database at the next process start.
func (f *Flusher) Reset() error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.stats.Reset(); err != nil {
		return err
	}
	if f.metrics != nil {
		f.metrics.Reset()
	}
	if f.tokens != nil {
		f.tokens.Reset()
	}
	if f.agents != nil {
		f.agents.Reset()
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

// UntilNextMinute returns the delay until just past the next minute boundary
// (with a small overshoot so the boundary minute is complete).
func UntilNextMinute(now time.Time) time.Duration {
	next := now.Truncate(time.Minute).Add(time.Minute)
	duration := next.Sub(now) + 200*time.Millisecond
	if duration < 50*time.Millisecond {
		return 50 * time.Millisecond
	}
	return duration
}

// LegacyTokensPath resolves the pre-SQLite token counter file imported once at
// boot (migration source; never written).
func LegacyTokensPath(homeDir string) string {
	return filepath.Join(homeDir, ".model-proxy", "token_usage.json")
}
