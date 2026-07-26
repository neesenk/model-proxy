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
	mu        sync.RWMutex
	state     map[string]*provider.QuotaSnapshot
	path      string
	cfg       func() *Config
	provs     func() map[string]provider.Provider
	stopCh    chan struct{}
	stopOnce  sync.Once
	lifeMu    sync.Mutex
	accepting bool
	// poller tracks the background poll goroutine(s) so stop can WAIT for them
	// to drain (Proxy.Close / tests) rather than leaving a poll that fires a
	// persist after the owner has torn down or moved to a new config generation.
	poller sync.WaitGroup
	// generation identifies the Proxy config generation that owns provider
	// snapshots. A nil callback means a standalone/test tracker with generation 0.
	generation      func() uint64
	stateGeneration uint64
	// persistMu serializes persist() WITHIN one tracker. Cross-tracker
	// contention (parallel test proxies, or the daemon vs a test) is handled by
	// the unique temp file in persist() — the fixed ".tmp" name used to make a
	// concurrent writer's rename fail ENOENT.
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
	// fullSnapshot is installed by Proxy and atomically snapshots config
	// fingerprint + health/sticky + quota under the repository lock order. It
	// supersedes the legacy stickySnapshot/healthSnapshot callbacks above, which
	// remain for focused quotaTracker unit tests.
	fullSnapshot func() persistedFullSnapshot
	// LoadedHealth is populated by load() on boot; NewProxy applies it (future-
	// dated entries only) to p.health / p.modelLocks / p.paramBlock.
	LoadedHealth map[string]persistedHealth
	// LoadedHealthFP is the config fingerprint the loaded health was frozen
	// under; NewProxy restores ONLY when it matches the current config's
	// fingerprint (see healthConfigFingerprint).
	LoadedHealthFP string
	// LoadedWireCaps is populated by load() on boot; NewProxy restores the
	// verdicts whose base_url still matches the current config (wirecap.go).
	LoadedWireCaps map[string]wireCaps
}

// persistedHealth is the on-disk form of one provider's frozen runtime state.
type persistedHealth struct {
	RateLimitedUntil time.Time            `json:"rate_limited_until,omitempty"`
	RateLimitKind    string               `json:"rate_limit_kind,omitempty"`
	CircuitOpenUntil time.Time            `json:"circuit_open_until,omitempty"`
	ModelLocks       map[string]time.Time `json:"model_locks,omitempty"` // model → lockedUntil
	ParamBlock       map[string][]string  `json:"param_block,omitempty"` // model → learned unsupported top-level params
}

type persistedFullSnapshot struct {
	Providers  map[string]persistedSnapshot
	Sticky     map[string]routeSticky
	Health     map[string]persistedHealth
	HealthFP   string
	Generation uint64
	WireCaps   map[string]wireCaps
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
		accepting:     true,
		retryAttempts: 3,
		retryBackoff:  time.Second,
	}
}

func (t *quotaTracker) currentGeneration() uint64 {
	if t.generation == nil {
		return 0
	}
	return t.generation()
}

// launch admits a background task and increments the WaitGroup under the same
// lifecycle mutex used by stop. This makes accepting+Add atomic with the
// accepting=false transition, so Add can never race a zero-counter Wait.
func (t *quotaTracker) launch(fn func()) bool {
	t.lifeMu.Lock()
	if !t.accepting {
		t.lifeMu.Unlock()
		return false
	}
	t.poller.Add(1)
	t.lifeMu.Unlock()
	go func() {
		defer t.poller.Done()
		fn()
	}()
	return true
}

func (t *quotaTracker) start() {
	t.load() // baseline before first poll
	t.launch(func() {
		interval := t.cfg().Scheduling.pollInterval()
		// bootstrap poll shortly after start, as a one-shot timer in the same
		// goroutine — keeps the lifecycle to a single tracked goroutine (the
		// bootstrap used to spawn a second, untracked one via pollAfter).
		bootstrap := time.NewTimer(10 * time.Second)
		defer bootstrap.Stop()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-bootstrap.C:
				t.pollAll(time.Now())
			case <-ticker.C:
				t.pollAll(time.Now())
			case <-t.stopCh:
				return
			}
		}
	})
}

// stop signals the poller goroutine(s) to exit and waits for them to drain, so
// the owner (Proxy.Close / tests) releases the tracker deterministically —
// without the wait a lingering poll could fire a persist after the owner has
// torn down or moved to a new config generation. Idempotent via stopOnce.
func (t *quotaTracker) stop() {
	t.stopOnce.Do(func() {
		t.lifeMu.Lock()
		t.accepting = false
		if t.stopCh != nil {
			close(t.stopCh)
		}
		t.lifeMu.Unlock()
		t.poller.Wait()
	})
}

func (t *quotaTracker) pollAfter(d time.Duration) {
	t.launch(func() {
		select {
		case <-time.After(d):
			t.pollAll(time.Now())
		case <-t.stopCh:
		}
	})
}

// stopped reports whether stop has been signaled (non-blocking). Used by the
// async dispatchers to no-op a poll dispatched after Close.
func (t *quotaTracker) stopped() bool {
	select {
	case <-t.stopCh:
		return true
	default:
		return false
	}
}

// pollAsync dispatches one pollAll on a tracked, stop-aware goroutine — the path
// reload uses to "poll now". Tracked by poller so Proxy.Close waits for it, and
// stop-aware so a dispatch after Close no-ops instead of firing a persist after
// the final flush (the bug: reload's bare `go pollAll` bypassed the WaitGroup).
func (t *quotaTracker) pollAsync(now time.Time) {
	gen := t.currentGeneration()
	t.launch(func() {
		if t.stopped() {
			return
		}
		t.pollAllGeneration(now, gen)
	})
}

// refreshAsync dispatches one refreshOne on a tracked, stop-aware goroutine —
// the 429 path. Same lifecycle as pollAsync; refreshOne keeps its own dedup.
func (t *quotaTracker) refreshAsync(name string, generations ...uint64) {
	gen := t.currentGeneration()
	if len(generations) > 0 {
		gen = generations[0]
	}
	t.launch(func() {
		if t.stopped() {
			return
		}
		t.refreshOne(name, gen)
	})
}

// pollAll polls every runnable provider instance in parallel (bounded by the
// runtime's goroutine scheduling; provider count is small) and persists once at
// the end. It iterates the RUNTIME provider map — which holds the unrolled
// "name#<accountID>" virtuals for multi-account pools plus the plain names of
// single-account providers — NOT cfg.Providers: a pooled parent name is not a
// runtime key (buildProviders unrolls it into virtuals), so iterating
// cfg.Providers and looking the parent up by name found nil and skipped the
// whole pool every cycle. Each virtual carries its own bound credentials
// (buildOne binding point #1), so fetchQuota(p) queries the correct account.
func (t *quotaTracker) pollAll(now time.Time) {
	t.pollAllGeneration(now, t.currentGeneration())
}

func (t *quotaTracker) pollAllGeneration(now time.Time, generation uint64) {
	provs := t.provs()
	var wg sync.WaitGroup
	results := make(map[string]*provider.QuotaSnapshot, len(provs))
	var resultsMu sync.Mutex
	for name, provImpl := range provs {
		wg.Add(1)
		go func(n string, p provider.Provider) {
			defer wg.Done()
			s := t.fetchQuota(p, now) // fetchQuota retries transient errors
			resultsMu.Lock()
			results[n] = s
			resultsMu.Unlock()
		}(name, provImpl)
	}
	wg.Wait()
	t.mu.Lock()
	if t.currentGeneration() != generation {
		t.mu.Unlock()
		return // reload happened while the upstream polls were in flight
	}
	for name, snapshot := range results {
		t.state[name] = snapshot
	}
	t.stateGeneration = generation
	t.mu.Unlock()
	if err := t.persist(); err != nil {
		log.Printf("[quota] persist after pollAll failed: %v", err)
	}
}

// clearForGeneration drops quota snapshots owned by the previous config. The
// caller changes Proxy generation first, then calls this while holding the
// Proxy's config+health locks (lock order: p.mu -> healthMu -> quotaMu).
func (t *quotaTracker) clearForGeneration(generation uint64) {
	t.mu.Lock()
	t.state = map[string]*provider.QuotaSnapshot{}
	t.stateGeneration = generation
	t.mu.Unlock()
}

// pollOne re-polls a single provider by its quota key (a config name or a
// pooled-account virtual id "name#<accountID>") and persists. Used by the Web
// UI's per-account "Refresh usage" - unlike refreshOne it is NOT debounced
// (a manual click should always re-poll) and runs synchronously so the caller
// sees the fresh snapshot. Returns false if the key isn't a live provider.
func (t *quotaTracker) pollOne(key string) bool {
	generation := t.currentGeneration()
	p := t.provs()[key]
	if p == nil {
		return false
	}
	if !t.commitSnapshot(generation, key, t.fetchQuota(p, time.Now())) {
		return false
	}
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
func (t *quotaTracker) refreshOne(name string, generations ...uint64) {
	generation := t.currentGeneration()
	if len(generations) > 0 {
		generation = generations[0]
	}
	if t.currentGeneration() != generation {
		return
	}
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
		if t.commitSnapshot(generation, name, s) {
			if err := t.persist(); err != nil {
				log.Printf("[quota] persist after refreshOne(%s) failed: %v", name, err)
			}
			refreshed = true
		}
	}

	t.mu.Lock()
	g.inFlight = false
	if refreshed {
		g.last = time.Now()
	}
	t.mu.Unlock()
}

func (t *quotaTracker) commitSnapshot(generation uint64, name string, snapshot *provider.QuotaSnapshot) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.currentGeneration() != generation {
		return false
	}
	t.state[name] = snapshot
	t.stateGeneration = generation
	return true
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
	if t.path == "" {
		return nil // in-memory tracker (direct-construct tests) has no file
	}
	// Serialize the whole write (snapshot → tmp → rename) WITHIN this tracker.
	// Cross-tracker serialization is not needed: each write gets a UNIQUE temp
	// file, so concurrent writers never contend on a shared ".tmp" (the old
	// fixed name made a loser's rename fail ENOENT).
	t.persistMu.Lock()
	defer t.persistMu.Unlock()
	wrap := map[string]any{}
	if t.fullSnapshot != nil {
		s := t.fullSnapshot()
		wrap["providers"] = s.Providers
		sticky := make(map[string]persistedSticky, len(s.Sticky))
		for k, v := range s.Sticky {
			sticky[k] = persistedSticky{Provider: v.provider, Since: v.since}
		}
		wrap["sticky"] = sticky
		wrap["health"] = s.Health
		wrap["health_fp"] = s.HealthFP
		if len(s.WireCaps) > 0 {
			wrap["wire_caps"] = s.WireCaps
		}
	} else {
		t.mu.RLock()
		out := make(map[string]persistedSnapshot, len(t.state))
		for k, v := range t.state {
			out[k] = persistedSnapshot{
				Billing: v.Billing, RemainingPct: v.RemainingPct,
				Windows: v.Windows, AsOf: v.AsOf, Err: v.Err,
			}
		}
		t.mu.RUnlock()
		wrap["providers"] = out
	}
	if t.fullSnapshot == nil && t.stickySnapshot != nil {
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
	if t.fullSnapshot == nil && t.healthSnapshot != nil {
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
	dir := filepath.Dir(t.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// Unique temp file PER WRITE (same dir, so the rename is atomic on every
	// platform): two trackers — or a tracker vs a synchronous caller — sharing
	// one state file no longer race on a fixed ".tmp" name (loser's rename used
	// to fail ENOENT, and interleaved writes could corrupt the file). Rename is
	// atomic → last writer wins, the file is never half-written.
	f, err := os.CreateTemp(dir, ".quota_state-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	remove := func() { os.Remove(tmp) }
	if _, err := f.Write(data); err != nil {
		f.Close()
		remove()
		return err
	}
	if err := f.Close(); err != nil {
		remove()
		return err
	}
	if err := os.Rename(tmp, t.path); err != nil {
		log.Printf("[quota] persist rename failed: %v", err)
		remove()
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
		WireCaps  map[string]wireCaps          `json:"wire_caps"`
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
	t.stateGeneration = t.currentGeneration()
	t.LoadedSticky = make(map[string]routeSticky, len(wrap.Sticky))
	for k, v := range wrap.Sticky {
		t.LoadedSticky[k] = routeSticky{provider: v.Provider, since: v.Since}
	}
	t.LoadedHealth = wrap.Health
	t.LoadedHealthFP = wrap.HealthFP
	t.LoadedWireCaps = wrap.WireCaps
}
