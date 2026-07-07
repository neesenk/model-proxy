package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"

	"model-proxy/provider"
)

// quotaTracker polls providers' Quota() periodically, caches the results in
// memory + a file (~/.model-proxy/quota_state.json), and serves them to the
// scheduler. It has its own mutex (quotaMu), independent of healthMu / reload mu.
type quotaTracker struct {
	mu       sync.RWMutex
	state    map[string]*provider.QuotaSnapshot
	path     string
	cfg      func() *Config
	provs    func() map[string]provider.Provider
	stopCh   chan struct{}
	stopOnce sync.Once
	// refreshHook, if set, replaces refreshOne's real poll — used by tests to
	// observe refreshes without hitting a network. If nil, the real poll runs.
	refreshHook func(name string)
	// refreshGuard dedupes 429-triggered refreshes per provider (inFlight
	// coalesces concurrent ones; last debounces ones that just ran), so a 429
	// storm doesn't fire N upstream Quota() calls + N persists. Guarded by mu.
	refreshGuard map[string]*refreshState
	// stickySnapshot, if set, returns the current per-route sticky map for
	// persistence — restored on boot so the proxy resumes parking on the same
	// providers (prompt-cache-friendly across restarts, and gives visibility into
	// the previous selection).
	stickySnapshot func() map[string]routeSticky
	// LoadedSticky is populated by load() on boot; NewProxy applies it to p.sticky.
	LoadedSticky map[string]routeSticky
}

// refreshState tracks per-provider refresh dedup state (guarded by quotaTracker.mu).
type refreshState struct {
	last     time.Time
	inFlight bool
}

func newQuotaTracker(path string, cfg func() *Config, provs func() map[string]provider.Provider) *quotaTracker {
	return &quotaTracker{
		state:        map[string]*provider.QuotaSnapshot{},
		refreshGuard: map[string]*refreshState{},
		path:         path,
		cfg:          cfg,
		provs:        provs,
		stopCh:       make(chan struct{}),
	}
}

func (t *quotaTracker) start() {
	t.load() // baseline before first poll
	go func() {
		// bootstrap poll shortly after start
		t.pollAfter(10 * time.Second)
		interval := t.cfg().Scheduling.pollInterval()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				t.pollAll(time.Now())
			case <-t.stopCh:
				return
			}
		}
	}()
}

func (t *quotaTracker) stop() { t.stopOnce.Do(func() { close(t.stopCh) }) }

func (t *quotaTracker) pollAfter(d time.Duration) {
	go func() {
		select {
		case <-time.After(d):
			t.pollAll(time.Now())
		case <-t.stopCh:
		}
	}()
}

// pollAll polls every configured provider in parallel (bounded by the runtime's
// goroutine scheduling; provider count is small) and persists once at the end.
func (t *quotaTracker) pollAll(now time.Time) {
	cfg := t.cfg()
	provs := t.provs()
	var wg sync.WaitGroup
	names := make([]string, 0, len(cfg.Providers))
	for name := range cfg.Providers {
		names = append(names, name)
	}
	for _, name := range names {
		provImpl := provs[name]
		if provImpl == nil {
			continue
		}
		wg.Add(1)
		go func(n string, p provider.Provider) {
			defer wg.Done()
			s, err := p.Quota()
			if err != nil || s == nil {
				s = &provider.QuotaSnapshot{Billing: provider.BillingUnknown, AsOf: now}
				if err != nil {
					s.Err = err.Error()
				}
			}
			s.AsOf = now
			t.setSnapshot(n, s) // setSnapshot takes t.mu — no extra guard needed
		}(name, provImpl)
	}
	wg.Wait()
	t.persist()
}

// refreshOne re-polls a single provider (called after a 429). If a refreshHook
// is installed it replaces the real poll (used by tests). Otherwise the call is
// deduped: a concurrent refresh (inFlight) or one that ran less than
// pollInterval/2 ago (last) is dropped, so a 429 storm doesn't fire N upstream
// Quota() calls + N persists for the same provider.
func (t *quotaTracker) refreshOne(name string) {
	if t.refreshHook != nil {
		t.refreshHook(name)
		return
	}
	now := time.Now()
	half := t.cfg().Scheduling.pollInterval() / 2
	t.mu.Lock()
	g := t.refreshGuard[name]
	if g == nil {
		g = &refreshState{}
		t.refreshGuard[name] = g
	}
	if g.inFlight || now.Sub(g.last) < half {
		t.mu.Unlock()
		return // coalesced/debounced — a covering refresh already ran or is running
	}
	g.inFlight = true
	t.mu.Unlock()

	refreshed := false
	if p := t.provs()[name]; p != nil {
		s, err := p.Quota()
		if err != nil || s == nil {
			s = &provider.QuotaSnapshot{Billing: provider.BillingUnknown, AsOf: time.Now()}
			if err != nil {
				s.Err = err.Error()
			}
		}
		s.AsOf = time.Now()
		t.setSnapshot(name, s)
		t.persist()
		refreshed = true
	}

	t.mu.Lock()
	g.inFlight = false
	if refreshed {
		g.last = time.Now()
	}
	t.mu.Unlock()
}

func (t *quotaTracker) setSnapshot(name string, s *provider.QuotaSnapshot) {
	t.mu.Lock()
	t.state[name] = s
	t.mu.Unlock()
}

func (t *quotaTracker) snapshot(name string) *provider.QuotaSnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.state[name]
}

func (t *quotaTracker) allSnapshots() map[string]*provider.QuotaSnapshot {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[string]*provider.QuotaSnapshot, len(t.state))
	for k, v := range t.state {
		out[k] = v
	}
	return out
}

// classifyBilling applies the pay-as-you-go config override, the error guard,
// and the staleness guard (a snapshot older than 3× the poll interval, or one
// carrying an error, is treated as Unknown). Pure; shared by Proxy.billingClass
// so the scheduling-tier logic lives in one place.
func classifyBilling(s *provider.QuotaSnapshot, billingCfg string, interval time.Duration) provider.BillingClass {
	if billingCfg == "pay-as-you-go" {
		return provider.BillingPayG
	}
	if s == nil || s.Billing == provider.BillingUnknown || s.Err != "" {
		return provider.BillingUnknown
	}
	if time.Since(s.AsOf) > 3*interval {
		return provider.BillingUnknown
	}
	return s.Billing
}

type persistedSnapshot struct {
	Billing      provider.BillingClass  `json:"billing"`
	RemainingPct float64                `json:"remaining_pct"`
	Windows      []provider.QuotaWindow `json:"windows"`
	AsOf         time.Time              `json:"as_of"`
	Err          string                 `json:"err,omitempty"`
}

type persistedSticky struct {
	Provider string    `json:"provider"`
	Since    time.Time `json:"since"`
}

func (t *quotaTracker) persist() {
	t.mu.RLock()
	out := make(map[string]persistedSnapshot, len(t.state))
	for k, v := range t.state {
		out[k] = persistedSnapshot{
			Billing: v.Billing, RemainingPct: v.RemainingPct,
			Windows: v.Windows, AsOf: v.AsOf, Err: v.Err,
		}
	}
	t.mu.RUnlock()
	wrap := map[string]any{"providers": out}
	if t.stickySnapshot != nil {
		// stickySnapshot takes healthMu (proxy.go). It MUST be called outside
		// quotaMu — calling it inside the RLock above would invert the lock
		// order (healthMu → quotaMu is the rule; reverse = deadlock risk).
		sm := t.stickySnapshot()
		sticky := make(map[string]persistedSticky, len(sm))
		for k, v := range sm {
			sticky[k] = persistedSticky{Provider: v.provider, Since: v.since}
		}
		wrap["sticky"] = sticky
	}
	data, err := json.MarshalIndent(wrap, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(t.path), 0o700); err != nil {
		return
	}
	tmp := t.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, t.path); err != nil {
		log.Printf("[quota] persist rename failed: %v", err)
	}
}

func (t *quotaTracker) load() {
	data, err := os.ReadFile(t.path)
	if err != nil {
		return
	}
	var wrap struct {
		Providers map[string]persistedSnapshot `json:"providers"`
		Sticky    map[string]persistedSticky   `json:"sticky"`
	}
	if err := json.Unmarshal(data, &wrap); err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for k, v := range wrap.Providers {
		t.state[k] = &provider.QuotaSnapshot{
			Billing: v.Billing, RemainingPct: v.RemainingPct,
			Windows: v.Windows, AsOf: v.AsOf, Err: v.Err,
		}
	}
	t.LoadedSticky = make(map[string]routeSticky, len(wrap.Sticky))
	for k, v := range wrap.Sticky {
		t.LoadedSticky[k] = routeSticky{provider: v.Provider, since: v.Since}
	}
}
