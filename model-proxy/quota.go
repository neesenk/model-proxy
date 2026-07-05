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
	mu     sync.RWMutex
	state  map[string]*provider.QuotaSnapshot
	path   string
	cfg    func() *Config
	provs  func() map[string]provider.Provider
	stopCh chan struct{}
	stopOnce sync.Once
	// refreshHook, if set, replaces refreshOne's real poll — used by tests to
	// observe refreshes without hitting a network. If nil, the real poll runs.
	refreshHook func(name string)
}

func newQuotaTracker(path string, cfg func() *Config, provs func() map[string]provider.Provider) *quotaTracker {
	return &quotaTracker{
		state:  map[string]*provider.QuotaSnapshot{},
		path:   path,
		cfg:    cfg,
		provs:  provs,
		stopCh: make(chan struct{}),
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
		time.Sleep(d)
		t.pollAll(time.Now())
	}()
}

// pollAll polls every configured provider in parallel (bounded by the runtime's
// goroutine scheduling; provider count is small) and persists once at the end.
func (t *quotaTracker) pollAll(now time.Time) {
	cfg := t.cfg()
	provs := t.provs()
	var wg sync.WaitGroup
	var mu sync.Mutex
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
			mu.Lock()
			defer mu.Unlock()
			if err != nil || s == nil {
				s = &provider.QuotaSnapshot{Billing: provider.BillingUnknown, AsOf: now}
				if err != nil {
					s.Err = err.Error()
				}
			}
			s.AsOf = now
			t.setSnapshot(n, s)
		}(name, provImpl)
	}
	wg.Wait()
	t.persist()
}

// refreshOne re-polls a single provider (called after a 429). If a refreshHook
// is installed it replaces the real poll (used by tests).
func (t *quotaTracker) refreshOne(name string) {
	if t.refreshHook != nil {
		t.refreshHook(name)
		return
	}
	provs := t.provs()
	p := provs[name]
	if p == nil {
		return
	}
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

// effectiveBilling applies the staleness guard: a snapshot older than 3× the poll
// interval is treated as Unknown. Pay-as-you-go config intent is honored here too.
func (t *quotaTracker) effectiveBilling(name string, interval time.Duration) provider.BillingClass {
	cfg := t.cfg()
	if cfg.Providers[name].Billing == "pay-as-you-go" {
		return provider.BillingPayG
	}
	s := t.snapshot(name)
	if s == nil {
		return provider.BillingUnknown
	}
	if s.Billing == provider.BillingUnknown || s.Err != "" {
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
	data, err := json.MarshalIndent(map[string]any{"providers": out}, "", "  ")
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
}
