package app

import (
	"context"
	"encoding/json"
	"errors"
	configdomain "model-proxy/internal/config"
	obscounters "model-proxy/internal/observe/counters"
	runtimestate "model-proxy/internal/runtime"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"fmt"
	observestats "model-proxy/internal/observe/stats"
)

func openRuntimeStatsStore(
	t *testing.T,
	path string,
	retention time.Duration,
) *observestats.Store {
	t.Helper()
	store, err := observestats.Open(observestats.Options{
		Path: path, Retention: retention,
	})
	if err != nil {
		t.Fatalf("open stats store: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	return store
}

func addRuntimeStats(
	metrics *obscounters.MetricsStore,
	tokens *obscounters.TokenCounter,
	agents *obscounters.AgentCounter,
	requests int,
	input uint64,
) {
	for range requests {
		metrics.Inc("provider", "model", obscounters.EvRequests)
		agents.IncRequests("codex", "provider", "model")
	}
	if input > 0 {
		usage := obscounters.TokenUsage{Input: input}
		tokens.Commit(obscounters.TokenKey{Provider: "provider", Model: "model"}, usage)
		agents.AddTokens("codex", "provider", "model", usage)
	}
}

type blockingStatsSink struct {
	*observestats.Store
	entered      chan struct{}
	release      chan struct{}
	resetEntered chan struct{}
	flushOnce    sync.Once
	releaseOnce  sync.Once
	resetOnce    sync.Once
}

func (sink *blockingStatsSink) FlushContext(
	ctx context.Context,
	minute int64,
	deltas map[observestats.Key]observestats.Counters,
) error {
	sink.flushOnce.Do(func() {
		close(sink.entered)
		select {
		case <-sink.release:
		case <-ctx.Done():
		}
	})
	if err := ctx.Err(); err != nil {
		return err
	}
	return sink.Store.FlushContext(ctx, minute, deltas)
}

func (sink *blockingStatsSink) Reset() error {
	sink.resetOnce.Do(func() {
		close(sink.resetEntered)
	})
	return sink.Store.Reset()
}

func (sink *blockingStatsSink) unblock() {
	sink.releaseOnce.Do(func() {
		close(sink.release)
	})
}

func TestInitStatsRestoresAllRuntimeFieldsWithoutDuplicateFlush(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "stats.db")
	key := observestats.Key{Provider: "provider", Model: "model"}
	persisted := observestats.Counters{
		Requests: 11, Failovers: 2, RateLimited429: 3, Failures: 4,
		Input: 101, Output: 202, CacheCreation: 303, CacheRead: 404,
		TokenRequests: 5, LastRequestAt: 1_234,
		LatencySum: 5_050, TTFTSum: 606,
	}
	seed, err := observestats.Open(observestats.Options{Path: path})
	if err != nil {
		t.Fatalf("open seed store: %v", err)
	}
	if err := seed.Flush(60, map[observestats.Key]observestats.Counters{
		key: persisted,
	}); err != nil {
		_ = seed.Close()
		t.Fatalf("seed stats: %v", err)
	}
	if err := seed.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	proxy := &Proxy{
		processServices: processServices{
			metrics: obscounters.NewMetricsStore(),
			tokens:  obscounters.NewTokenCounter(),
			agents:  obscounters.NewAgentCounter(),
		},
	}
	proxy.initStats(configdomain.StatsConfig{DBPath: path, Retention: "0"})
	if proxy.stats == nil || proxy.flusher == nil {
		t.Fatal("initStats did not bind the durable store and runtime flusher")
	}
	t.Cleanup(func() {
		_ = proxy.stats.Close()
	})

	runtimeKey := obscounters.PMKey{Provider: key.Provider, Model: key.Model}
	wantMetrics := obscounters.ProviderMetricsSnapshot{
		Requests: persisted.Requests, Failovers: persisted.Failovers,
		RateLimited429: persisted.RateLimited429, Failures: persisted.Failures,
		LastRequestAt: persisted.LastRequestAt, LatencySum: persisted.LatencySum,
		TTFTSum: persisted.TTFTSum,
	}
	if got := proxy.metrics.Snapshot()[runtimeKey]; got != wantMetrics {
		t.Errorf("restored metrics = %+v, want %+v", got, wantMetrics)
	}
	wantTokens := obscounters.TokenUsage{
		Input: persisted.Input, Output: persisted.Output,
		CacheCreation: persisted.CacheCreation, CacheRead: persisted.CacheRead,
		Requests: persisted.TokenRequests,
	}
	if got := proxy.tokens.Snapshot()[runtimeKey]; got != wantTokens {
		t.Errorf("restored tokens = %+v, want %+v", got, wantTokens)
	}

	if proxy.flusher.Flush(time.Unix(180, 0)) {
		t.Fatal("unchanged restored counters were written again on the first flush")
	}
	cumulative, err := proxy.stats.LoadCumulative()
	if err != nil {
		t.Fatal(err)
	}
	if got := cumulative[key]; got != persisted {
		t.Errorf("first flush changed durable cumulative counters: got %+v, want %+v", got, persisted)
	}
}

func TestStatsResetSerializesWithFlushAndRebaselines(t *testing.T) {
	oldProcs := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(oldProcs)

	store := openRuntimeStatsStore(t, filepath.Join(t.TempDir(), "stats.db"), 0)
	sink := &blockingStatsSink{
		Store: store, entered: make(chan struct{}), release: make(chan struct{}),
		resetEntered: make(chan struct{}),
	}
	t.Cleanup(sink.unblock)
	metrics := obscounters.NewMetricsStore()
	tokens := obscounters.NewTokenCounter()
	agents := obscounters.NewAgentCounter()
	flusher := observestats.NewFlusher(sink, metrics, tokens, agents, nil, nil)
	proxy := &Proxy{
		processServices: processServices{
			metrics: metrics, tokens: tokens, agents: agents,
			stats: store, flusher: flusher,
		},
	}
	addRuntimeStats(metrics, tokens, agents, 3, 30)

	flushDone := make(chan struct{})
	go func() {
		flusher.Flush(time.Unix(120, 0))
		close(flushDone)
	}()
	<-sink.entered // Flush holds flusher.mu before entering the Store port.

	resetStarted := make(chan struct{})
	resetDone := make(chan error, 1)
	go func() {
		close(resetStarted)
		resetDone <- proxy.resetStats()
	}()
	<-resetStarted
	runtime.Gosched()
	select {
	case <-sink.resetEntered:
		t.Fatal("durable reset entered while flush still held the flusher lock")
	default:
	}

	sink.unblock()
	<-flushDone
	if err := <-resetDone; err != nil {
		t.Fatalf("resetStats: %v", err)
	}
	select {
	case <-sink.resetEntered:
	default:
		t.Fatal("reset completed without entering the durable reset")
	}

	rows, err := store.QueryRange(0, 300, "", "", 60)
	if err != nil {
		t.Fatal(err)
	}
	agentRows, err := store.QueryAgents(0, 300, "", "", "", 60)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 || len(agentRows) != 0 {
		t.Fatalf("reset left durable rows: stats=%+v agents=%+v", rows, agentRows)
	}
	if len(metrics.Snapshot()) != 0 || len(tokens.Snapshot()) != 0 || len(agents.Snapshot()) != 0 {
		t.Fatalf("reset left runtime counters: metrics=%+v tokens=%+v agents=%+v",
			metrics.Snapshot(), tokens.Snapshot(), agents.Snapshot())
	}

	addRuntimeStats(metrics, tokens, agents, 1, 5)
	if !flusher.Flush(time.Unix(180, 0)) {
		t.Fatal("first post-reset delta was not persisted")
	}
	rows, _ = store.QueryRange(0, 300, "", "", 60)
	agentRows, _ = store.QueryAgents(0, 300, "", "", "", 60)
	if len(rows) != 1 || rows[0].Requests != 1 || rows[0].Input != 5 ||
		rows[0].TokenRequests != 1 {
		t.Errorf("post-reset stats = %+v, want exactly one request/input delta", rows)
	}
	if len(agentRows) != 1 || agentRows[0].Requests != 1 || agentRows[0].Input != 5 {
		t.Errorf("post-reset agent stats = %+v, want exactly one request/input delta", agentRows)
	}
}

type failAgentOnceSink struct {
	*observestats.Store
	mu        sync.Mutex
	failAgent bool
}

func (sink *failAgentOnceSink) FlushAgentsContext(
	ctx context.Context,
	minute int64,
	deltas map[observestats.AgentKey]observestats.AgentCounters,
) error {
	sink.mu.Lock()
	fail := sink.failAgent
	sink.failAgent = false
	sink.mu.Unlock()
	if fail {
		return errors.New("injected agent flush failure")
	}
	return sink.Store.FlushAgentsContext(ctx, minute, deltas)
}

func TestStatsFlusherRetriesPipelinesIndependently(t *testing.T) {
	store := openRuntimeStatsStore(t, filepath.Join(t.TempDir(), "stats.db"), 0)
	sink := &failAgentOnceSink{Store: store, failAgent: true}
	metrics := obscounters.NewMetricsStore()
	agents := obscounters.NewAgentCounter()
	flusher := observestats.NewFlusher(sink, metrics, obscounters.NewTokenCounter(), agents, nil, nil)

	for range 2 {
		metrics.Inc("provider", "model", obscounters.EvRequests)
	}
	for range 3 {
		agents.IncRequests("codex", "provider", "model")
	}
	if !flusher.Flush(time.Unix(120, 0)) {
		t.Fatal("primary success should report a durable write")
	}
	cumulative, _ := store.LoadCumulative()
	if got := cumulative[observestats.Key{Provider: "provider", Model: "model"}].Requests; got != 2 {
		t.Fatalf("primary first flush = %d requests, want 2", got)
	}
	if rows, _ := store.QueryAgents(0, 300, "", "", "", 60); len(rows) != 0 {
		t.Fatalf("failed agent pipeline unexpectedly advanced: %+v", rows)
	}

	metrics.Inc("provider", "model", obscounters.EvRequests)
	agents.IncRequests("codex", "provider", "model")
	if !flusher.Flush(time.Unix(180, 0)) {
		t.Fatal("recovery flush did not persist deltas")
	}
	cumulative, _ = store.LoadCumulative()
	if got := cumulative[observestats.Key{Provider: "provider", Model: "model"}].Requests; got != 3 {
		t.Errorf("primary total = %d, want 3 without duplicate retry", got)
	}
	agentRows, _ := store.QueryAgents(0, 300, "", "", "", 60)
	if len(agentRows) != 2 ||
		agentRows[0].Minute != 60 || agentRows[0].Requests != 3 ||
		agentRows[1].Minute != 120 || agentRows[1].Requests != 1 {
		t.Errorf("agent retry = %+v, want original 60:3 batch then 120:1 batch", agentRows)
	}
}

func TestStatsFlusherPrunesDuringIdleMinute(t *testing.T) {
	now := time.Unix(10_000, 0)
	store := openRuntimeStatsStore(
		t,
		filepath.Join(t.TempDir(), "stats.db"),
		time.Hour,
	)
	old := now.Add(-2*time.Hour).Unix() / 60 * 60
	if err := store.Flush(old, map[observestats.Key]observestats.Counters{
		{Provider: "old", Model: "model"}: {Requests: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FlushAgents(old, map[observestats.AgentKey]observestats.AgentCounters{
		{Agent: "codex", Provider: "old", Model: "model"}: {Requests: 1},
	}); err != nil {
		t.Fatal(err)
	}

	flusher := observestats.NewFlusher(
		store, obscounters.NewMetricsStore(), obscounters.NewTokenCounter(), obscounters.NewAgentCounter(), nil, nil)
	if flusher.Flush(now) {
		t.Fatal("idle prune reported a counter write")
	}
	rows, _ := store.QueryRange(0, now.Unix()+60, "", "", 60)
	agentRows, _ := store.QueryAgents(0, now.Unix()+60, "", "", "", 60)
	if len(rows) != 0 || len(agentRows) != 0 {
		t.Fatalf("idle minute did not prune both tables: stats=%+v agents=%+v", rows, agentRows)
	}
}

func TestProxyCloseFinalFlushesOnceAndClosesStatsStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.db")
	store := openRuntimeStatsStore(t, path, 0)
	metrics := obscounters.NewMetricsStore()
	tokens := obscounters.NewTokenCounter()
	agents := obscounters.NewAgentCounter()
	flusher := observestats.NewFlusher(store, metrics, tokens, agents, nil, nil)
	proxy := &Proxy{
		processServices: processServices{
			lifecycle: runtimestate.NewLifecycle(),
			metrics:   metrics,
			tokens:    tokens,
			agents:    agents,
			stats:     store,
			flusher:   flusher,
		},
	}
	t.Cleanup(proxy.Close)

	addRuntimeStats(metrics, tokens, agents, 1, 10)
	if !flusher.Flush(time.Unix(120, 0)) {
		t.Fatal("pre-close baseline flush did not write")
	}
	addRuntimeStats(metrics, tokens, agents, 2, 20)

	proxy.Close()
	proxy.Close() // idempotence must not duplicate the final delta.
	if _, err := store.QueryRange(0, time.Now().Unix()+3600, "", "", 60); err == nil {
		t.Fatal("Proxy.Close left the stats Store usable; want closed database")
	}

	reopened, err := observestats.Open(observestats.Options{Path: path})
	if err != nil {
		t.Fatalf("reopen final stats: %v", err)
	}
	defer reopened.Close()
	cumulative, err := reopened.LoadCumulative()
	if err != nil {
		t.Fatal(err)
	}
	got := cumulative[observestats.Key{Provider: "provider", Model: "model"}]
	if got.Requests != 3 || got.Input != 30 || got.TokenRequests != 2 {
		t.Errorf("final cumulative = %+v, want requests=3 input=30 token_requests=2", got)
	}
	agentRows, err := reopened.QueryAgents(0, time.Now().Unix()+3600, "", "", "", 60)
	if err != nil {
		t.Fatal(err)
	}
	var agentRequests, agentInput uint64
	for _, row := range agentRows {
		agentRequests += row.Requests
		agentInput += row.Input
	}
	if agentRequests != 3 || agentInput != 30 {
		t.Errorf("final agent totals = requests=%d input=%d, want 3/30", agentRequests, agentInput)
	}
}

func TestProxyCloseRetriesTransientFinalStatsFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stats.db")
	store := openRuntimeStatsStore(t, path, 0)
	metrics := obscounters.NewMetricsStore()
	tokens := obscounters.NewTokenCounter()
	agents := obscounters.NewAgentCounter()
	sink := &failOnceStatsSink{Store: store}
	proxy := &Proxy{
		processServices: processServices{
			lifecycle: runtimestate.NewLifecycle(),
			metrics:   metrics,
			tokens:    tokens,
			agents:    agents,
			stats:     store,
			flusher:   observestats.NewFlusher(sink, metrics, tokens, agents, nil, nil),
		},
	}
	t.Cleanup(proxy.Close)
	addRuntimeStats(metrics, tokens, agents, 2, 20)

	proxy.Close()
	if sink.failNext {
		t.Fatal("shutdown did not exercise the injected final-flush failure")
	}

	reopened, err := observestats.Open(observestats.Options{Path: path})
	if err != nil {
		t.Fatalf("reopen final stats: %v", err)
	}
	defer reopened.Close()
	cumulative, err := reopened.LoadCumulative()
	if err != nil {
		t.Fatal(err)
	}
	got := cumulative[observestats.Key{Provider: "provider", Model: "model"}]
	if got.Requests != 2 || got.Input != 20 || got.TokenRequests != 1 {
		t.Errorf("retried final cumulative = %+v, want requests=2 input=20 token_requests=1", got)
	}
}

type blockingShutdownStatsSink struct {
	*observestats.Store
	entered chan struct{}
	once    sync.Once
}

func (sink *blockingShutdownStatsSink) FlushContext(
	ctx context.Context,
	_ int64,
	_ map[observestats.Key]observestats.Counters,
) error {
	sink.once.Do(func() {
		close(sink.entered)
	})
	<-ctx.Done()
	return ctx.Err()
}

func TestStatsShutdownFlushHonorsContextDeadline(t *testing.T) {
	store := openRuntimeStatsStore(t, filepath.Join(t.TempDir(), "stats.db"), 0)
	metrics := obscounters.NewMetricsStore()
	tokens := obscounters.NewTokenCounter()
	agents := obscounters.NewAgentCounter()
	sink := &blockingShutdownStatsSink{
		Store: store, entered: make(chan struct{}),
	}
	flusher := observestats.NewFlusher(sink, metrics, tokens, agents, nil, nil)
	addRuntimeStats(metrics, tokens, agents, 1, 5)

	const timeout = 75 * time.Millisecond
	start := time.Now()
	flusher.FlushForShutdown(timeout)
	elapsed := time.Since(start)
	select {
	case <-sink.entered:
	default:
		t.Fatal("shutdown did not enter the context-aware Store port")
	}
	if elapsed < timeout/2 || elapsed > 500*time.Millisecond {
		t.Errorf("shutdown flush elapsed = %v, want a bounded wait near %v", elapsed, timeout)
	}
	providerPending, agentPending := flusher.PendingCounts()
	if providerPending != 1 || agentPending != 1 {
		t.Errorf("pending after deadline = provider:%d agent:%d, want 1/1",
			providerPending, agentPending)
	}
}

func TestTokensResetClearsDurableStatsButNotResponseCache(t *testing.T) {
	store := openRuntimeStatsStore(t, filepath.Join(t.TempDir(), "stats.db"), 0)
	metrics := obscounters.NewMetricsStore()
	tokens := obscounters.NewTokenCounter()
	agents := obscounters.NewAgentCounter()
	cache := NewResponseCache(configdomain.CacheConfig{Enabled: true, TTL: "1h"}, nil)
	flusher := observestats.NewFlusher(store, metrics, tokens, agents, nil, nil)
	proxy := &Proxy{
		generationState: generationState{
			cache: cache,
		},
		processServices: processServices{
			metrics: metrics, tokens: tokens, agents: agents,
			stats: store, flusher: flusher,
		},
	}
	addRuntimeStats(metrics, tokens, agents, 2, 20)
	if !flusher.Flush(time.Unix(120, 0)) {
		t.Fatal("reset precondition flush did not write")
	}
	cache.Put("key", "m", http.StatusOK, http.Header{"X-Test": {"value"}}, []byte("body"), time.Now())

	recorder := httptest.NewRecorder()
	serveWeb(NewWebServer(proxy, "test-config.yaml"),
		recorder,
		httptest.NewRequest(http.MethodPost, "/api/tokens/reset", nil),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf("reset status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response) != 1 || response["status"] != "reset" {
		t.Errorf("reset response = %#v, want exact status envelope", response)
	}
	rows, _ := store.QueryRange(0, 300, "", "", 60)
	agentRows, _ := store.QueryAgents(0, 300, "", "", "", 60)
	if len(rows) != 0 || len(agentRows) != 0 ||
		len(metrics.Snapshot()) != 0 || len(tokens.Snapshot()) != 0 ||
		len(agents.Snapshot()) != 0 {
		t.Fatalf("reset incomplete: rows=%+v agents=%+v metrics=%+v tokens=%+v agentCounters=%+v",
			rows, agentRows, metrics.Snapshot(), tokens.Snapshot(), agents.Snapshot())
	}
	// The response cache is operational accounting, deliberately decoupled
	// from "reset counters": its entries (and hit/miss history) survive.
	if cache.Stats().Entries != 1 {
		t.Fatalf("reset cleared response-cache entries: %+v", cache.Stats())
	}
}

type resetErrorSink struct {
	*observestats.Store
}

func (sink *resetErrorSink) Reset() error {
	return errors.New("injected durable reset failure")
}

func TestTokensResetFailurePreservesLiveState(t *testing.T) {
	store := openRuntimeStatsStore(t, filepath.Join(t.TempDir(), "stats.db"), 0)
	metrics := obscounters.NewMetricsStore()
	tokens := obscounters.NewTokenCounter()
	agents := obscounters.NewAgentCounter()
	cache := NewResponseCache(configdomain.CacheConfig{Enabled: true, TTL: "1h"}, nil)
	flusher := observestats.NewFlusher(
		&resetErrorSink{Store: store}, metrics, tokens, agents, nil, nil)
	proxy := &Proxy{
		generationState: generationState{
			cache: cache,
		},
		processServices: processServices{
			metrics: metrics, tokens: tokens, agents: agents,
			stats: store, flusher: flusher,
		},
	}
	addRuntimeStats(metrics, tokens, agents, 1, 5)
	cache.Put("key", "m", http.StatusOK, nil, []byte("body"), time.Now())

	recorder := httptest.NewRecorder()
	serveWeb(NewWebServer(proxy, "test-config.yaml"),
		recorder,
		httptest.NewRequest(http.MethodPost, "/api/tokens/reset", nil),
	)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("reset failure status = %d, want 500; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "injected durable reset failure") {
		t.Errorf("reset failure body = %s", recorder.Body.String())
	}
	if metrics.Snapshot()[obscounters.PMKey{Provider: "provider", Model: "model"}].Requests != 1 ||
		tokens.Snapshot()[obscounters.TokenKey{Provider: "provider", Model: "model"}].Input != 5 ||
		agents.Snapshot()[obscounters.AgentKey{Agent: "codex", Provider: "provider", Model: "model"}].Requests != 1 ||
		cache.Stats().Entries != 1 {
		t.Fatalf("failed reset mutated live state: metrics=%+v tokens=%+v agents=%+v cache=%+v",
			metrics.Snapshot(), tokens.Snapshot(), agents.Snapshot(), cache.Stats())
	}
}

// failOnceStatsSink injects one FlushContext failure — the transient-failure
// seam for the shutdown/reset integration tests. Test-side only: production
// packages carry no test-only hooks (testing.md "模块归属").
type failOnceStatsSink struct {
	*observestats.Store
	failNext bool
}

func (s *failOnceStatsSink) FlushContext(
	ctx context.Context,
	minute int64,
	deltas map[observestats.Key]observestats.Counters,
) error {
	if s.failNext {
		s.failNext = false
		return fmt.Errorf("injected stats flush failure")
	}
	return s.Store.FlushContext(ctx, minute, deltas)
}
