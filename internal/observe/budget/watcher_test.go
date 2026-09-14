package budget

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
	observeevents "model-proxy/internal/observe/events"
	observestats "model-proxy/internal/observe/stats"
	"model-proxy/internal/pricing"
)

var testNow = time.Date(2026, 8, 16, 12, 0, 0, 0, time.Local)

// fakePorts is the test double for the composition root: fixed copies of the
// reload-owned state, a fixed analytics result, override-only pricing, and an
// in-memory event sink mirroring the live hub contract.
type fakePorts struct {
	mu        sync.Mutex
	budgets   configdomain.BudgetsConfig
	parentOf  map[string]string
	stateOK   bool
	calls     int
	buckets   []observestats.AnalyticsBucket
	queryErr  error
	overrides map[string]pricing.Override
	catalog   *pricing.Catalog
	aliases   map[string]string
	events    []observeevents.Event
}

// usageBucket is one month bucket: 1M input + 0.5M output on glm-5.2 at
// $1.0/$2.0 per 1M tokens = 1×$1.0 + 0.5×$2.0 = $2.00 equivalent cost.
func usageBucket(provider string) observestats.AnalyticsBucket {
	return observestats.AnalyticsBucket{
		Provider: provider, Model: "glm-5.2",
		Requests: 1, Input: 1_000_000, Output: 500_000,
	}
}

func newFakePorts(budgets configdomain.BudgetsConfig, buckets ...observestats.AnalyticsBucket) *fakePorts {
	return &fakePorts{
		budgets: budgets,
		stateOK: true,
		buckets: buckets,
		overrides: map[string]pricing.Override{
			"glm-5.2": {Input: 1.0, Output: 2.0}, // USD per 1M tokens
		},
	}
}

func (f *fakePorts) ports() Ports {
	return Ports{
		BudgetState: func() (configdomain.BudgetsConfig, map[string]string, bool) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.calls++
			return f.budgets, f.parentOf, f.stateOK
		},
		QueryAnalytics: func(from, to int64) ([]observestats.AnalyticsBucket, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.buckets, f.queryErr
		},
		PricingSnapshot: func() (map[string]pricing.Override, *pricing.Catalog, map[string]string) {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.overrides, f.catalog, f.aliases
		},
		Publish: func(event observeevents.Event) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.events = append(f.events, event)
		},
	}
}

// newTestWatcher constructs the watcher directly (no loop goroutine) so tests
// drive check() with a fixed clock and no sleeps.
func newTestWatcher(f *fakePorts) *Watcher {
	w := NewWatcher(f.ports(), &http.Client{})
	w.now = func() time.Time { return testNow }
	return w
}

func budgetEvents(t *testing.T, f *fakePorts) []alertPayload {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []alertPayload
	for _, event := range f.events {
		if event.Type != "budget" {
			continue
		}
		var payload alertPayload
		if err := json.Unmarshal([]byte(event.Detail), &payload); err != nil {
			t.Fatalf("budget event Detail is not the alert payload: %v", err)
		}
		if payload.Scope != "global" && event.Provider != payload.Scope {
			t.Fatalf("provider-scope event Provider = %q, want %q", event.Provider, payload.Scope)
		}
		out = append(out, payload)
	}
	return out
}

func TestWatcher_GlobalThresholdFiresEventOnce(t *testing.T) {
	f := newFakePorts(configdomain.BudgetsConfig{MonthlyUSD: 1.0}, usageBucket("zhipu"))
	w := newTestWatcher(f)

	w.check(w.now(), nil)
	events := budgetEvents(t, f)
	if len(events) != 1 {
		t.Fatalf("budget events = %d, want 1", len(events))
	}
	got := events[0]
	if got.Scope != "global" || got.Month != "2026-08" || got.ThresholdUSD != 1.0 || got.ActualUSD != 2.0 {
		t.Fatalf("payload = %+v, want {global 2026-08 1 2}", got)
	}

	// Same (scope, month, threshold): no duplicate alert on the next check.
	w.check(w.now(), nil)
	if got := budgetEvents(t, f); len(got) != 1 {
		t.Fatalf("budget events after second check = %d, want 1 (dedup)", len(got))
	}
}

func TestWatcher_BelowThresholdStaysQuiet(t *testing.T) {
	f := newFakePorts(configdomain.BudgetsConfig{MonthlyUSD: 100.0}, usageBucket("zhipu"))
	w := newTestWatcher(f)

	w.check(w.now(), nil)
	if got := budgetEvents(t, f); len(got) != 0 {
		t.Fatalf("budget events = %d, want 0 below threshold", len(got))
	}
}

func TestWatcher_ProviderOverrideBeatsGlobal(t *testing.T) {
	// Global threshold is far above the spend, so only the provider override
	// may fire — and it must carry the override threshold, not the global one.
	f := newFakePorts(configdomain.BudgetsConfig{
		MonthlyUSD: 100.0,
		Providers:  map[string]float64{"zhipu": 1.0, "deepseek": 0},
	}, usageBucket("zhipu"))
	w := newTestWatcher(f)

	w.check(w.now(), nil)
	events := budgetEvents(t, f)
	if len(events) != 1 {
		t.Fatalf("budget events = %d, want exactly 1 (provider scope only)", len(events))
	}
	got := events[0]
	if got.Scope != "zhipu" || got.ThresholdUSD != 1.0 || got.ActualUSD != 2.0 {
		t.Fatalf("payload = %+v, want provider scope with override threshold 1", got)
	}
}

func TestWatcher_PooledVirtualIDAttributesToParent(t *testing.T) {
	// Stats buckets for pooled accounts arrive under the virtual id
	// (name#account); the per-provider scope must aggregate under the parent.
	f := newFakePorts(configdomain.BudgetsConfig{
		Providers: map[string]float64{"zhipu": 1.0},
	}, usageBucket("zhipu#work"), usageBucket("zhipu#personal"))
	f.parentOf = map[string]string{"zhipu#work": "zhipu", "zhipu#personal": "zhipu"}
	w := newTestWatcher(f)

	w.check(w.now(), nil)
	events := budgetEvents(t, f)
	if len(events) != 1 {
		t.Fatalf("budget events = %d, want exactly 1 (parent scope)", len(events))
	}
	got := events[0]
	if got.Scope != "zhipu" || got.ThresholdUSD != 1.0 || got.ActualUSD != 4.0 {
		t.Fatalf("payload = %+v, want parent scope aggregating both virtual ids to $4", got)
	}
}

func TestWatcher_UnpricedModelsContributeNothing(t *testing.T) {
	// mystery has no override and no catalog entry: n/a cost, as in
	// /api/analytics — it must not push the month over the threshold.
	f := newFakePorts(configdomain.BudgetsConfig{MonthlyUSD: 1.0},
		observestats.AnalyticsBucket{Provider: "zhipu", Model: "mystery", Requests: 1, Input: 1_000_000, Output: 500_000})
	w := newTestWatcher(f)

	w.check(w.now(), nil)
	if got := budgetEvents(t, f); len(got) != 0 {
		t.Fatalf("budget events = %d, want 0 (unpriced usage has no known cost)", len(got))
	}
}

func TestWatcher_AliasPricedModelContributes(t *testing.T) {
	// kimi-code meters upstream "k3"; the price lives on the exposed alias
	// "kimi-k3" ($1/$2 per 1M → 1M in + 0.5M out = $2.00) and must count
	// toward the budget exactly like a directly-priced model.
	f := newFakePorts(configdomain.BudgetsConfig{MonthlyUSD: 1.0},
		observestats.AnalyticsBucket{Provider: "kimi-code", Model: "k3", Requests: 1, Input: 1_000_000, Output: 500_000})
	f.overrides = map[string]pricing.Override{"kimi-k3": {Input: 1.0, Output: 2.0}}
	f.aliases = map[string]string{pricing.AliasKey("kimi-code", "k3"): "kimi-k3"}
	w := newTestWatcher(f)

	w.check(w.now(), nil)
	if got := budgetEvents(t, f); len(got) != 1 || got[0].ActualUSD != 2.0 {
		t.Fatalf("budget events = %+v, want one alert at $2 via the alias price", got)
	}
}

func TestWatcher_StateUnavailableSkipsTick(t *testing.T) {
	// ok=false from BudgetState (nil cfg, budgets disabled, or no stats store)
	// must skip the tick silently: no query, no pricing snapshot, no events.
	f := newFakePorts(configdomain.BudgetsConfig{MonthlyUSD: 1.0}, usageBucket("zhipu"))
	f.stateOK = false
	w := newTestWatcher(f)

	w.check(w.now(), nil)
	if got := budgetEvents(t, f); len(got) != 0 {
		t.Fatalf("budget events = %d, want 0 when state unavailable", len(got))
	}
}

func TestWatcher_QueryFailureRetriesNextTick(t *testing.T) {
	f := newFakePorts(configdomain.BudgetsConfig{MonthlyUSD: 1.0}, usageBucket("zhipu"))
	f.queryErr = errTest
	w := newTestWatcher(f)

	w.check(w.now(), nil)
	if got := budgetEvents(t, f); len(got) != 0 {
		t.Fatalf("budget events = %d, want 0 on query failure", len(got))
	}
	f.mu.Lock()
	f.queryErr = nil
	f.mu.Unlock()
	w.check(w.now(), nil)
	if got := budgetEvents(t, f); len(got) != 1 {
		t.Fatalf("budget events after recovery = %d, want 1", len(got))
	}
}

type testError string

func (e testError) Error() string { return string(e) }

const errTest = testError("stats query failed")

func TestWatcher_WebhookPostsPayload(t *testing.T) {
	var (
		mu       sync.Mutex
		bodies   []alertPayload
		failures int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if failures < 2 { // exercise the retry path: 5xx, 5xx, then accept
			failures++
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", ct)
		}
		var payload alertPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode webhook body: %v", err)
			return
		}
		bodies = append(bodies, payload)
	}))
	t.Cleanup(server.Close)

	f := newFakePorts(configdomain.BudgetsConfig{MonthlyUSD: 1.0, WebhookURL: server.URL}, usageBucket("zhipu"))
	w := newTestWatcher(f)

	w.check(w.now(), nil)
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("webhook deliveries = %d, want 1", len(bodies))
	}
	if failures != 2 {
		t.Fatalf("webhook 5xx attempts = %d, want 2 retries before success", failures)
	}
	got := bodies[0]
	if got.Scope != "global" || got.Month != "2026-08" || got.ThresholdUSD != 1.0 || got.ActualUSD != 2.0 {
		t.Fatalf("webhook payload = %+v, want {global 2026-08 1 2}", got)
	}
}

func TestWatcher_Webhook4xxDoesNotRetry(t *testing.T) {
	var (
		mu       sync.Mutex
		attempts int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		w.WriteHeader(http.StatusBadRequest)
	}))
	t.Cleanup(server.Close)

	f := newFakePorts(configdomain.BudgetsConfig{MonthlyUSD: 1.0, WebhookURL: server.URL}, usageBucket("zhipu"))
	w := newTestWatcher(f)
	w.retryBackoff = 0

	w.check(w.now(), nil)
	mu.Lock()
	defer mu.Unlock()
	if attempts != 1 {
		t.Fatalf("webhook attempts = %d, want exactly 1 (4xx is permanent)", attempts)
	}
}

func TestWatcher_Webhook5xxExhaustsRetries(t *testing.T) {
	var (
		mu       sync.Mutex
		attempts int
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		attempts++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(server.Close)

	f := newFakePorts(configdomain.BudgetsConfig{MonthlyUSD: 1.0, WebhookURL: server.URL}, usageBucket("zhipu"))
	w := newTestWatcher(f)
	w.retryBackoff = 0

	w.check(w.now(), nil)
	mu.Lock()
	defer mu.Unlock()
	if attempts != webhookRetries+1 {
		t.Fatalf("webhook attempts = %d, want %d (first try + retries)", attempts, webhookRetries+1)
	}
}

// TestWatcher_WebhookRetryAbortsOnStop: the webhook retry loop runs inside a
// lifecycle task; an unresponsive webhook endpoint must not pin Proxy.Close
// (worst case 3×5s timeouts + backoffs) — the in-flight request and the retry
// sleep both abort when lifecycle stop closes.
func TestWatcher_WebhookRetryAbortsOnStop(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer server.Close()
	defer close(release)

	f := newFakePorts(configdomain.BudgetsConfig{MonthlyUSD: 1.0, WebhookURL: server.URL}, usageBucket("zhipu"))
	w := newTestWatcher(f)
	w.retryBackoff = 0 // all 3 attempts fail fast into the hanging POST

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		w.check(testNow, stop)
		close(done)
	}()
	// Let the first POST park inside the hanging server, then stop.
	time.Sleep(50 * time.Millisecond)
	close(stop)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("webhook retry did not abort on lifecycle stop — Close would be pinned ~17s")
	}
}

// TestWatcher_LoopChecksAtStartThenEachMinute: the loop runs one check
// immediately, then waits out the remainder of the wall-clock minute (the
// stats flush cadence) before the next check, and stops promptly on close.
func TestWatcher_LoopChecksAtStartThenEachMinute(t *testing.T) {
	f := newFakePorts(configdomain.BudgetsConfig{MonthlyUSD: 100.0}, usageBucket("zhipu"))
	w := NewWatcher(f.ports(), &http.Client{})

	// Park the clock 200ms before the next minute boundary so the aligned
	// tick is UntilNextMinute = 200ms remainder + 200ms grace.
	clockMu := sync.Mutex{}
	clock := time.Date(2026, 8, 16, 12, 0, 59, 800_000_000, time.Local)
	w.now = func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return clock
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		w.Loop(stop)
		close(done)
	}()

	waitForCalls := func(want int) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			f.mu.Lock()
			calls := f.calls
			f.mu.Unlock()
			if calls >= want {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		t.Fatalf("BudgetState calls = %d, want >= %d (aligned minute tick missing)", f.calls, want)
	}

	waitForCalls(1) // startup check, no minute wait
	clockMu.Lock()
	clock = clock.Add(time.Minute)
	clockMu.Unlock()
	waitForCalls(2) // first wall-clock-minute-aligned tick

	close(stop)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("loop did not return after stop closed")
	}
}
