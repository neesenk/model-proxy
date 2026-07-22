package main

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
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
	// persistMu serializes persist(): pollAll/pollOne/refreshOne run on
	// independent goroutines and share one fixed .tmp sibling — without
	// serialization a second writer's rename fails ENOENT (and interleaved
	// writes could corrupt the file).
	persistMu sync.Mutex
	// retryAttempts/retryBackoff tune fetchQuota's transient-error retry.
	// Defaults (3 / 1s) are set in newQuotaTracker; tests shrink them to stay fast.
	retryAttempts int
	retryBackoff  time.Duration
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
	// healthSnapshot, if set, returns the frozen health state (rate-limit /
	// circuit cooldowns, model lockouts, learned param blocklist) for
	// persistence — restored on boot so long cooldowns (quota-exhausted, daily)
	// survive a restart. Same healthMu-only lock discipline as stickySnapshot.
	healthSnapshot func() map[string]persistedHealth
	// LoadedHealth is populated by load() on boot; NewProxy applies it (future-
	// dated entries only) to p.health / p.modelLocks / p.paramBlock.
	LoadedHealth map[string]persistedHealth
	// LoadedHealthFP is the config fingerprint the loaded health was frozen
	// under; NewProxy restores ONLY when it matches the current config's
	// fingerprint (see healthConfigFingerprint).
	LoadedHealthFP string
}

// persistedHealth is the on-disk form of one provider's frozen runtime state.
type persistedHealth struct {
	RateLimitedUntil time.Time            `json:"rate_limited_until,omitempty"`
	RateLimitKind    string               `json:"rate_limit_kind,omitempty"`
	CircuitOpenUntil time.Time            `json:"circuit_open_until,omitempty"`
	ModelLocks       map[string]time.Time `json:"model_locks,omitempty"` // model → lockedUntil
	ParamBlock       map[string][]string  `json:"param_block,omitempty"` // model → learned unsupported top-level params
}

// refreshState tracks per-provider refresh dedup state (guarded by quotaTracker.mu).
type refreshState struct {
	last     time.Time
	inFlight bool
}

func newQuotaTracker(path string, cfg func() *Config, provs func() map[string]provider.Provider) *quotaTracker {
	return &quotaTracker{
		state:         map[string]*provider.QuotaSnapshot{},
		refreshGuard:  map[string]*refreshState{},
		path:          path,
		cfg:           cfg,
		provs:         provs,
		stopCh:        make(chan struct{}),
		retryAttempts: 3,
		retryBackoff:  time.Second,
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
			t.setSnapshot(n, t.fetchQuota(p, now)) // fetchQuota retries transient errors
		}(name, provImpl)
	}
	wg.Wait()
	if err := t.persist(); err != nil {
		log.Printf("[quota] persist after pollAll failed: %v", err)
	}
}

// pollOne re-polls a single provider by its quota key (a config name or a
// pooled-account virtual id "name#<accountID>") and persists. Used by the Web
// UI's per-account "Refresh usage" - unlike refreshOne it is NOT debounced
// (a manual click should always re-poll) and runs synchronously so the caller
// sees the fresh snapshot. Returns false if the key isn't a live provider.
func (t *quotaTracker) pollOne(key string) bool {
	p := t.provs()[key]
	if p == nil {
		return false
	}
	t.setSnapshot(key, t.fetchQuota(p, time.Now()))
	if err := t.persist(); err != nil {
		log.Printf("[quota] persist after pollOne(%s) failed: %v", key, err)
	}
	return true
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
		s := t.fetchQuota(p, time.Now())
		t.setSnapshot(name, s)
		if err := t.persist(); err != nil {
			log.Printf("[quota] persist after refreshOne(%s) failed: %v", name, err)
		}
		refreshed = true
	}

	t.mu.Lock()
	g.inFlight = false
	if refreshed {
		g.last = time.Now()
	}
	t.mu.Unlock()
}

// fetchQuota polls a provider's Quota(), retrying transient errors (DNS "no
// such host", connection refused, timeout, 5xx) a few times with backoff so a
// brief network blip doesn't fail the whole poll cycle and leave the UI stuck
// on the error until the next interval. Non-transient errors (auth, retcode,
// 4xx, missing credentials) return immediately - retrying those just wastes
// time. Returns the final snapshot (Err set if all attempts failed); never nil.
func (t *quotaTracker) fetchQuota(p provider.Provider, now time.Time) *provider.QuotaSnapshot {
	var s *provider.QuotaSnapshot
	for attempt := 0; attempt < t.retryAttempts; attempt++ {
		s, _ = p.Quota()
		if s == nil {
			s = &provider.QuotaSnapshot{Billing: provider.BillingUnknown, AsOf: now}
		}
		if s.Err == "" || !isTransientQuotaErr(s.Err) {
			break // success, or a non-transient error - don't retry
		}
		if attempt < t.retryAttempts-1 {
			// backoff: b, 2b, 4b ... (1s, 2s by default). Respects stop so a
			// shutting-down daemon isn't held by a retry sleep.
			select {
			case <-time.After(t.retryBackoff << uint(attempt)):
			case <-t.stopCh:
			}
		}
	}
	s.AsOf = now
	return s
}

// isTransientQuotaErr reports whether a quota-fetch error is worth retrying.
// Network blips often clear within seconds; auth/config/rejection errors won't,
// so retrying those just burns time. Unrecognized errors default to transient so
// a new failure shape still gets retried (and recovers) rather than sticking for
// a whole poll interval.
func isTransientQuotaErr(err string) bool {
	e := strings.ToLower(err)
	// Permanent: auth, config, or upstream-rejected - retry won't help.
	for _, m := range []string{
		"not logged in", "no project_id", "session expired", "retcode=",
		"ak/sk not configured", "api key provisioning", "unknown provider",
		"http 400", "http 401", "http 403", "http 404", "http 422",
		"not zhipu quota format",
	} {
		if strings.Contains(e, m) {
			return false
		}
	}
	return true
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

// persist writes the quota/sticky/health snapshot atomically (tmp + rename).
// Returns the write error so synchronous callers (unfreeze API) can fail the
// operation instead of reporting a false success; background callers log it.
func (t *quotaTracker) persist() error {
	// Serialize the whole write (snapshot → tmp → rename): concurrent persists
	// from pollAll/pollOne/refreshOne share the fixed .tmp sibling.
	t.persistMu.Lock()
	defer t.persistMu.Unlock()
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
	if t.healthSnapshot != nil {
		// Same lock discipline as stickySnapshot: healthMu only, never nested
		// inside quotaMu. The fingerprint gates restore to the exact config the
		// state was frozen under (health keys are provider names — without the
		// gate, a different config's daemon/test reading this file would
		// "restore" cooldowns onto unrelated same-named providers).
		wrap["health"] = t.healthSnapshot()
		wrap["health_fp"] = healthConfigFingerprint(t.cfg())
	}
	data, err := json.MarshalIndent(wrap, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(t.path), 0o700); err != nil {
		return err
	}
	tmp := t.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, t.path); err != nil {
		log.Printf("[quota] persist rename failed: %v", err)
		return err
	}
	return nil
}

func (t *quotaTracker) load() {
	data, err := os.ReadFile(t.path)
	if err != nil {
		return
	}
	var wrap struct {
		Providers map[string]persistedSnapshot `json:"providers"`
		Sticky    map[string]persistedSticky   `json:"sticky"`
		Health    map[string]persistedHealth   `json:"health"`
		HealthFP  string                       `json:"health_fp"`
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
	t.LoadedHealth = wrap.Health
	t.LoadedHealthFP = wrap.HealthFP
}
