package runtime

import (
	"encoding/json"
	"model-proxy/internal/observe/logx"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/provider"
	runtimewire "model-proxy/internal/runtime/wirecap"
)

// QuotaTracker polls providers' Quota() periodically, caches the results in
// memory + a file (~/.model-proxy/quota_state.json), and serves them to the
// scheduler. Quota values live in the shared runtime Manager; the tracker mutex
// only deduplicates refresh work.
type QuotaTracker struct {
	mu        sync.Mutex // guards RefreshGuard only; quota values live in runtime
	runtime   *Manager
	Path      string
	cfg       func() *configdomain.Config
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
	Generation func() uint64
	// frozenMaxAge (nanoseconds) is the snapshot-freshness window captured at
	// Start; atomic because request goroutines read it after serving begins.
	frozenMaxAge atomic.Int64
	// persistMu serializes persist() WITHIN one tracker. Cross-tracker
	// contention (parallel test proxies, or the daemon vs a test) is handled by
	// the unique temp file in persist() — the fixed ".tmp" name used to make a
	// concurrent writer's rename fail ENOENT.
	persistMu sync.Mutex
	// retryAttemptsValue/retryBackoffValue tune fetchQuota's transient-error
	// retry; reads go through the locked accessors below.
	// Defaults (3 / 1s) are set in newQuotaTracker; tests shrink them to stay fast.
	retryAttemptsValue int
	retryBackoffValue  time.Duration
	// RefreshGuard dedupes 429-triggered refreshes per provider (inFlight
	// coalesces concurrent ones; last debounces ones that just ran), so a 429
	// storm doesn't fire N upstream Quota() calls + N persists. Guarded by mu.
	RefreshGuard map[string]*RefreshState
	// loadedSticky is populated by load() on boot; NewProxy restores it through
	// the runtime Manager.
	LoadedSticky map[string]Sticky
	// fullSnapshot is installed by Proxy and atomically snapshots config
	// fingerprint + health/sticky + quota under the repository lock order.
	FullSnapshot func() PersistedFullSnapshot
	// loadedHealth is populated by load() on boot; NewProxy applies it (future-
	// dated entries only) through the runtime Manager.
	LoadedHealth map[string]PersistedHealth
	// loadedHealthFP is the config fingerprint the loaded health was frozen
	// under; NewProxy restores ONLY when it matches the current config's
	// fingerprint (see healthConfigFingerprint).
	LoadedHealthFP string
	// loadedWireCaps is populated by load() on boot; NewProxy restores the
	// verdicts whose base_url still matches the current config (wirecap.go).
	LoadedWireCaps map[string]runtimewire.Capabilities
}

type PersistedFullSnapshot struct {
	Providers  map[string]PersistedQuotaSnapshot
	Sticky     map[string]Sticky
	Health     map[string]PersistedHealth
	HealthFP   string
	Generation uint64
	WireCaps   map[string]runtimewire.Capabilities
}

// RefreshState tracks per-provider refresh dedup state (guarded by QuotaTracker.mu).
type RefreshState struct {
	last     time.Time
	inFlight bool
}

func NewQuotaTracker(
	path string,
	cfg func() *configdomain.Config,
	provs func() map[string]provider.Provider,
	runtimeManager *Manager,
) *QuotaTracker {
	if runtimeManager == nil {
		panic("quota tracker requires a runtime Manager")
	}
	return &QuotaTracker{
		runtime:            runtimeManager,
		RefreshGuard:       map[string]*RefreshState{},
		Path:               path,
		cfg:                cfg,
		provs:              provs,
		stopCh:             make(chan struct{}),
		accepting:          true,
		retryAttemptsValue: 3,
		retryBackoffValue:  time.Second,
	}
}

func (t *QuotaTracker) CurrentGeneration() uint64 {
	if t.Generation == nil {
		return 0
	}
	return t.Generation()
}

// launch admits a background task and increments the WaitGroup under the same
// lifecycle mutex used by stop. This makes accepting+Add atomic with the
// accepting=false transition, so Add can never race a zero-counter Wait.
func (t *QuotaTracker) Launch(fn func()) bool {
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

// FreshnessMaxAge returns the quota-snapshot freshness window: frozen at
// Start to the poll cadence the ticker captured, so a hot
// quota_poll_interval change cannot split the window from the actual polling
// (pitfalls #29 — the change needs a restart to affect either side). Before
// Start (or with an unwired config) it falls back to the live 3× poll
// interval / package default.
func (t *QuotaTracker) FreshnessMaxAge() time.Duration {
	if nanos := t.frozenMaxAge.Load(); nanos > 0 {
		return time.Duration(nanos)
	}
	if t.cfg == nil || t.cfg() == nil {
		return provider.DefaultEtaMaxGap
	}
	return 3 * t.cfg().Scheduling.PollInterval()
}

func (t *QuotaTracker) Start() {
	t.Load() // baseline before first poll
	// Freeze the freshness window to the SAME cadence the ticker uses:
	// scheduling decisions and failure classification (quotaExhausted*)
	// must not judge snapshots by a hot-changed window while polling
	// keeps the Start-time cadence (pitfalls #29 — restart to change).
	// Stored synchronously so the frozen value is visible the moment Start
	// returns, matching the ticker that launches below.
	interval := t.cfg().Scheduling.PollInterval()
	t.frozenMaxAge.Store((3 * interval).Nanoseconds())
	t.Launch(func() {
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
				t.PollAll(time.Now())
			case <-ticker.C:
				t.PollAll(time.Now())
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
func (t *QuotaTracker) Stop() {
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

func (t *QuotaTracker) PollAfter(d time.Duration) {
	t.Launch(func() {
		select {
		case <-time.After(d):
			t.PollAll(time.Now())
		case <-t.stopCh:
		}
	})
}

// stopped reports whether stop has been signaled (non-blocking). Used by the
// async dispatchers to no-op a poll dispatched after Close.
func (t *QuotaTracker) Stopped() bool {
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
func (t *QuotaTracker) PollAsync(now time.Time) {
	gen := t.CurrentGeneration()
	t.Launch(func() {
		if t.Stopped() {
			return
		}
		t.PollAllGeneration(now, gen)
	})
}

// refreshAsync dispatches one refreshOne on a tracked, stop-aware goroutine —
// the 429 path. Same lifecycle as pollAsync; refreshOne keeps its own dedup.
func (t *QuotaTracker) RefreshAsync(name string, generations ...uint64) {
	gen := t.CurrentGeneration()
	if len(generations) > 0 {
		gen = generations[0]
	}
	t.Launch(func() {
		if t.Stopped() {
			return
		}
		t.RefreshOne(name, gen)
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
func (t *QuotaTracker) PollAll(now time.Time) {
	t.PollAllGeneration(now, t.CurrentGeneration())
}

func (t *QuotaTracker) PollAllGeneration(now time.Time, generation uint64) {
	provs := t.provs()
	var wg sync.WaitGroup
	results := make(map[string]*provider.QuotaSnapshot, len(provs))
	var resultsMu sync.Mutex
	for name, provImpl := range provs {
		wg.Add(1)
		go func(n string, p provider.Provider) {
			defer wg.Done()
			s := t.FetchQuota(p, now) // fetchQuota retries transient errors
			resultsMu.Lock()
			results[n] = s
			resultsMu.Unlock()
		}(name, provImpl)
	}
	wg.Wait()
	// Attach the exhaustion prediction (display-only) before committing: the
	// burn rate needs the PREVIOUS committed snapshot as baseline. Computed
	// here — the tracker owns quota polling — never inside the Manager lock.
	maxGap := t.maxEtaGap()
	for n, s := range results {
		if s != nil {
			s.ExhaustionEta = provider.EstimateExhaustionEta(t.runtime.Quota(n), s, maxGap)
		}
	}
	if !t.runtime.MergeQuotas(results, generation) {
		return // reload happened while the upstream polls were in flight
	}
	if err := t.Persist(); err != nil {
		logx.Warnf("[quota] persist after pollAll failed: %v", err)
	}
}

// clearForGeneration drops quota snapshots owned by the previous config. The
// The caller changes the runtime Manager generation first; stale clears are
// rejected by the same generation gate as poll commits.
func (t *QuotaTracker) ClearForGeneration(generation uint64) {
	t.runtime.ClearQuotas(generation)
}

// pollOne re-polls a single provider by its quota key (a config name or a
// pooled-account virtual id "name#<accountID>") and persists. Used by the Web
// UI's per-account "Refresh usage" - unlike refreshOne it is NOT debounced
// (a manual click should always re-poll) and runs synchronously so the caller
// sees the fresh snapshot. Returns false if the key isn't a live provider.
func (t *QuotaTracker) PollOne(key string) bool {
	generation := t.CurrentGeneration()
	p := t.provs()[key]
	if p == nil {
		return false
	}
	if !t.CommitSnapshot(generation, key, t.FetchQuota(p, time.Now())) {
		return false
	}
	if err := t.Persist(); err != nil {
		logx.Warnf("[quota] persist after pollOne(%s) failed: %v", key, err)
	}
	return true
}

// refreshOne re-polls a single provider after a 429. The call is deduped: a
// concurrent refresh (inFlight) or one that ran less than
// pollInterval/2 ago (last) is dropped, so a 429 storm doesn't fire N upstream
// Quota() calls + N persists for the same provider.
func (t *QuotaTracker) RefreshOne(name string, generations ...uint64) {
	generation := t.CurrentGeneration()
	if len(generations) > 0 {
		generation = generations[0]
	}
	if t.CurrentGeneration() != generation {
		return
	}
	now := time.Now()
	half := t.cfg().Scheduling.PollInterval() / 2
	t.mu.Lock()
	g := t.RefreshGuard[name]
	if g == nil {
		g = &RefreshState{}
		t.RefreshGuard[name] = g
	}
	if g.inFlight || now.Sub(g.last) < half {
		t.mu.Unlock()
		return // coalesced/debounced — a covering refresh already ran or is running
	}
	g.inFlight = true
	t.mu.Unlock()

	refreshed := false
	if p := t.provs()[name]; p != nil {
		s := t.FetchQuota(p, time.Now())
		if t.CommitSnapshot(generation, name, s) {
			if err := t.Persist(); err != nil {
				logx.Warnf("[quota] persist after refreshOne(%s) failed: %v", name, err)
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

// Snapshot reads one quota snapshot (nil when absent). Test seam mirroring
// the removed root helper; production reads flow through Manager.Quota.
// Runtime returns the Manager this tracker writes quota snapshots into.
// Tests swap it to isolate state between scenarios.
// AdmissionOpen reports whether the tracker still admits background tasks
// (false after Stop begins). Test seam mirroring the removed root helper.
// SetRetryBackoff overrides the transient-error retry delay (tests shrink it
// to keep polling fast). Production keeps the constructor default.
func (t *QuotaTracker) SetRetryBackoff(d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.retryBackoffValue = d
}

// StopChannel returns the channel closed by Stop (read-only lifecycle probe
// for tests; production never selects on it directly).
func (t *QuotaTracker) StopChannel() <-chan struct{} { return t.stopCh }

func (t *QuotaTracker) AdmissionOpen() bool {
	t.lifeMu.Lock()
	defer t.lifeMu.Unlock()
	return t.accepting
}

func (t *QuotaTracker) Runtime() *Manager { return t.runtime }

func (t *QuotaTracker) Snapshot(name string) *provider.QuotaSnapshot {
	return t.runtime.Quota(name)
}

// SetSnapshot writes a quota snapshot for the tracker's current generation.
// Test seam mirroring the removed root helper: production commits flow through
// CommitSnapshot/FetchQuota.
func (t *QuotaTracker) SetSnapshot(name string, snapshot *provider.QuotaSnapshot) {
	t.runtime.SetQuota(name, snapshot, t.CurrentGeneration())
}

func (t *QuotaTracker) CommitSnapshot(generation uint64, name string, snapshot *provider.QuotaSnapshot) bool {
	if t.CurrentGeneration() != generation {
		return false
	}
	if snapshot != nil {
		snapshot.ExhaustionEta = provider.EstimateExhaustionEta(t.runtime.Quota(name), snapshot, t.maxEtaGap())
	}
	return t.runtime.SetQuota(name, snapshot, generation)
}

// maxEtaGap is the snapshot gap beyond which the burn-rate baseline is
// considered stale (a poll gap): 3× the configured poll interval — the same
// bound after which a snapshot degrades to BillingUnknown. Falls back to the
// provider package default when no config is wired (standalone/test trackers).
func (t *QuotaTracker) maxEtaGap() time.Duration {
	if t.cfg == nil || t.cfg() == nil {
		return provider.DefaultEtaMaxGap
	}
	return 3 * t.cfg().Scheduling.PollInterval()
}

// fetchQuota polls a provider's Quota(), retrying transient errors (DNS "no
// such host", connection refused, timeout, 5xx) a few times with backoff so a
// brief network blip doesn't fail the whole poll cycle and leave the UI stuck
// on the error until the next interval. Non-transient errors (auth, retcode,
// 4xx, missing credentials) return immediately - retrying those just wastes
// time. Returns the final snapshot (Err set if all attempts failed); never nil.
func (t *QuotaTracker) FetchQuota(p provider.Provider, now time.Time) *provider.QuotaSnapshot {
	var s *provider.QuotaSnapshot
	for attempt := 0; attempt < t.retryAttempts(); attempt++ {
		s, _ = p.Quota()
		if s == nil {
			s = &provider.QuotaSnapshot{Billing: provider.BillingUnknown, AsOf: now}
		}
		if s.Err == "" || !IsTransientQuotaErr(s.Err) {
			break // success, or a non-transient error - don't retry
		}
		if attempt < t.retryAttempts()-1 {
			// backoff: b, 2b, 4b ... (1s, 2s by default). Respects stop so a
			// shutting-down daemon isn't held by a retry sleep — and stops the
			// remaining attempts too: shutdown must not fire more upstream
			// calls (the stop wake-up is the last thing we do).
			select {
			case <-time.After(t.retryBackoff() << uint(attempt)):
			case <-t.stopCh:
				s.AsOf = now
				return s
			}
		}
	}
	s.AsOf = now
	return s
}

// retryAttempts/retryBackoff are read on the fetch path and written by tests
// via SetRetryBackoff under t.mu; route reads through the same lock so a
// concurrent adjustment can't race.
func (t *QuotaTracker) retryAttempts() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.retryAttemptsValue
}

func (t *QuotaTracker) retryBackoff() time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.retryBackoffValue
}

// isTransientQuotaErr reports whether a quota-fetch error is worth retrying.
// Network blips often clear within seconds; auth/config/rejection errors won't,
// so retrying those just burns time. Unrecognized errors default to transient so
// a new failure shape still gets retried (and recovers) rather than sticking for
// a whole poll interval.
func IsTransientQuotaErr(err string) bool {
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

type PersistedQuotaSnapshot struct {
	Billing      provider.BillingClass  `json:"billing"`
	RemainingPct float64                `json:"remaining_pct"`
	Windows      []provider.QuotaWindow `json:"windows"`
	AsOf         time.Time              `json:"as_of"`
	Err          string                 `json:"err,omitempty"`
}

// persist writes the quota/sticky/health snapshot atomically (tmp + rename).
// Returns the write error so synchronous callers (unfreeze/freeze APIs) can
// fail the operation instead of reporting a false success; background callers
// log it.
func (t *QuotaTracker) Persist() error {
	if t.Path == "" {
		return nil // in-memory tracker (direct-construct tests) has no file
	}
	// Serialize the whole write (snapshot → tmp → rename) WITHIN this tracker.
	// Cross-tracker serialization is not needed: each write gets a UNIQUE temp
	// file, so concurrent writers never contend on a shared ".tmp" (the old
	// fixed name made a loser's rename fail ENOENT).
	t.persistMu.Lock()
	defer t.persistMu.Unlock()
	wrap := map[string]any{}
	if t.FullSnapshot != nil {
		s := t.FullSnapshot()
		wrap["providers"] = s.Providers
		wrap["sticky"] = s.Sticky
		wrap["health"] = s.Health
		wrap["health_fp"] = s.HealthFP
		if len(s.WireCaps) > 0 {
			wrap["wire_caps"] = s.WireCaps
		}
	} else {
		snapshots := t.runtime.Quotas()
		out := make(map[string]PersistedQuotaSnapshot, len(snapshots))
		for k, v := range snapshots {
			out[k] = PersistedQuotaSnapshot{
				Billing: v.Billing, RemainingPct: v.RemainingPct,
				Windows: v.Windows, AsOf: v.AsOf, Err: v.Err,
			}
		}
		wrap["providers"] = out
	}
	data, err := json.MarshalIndent(wrap, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(t.Path)
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
	// fsync before rename: a crash+reboot must not leave the rename durable
	// while the data isn't (empty state file).
	if err := f.Sync(); err != nil {
		f.Close()
		remove()
		return err
	}
	if err := f.Close(); err != nil {
		remove()
		return err
	}
	if err := os.Rename(tmp, t.Path); err != nil {
		logx.Warnf("[quota] persist rename failed: %v", err)
		remove()
		return err
	}
	return nil
}

func (t *QuotaTracker) Load() {
	data, err := os.ReadFile(t.Path)
	if err != nil {
		return
	}
	var wrap struct {
		Providers map[string]PersistedQuotaSnapshot   `json:"providers"`
		Sticky    map[string]Sticky                   `json:"sticky"`
		Health    map[string]PersistedHealth          `json:"health"`
		HealthFP  string                              `json:"health_fp"`
		WireCaps  map[string]runtimewire.Capabilities `json:"wire_caps"`
	}
	if err := json.Unmarshal(data, &wrap); err != nil {
		return
	}
	quota := make(map[string]*provider.QuotaSnapshot, len(wrap.Providers))
	// Filter to the CURRENT provider set: quota keys include virtual ids from
	// pools, so keys from removed accounts/providers would otherwise merge
	// into the generation and be re-persisted forever (reviving on every
	// restart). Scheduling reads are lazy, but the file never shrinks.
	current := t.provs()
	for k, v := range wrap.Providers {
		if _, active := current[k]; !active {
			continue
		}
		quota[k] = &provider.QuotaSnapshot{
			Billing: v.Billing, RemainingPct: v.RemainingPct,
			Windows: v.Windows, AsOf: v.AsOf, Err: v.Err,
		}
	}
	t.runtime.MergeQuotas(quota, t.CurrentGeneration())
	t.mu.Lock()
	defer t.mu.Unlock()
	t.LoadedSticky = wrap.Sticky
	t.LoadedHealth = wrap.Health
	t.LoadedHealthFP = wrap.HealthFP
	t.LoadedWireCaps = wrap.WireCaps
}
