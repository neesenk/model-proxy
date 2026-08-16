package app

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
	observestats "model-proxy/internal/observe/stats"
)

var budgetTestNow = time.Date(2026, 8, 16, 12, 0, 0, 0, time.Local)

// newBudgetTestProxy builds a Proxy with an isolated temp stats store and
// offline pricing (catalog disabled; prices: overrides still apply).
func newBudgetTestProxy(t *testing.T, budgets configdomain.BudgetsConfig) *Proxy {
	t.Helper()
	cfg := &Config{
		Budgets: budgets,
		Pricing: PricingConfig{Enabled: false},
		Prices: map[string]PriceConfig{
			"glm-5.2": {Input: 1.0, Output: 2.0}, // USD per 1M tokens
		},
	}
	p := newTestProxy(t, cfg)
	p.stats = newTestStatsStore(t)
	return p
}

// newBudgetTestWatcher constructs the watcher directly (no loop goroutine) so
// tests drive check() with a fixed clock and no sleeps.
func newBudgetTestWatcher(p *Proxy) *budgetWatcher {
	return &budgetWatcher{
		proxy:   p,
		client:  &http.Client{},
		now:     func() time.Time { return budgetTestNow },
		alerted: map[string]bool{},
	}
}

// flushBudgetUsage persists one minute bucket: 1M input + 0.5M output on
// glm-5.2 = 1×$1.0 + 0.5×$2.0 = $2.00 equivalent cost.
func flushBudgetUsage(t *testing.T, p *Proxy, provider string) {
	t.Helper()
	monthStart := time.Date(2026, 8, 1, 0, 0, 0, 0, time.Local)
	err := p.stats.FlushContext(context.Background(), monthStart.Unix(), map[observestats.Key]observestats.Counters{
		{Provider: provider, Model: "glm-5.2"}: {
			Requests: 1, Input: 1_000_000, Output: 500_000,
		},
	})
	if err != nil {
		t.Fatalf("flush usage: %v", err)
	}
}

func budgetEvents(t *testing.T, p *Proxy) []budgetAlertPayload {
	t.Helper()
	var out []budgetAlertPayload
	for _, event := range p.events.Snapshot() {
		if event.Type != "budget" {
			continue
		}
		var payload budgetAlertPayload
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

func TestBudgetWatcher_GlobalThresholdFiresEventOnce(t *testing.T) {
	p := newBudgetTestProxy(t, configdomain.BudgetsConfig{MonthlyUSD: 1.0})
	flushBudgetUsage(t, p, "zhipu")
	w := newBudgetTestWatcher(p)

	w.check(w.now(), nil)
	events := budgetEvents(t, p)
	if len(events) != 1 {
		t.Fatalf("budget events = %d, want 1", len(events))
	}
	got := events[0]
	if got.Scope != "global" || got.Month != "2026-08" || got.ThresholdUSD != 1.0 || got.ActualUSD != 2.0 {
		t.Fatalf("payload = %+v, want {global 2026-08 1 2}", got)
	}

	// Same (scope, month, threshold): no duplicate alert on the next check.
	w.check(w.now(), nil)
	if got := budgetEvents(t, p); len(got) != 1 {
		t.Fatalf("budget events after second check = %d, want 1 (dedup)", len(got))
	}
}

func TestBudgetWatcher_BelowThresholdStaysQuiet(t *testing.T) {
	p := newBudgetTestProxy(t, configdomain.BudgetsConfig{MonthlyUSD: 100.0})
	flushBudgetUsage(t, p, "zhipu")
	w := newBudgetTestWatcher(p)

	w.check(w.now(), nil)
	if got := budgetEvents(t, p); len(got) != 0 {
		t.Fatalf("budget events = %d, want 0 below threshold", len(got))
	}
}

func TestBudgetWatcher_ProviderOverrideBeatsGlobal(t *testing.T) {
	// Global threshold is far above the spend, so only the provider override
	// may fire — and it must carry the override threshold, not the global one.
	p := newBudgetTestProxy(t, configdomain.BudgetsConfig{
		MonthlyUSD: 100.0,
		Providers:  map[string]float64{"zhipu": 1.0, "deepseek": 0},
	})
	flushBudgetUsage(t, p, "zhipu")
	w := newBudgetTestWatcher(p)

	w.check(w.now(), nil)
	events := budgetEvents(t, p)
	if len(events) != 1 {
		t.Fatalf("budget events = %d, want exactly 1 (provider scope only)", len(events))
	}
	got := events[0]
	if got.Scope != "zhipu" || got.ThresholdUSD != 1.0 || got.ActualUSD != 2.0 {
		t.Fatalf("payload = %+v, want provider scope with override threshold 1", got)
	}
}

func TestBudgetWatcher_WebhookPostsPayload(t *testing.T) {
	var (
		mu       sync.Mutex
		bodies   []budgetAlertPayload
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
		var payload budgetAlertPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode webhook body: %v", err)
			return
		}
		bodies = append(bodies, payload)
	}))
	t.Cleanup(server.Close)

	p := newBudgetTestProxy(t, configdomain.BudgetsConfig{MonthlyUSD: 1.0, WebhookURL: server.URL})
	flushBudgetUsage(t, p, "zhipu")
	w := newBudgetTestWatcher(p)

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

func TestBudgetWatcher_NotStartedWhenUnconfigured(t *testing.T) {
	p := newTestProxy(t, &Config{})
	p.startBudgetWatcher()
	if p.budget != nil {
		t.Fatal("budget watcher started without any configured threshold")
	}
}

func TestBudgetWatcher_StartedWhenConfigured(t *testing.T) {
	// stats is nil here: the loop's startup check must tolerate it and simply
	// wait for the next tick (Close stops and waits the loop goroutine).
	p := newTestProxy(t, &Config{Budgets: configdomain.BudgetsConfig{MonthlyUSD: 10}})
	p.startBudgetWatcher()
	if p.budget == nil {
		t.Fatal("budget watcher did not start with a configured threshold")
	}
}

// TestBudgetWatcher_WebhookRetryAbortsOnStop: the webhook retry loop runs
// inside a lifecycle task; an unresponsive webhook endpoint must not pin
// Proxy.Close (worst case 3×5s timeouts + backoffs) — the in-flight request
// and the retry sleep both abort when lifecycle stop closes.
func TestBudgetWatcher_WebhookRetryAbortsOnStop(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer server.Close()
	defer close(release)

	p := newBudgetTestProxy(t, configdomain.BudgetsConfig{MonthlyUSD: 1.0, WebhookURL: server.URL})
	flushBudgetUsage(t, p, "zhipu")

	stop := make(chan struct{})
	watcher := newBudgetTestWatcher(p)
	watcher.retryBackoff = 0 // all 3 attempts fail fast into the hanging POST
	done := make(chan struct{})
	go func() {
		watcher.check(budgetTestNow, stop)
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
