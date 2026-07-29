// Package runtime owns the mutable, generation-scoped routing state used by
// the proxy. It deliberately depends only on provider value types: config,
// HTTP, persistence, and Web presentation remain composition-root concerns.
package runtime

import (
	"sort"
	"strings"
	"sync"
	"time"

	"model-proxy/provider"
)

// RateLimitKind classifies what an upstream 429 says is exhausted.
type RateLimitKind int

const (
	Transient RateLimitKind = iota
	Quota
	Daily
)

// Descriptive aliases are useful at call sites that already use quota as a
// noun. The short names above are the canonical persisted categories.
const (
	RateLimitTransient = Transient
	RateLimitQuota     = Quota
	RateLimitDaily     = Daily
)

func (k RateLimitKind) String() string {
	switch k {
	case Quota:
		return "quota"
	case Daily:
		return "daily"
	default:
		return "transient"
	}
}

// ParseRateLimitKind parses the persisted category. Unknown and legacy values
// conservatively retain the historical transient behavior.
func ParseRateLimitKind(s string) RateLimitKind {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "quota":
		return Quota
	case "daily":
		return Daily
	default:
		return Transient
	}
}

// Sticky records where a route or session is parked and when it was selected.
type Sticky struct {
	Provider string    `json:"provider"`
	Since    time.Time `json:"since"`
}

// Pin is an in-memory hard route constraint. A zero ExpiresAt never expires.
type Pin struct {
	Provider  string    `json:"provider"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
}

func (p Pin) Active(now time.Time) bool {
	return p.ExpiresAt.IsZero() || now.Before(p.ExpiresAt)
}

func (p Pin) ExpiresLabel(now time.Time) string {
	if p.ExpiresAt.IsZero() {
		return ""
	}
	d := p.ExpiresAt.Sub(now)
	if d <= 0 {
		return "expired"
	}
	return "expires in " + d.Round(time.Second).String()
}

// ModelKey scopes model failures and learned parameter incompatibilities to one
// provider/model pair.
type ModelKey struct {
	Provider string
	Model    string
}

// PersistedHealth is the on-disk representation of one provider's frozen
// runtime state. Its JSON shape intentionally matches the legacy root type.
type PersistedHealth struct {
	RateLimitedUntil time.Time            `json:"rate_limited_until,omitempty"`
	RateLimitKind    string               `json:"rate_limit_kind,omitempty"`
	CircuitOpenUntil time.Time            `json:"circuit_open_until,omitempty"`
	ModelLocks       map[string]time.Time `json:"model_locks,omitempty"`
	ParamBlock       map[string][]string  `json:"param_block,omitempty"`
}

// PersistSnapshot is an atomic, detached view suitable for durable encoding.
type PersistSnapshot struct {
	Generation uint64
	Quotas     map[string]*provider.QuotaSnapshot
	Sticky     map[string]Sticky
	Health     map[string]PersistedHealth
}

// ProviderStatus is the detached dashboard state of one provider.
type ProviderStatus struct {
	ConsecutiveFailures int
	CircuitState        string
	Available           bool
	CircuitOpenUntil    time.Time
	RateLimitedUntil    time.Time
	RateLimitKind       RateLimitKind
	HalfOpenInFlight    bool
}

// ModelLockStatus is the detached dashboard state of one model lock.
type ModelLockStatus struct {
	Provider    string
	Model       string
	Failures    int
	LockedUntil time.Time
}

// DashboardSnapshot is an atomic, detached view for status presentation.
type DashboardSnapshot struct {
	Generation uint64
	Providers  map[string]ProviderStatus
	ModelLocks map[string][]ModelLockStatus
	Sticky     map[string]Sticky
	Pins       map[string]Pin
	Quotas     map[string]*provider.QuotaSnapshot

	capturedAt time.Time
	spread     map[string]uint64
}

// Target contains immutable config-derived scheduling inputs. Quota-derived
// billing and surplus deliberately do not cross the Manager boundary: they are
// projected from the Manager-owned quota snapshot in the same critical section
// that reads health, pin, sticky, model locks, and spread.
type Target struct {
	Provider        string
	Parent          string
	Model           string
	Priority        int
	BillingOverride provider.BillingClass
	PeakMultiplier  float64
}

type ScheduleInput struct {
	Exposed      string
	SessionKey   string
	Targets      []Target
	RouteKeys    map[string]bool
	Dwell        time.Duration
	SwitchMargin float64
	Now          time.Time
	QuotaMaxAge  time.Duration
	Commit       bool
	Generation   uint64
}

// ScheduleFacts is the quota projection used for one input target. Facts stays
// aligned with ScheduleInput.Targets, allowing a dashboard preview to render
// the exact billing/surplus values used by its ordering decision.
type ScheduleFacts struct {
	Billing provider.BillingClass
	Surplus float64
}

type ScheduleResult struct {
	Order          []int
	StickyProvider string
	Facts          []ScheduleFacts
}

type scheduleCandidate struct {
	target  Target
	index   int
	tier    int
	surplus float64
}

type providerHealth struct {
	consecutiveFailures int
	circuitOpenUntil    time.Time
	rateLimitedUntil    time.Time
	rateLimitKind       RateLimitKind
	halfOpenInFlight    bool
}

func (h *providerHealth) available(now time.Time) bool {
	if now.Before(h.rateLimitedUntil) {
		return false
	}
	if !h.circuitOpenUntil.IsZero() && now.Before(h.circuitOpenUntil) {
		return false
	}
	if !h.circuitOpenUntil.IsZero() &&
		!now.Before(h.circuitOpenUntil) &&
		h.halfOpenInFlight {
		return false
	}
	return true
}

type modelLock struct {
	failures    int
	lockedUntil time.Time
}

// Manager owns all mutable routing state behind one mutex. The zero value is
// ready for use; maps are allocated lazily by mutating methods.
type Manager struct {
	mu sync.Mutex

	generation uint64
	health     map[string]*providerHealth
	sticky     map[string]Sticky
	pins       map[string]Pin
	modelLocks map[ModelKey]*modelLock
	paramBlock map[ModelKey]map[string]bool
	spread     map[string]uint64
	quotas     map[string]*provider.QuotaSnapshot
}

func NewManager(generation uint64) *Manager {
	m := &Manager{generation: generation}
	m.ensureLocked()
	return m
}

func (m *Manager) ensureLocked() {
	if m.health == nil {
		m.health = make(map[string]*providerHealth)
	}
	if m.sticky == nil {
		m.sticky = make(map[string]Sticky)
	}
	if m.pins == nil {
		m.pins = make(map[string]Pin)
	}
	if m.modelLocks == nil {
		m.modelLocks = make(map[ModelKey]*modelLock)
	}
	if m.paramBlock == nil {
		m.paramBlock = make(map[ModelKey]map[string]bool)
	}
	if m.spread == nil {
		m.spread = make(map[string]uint64)
	}
	if m.quotas == nil {
		m.quotas = make(map[string]*provider.QuotaSnapshot)
	}
}

func (m *Manager) generationMatchesLocked(generation uint64) bool {
	return generation == 0 || generation == m.generation
}

func (m *Manager) Generation() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.generation
}

// ReplaceGeneration atomically clears state tied to the old config while
// preserving operator pins, which intentionally survive hot reloads.
func (m *Manager) ReplaceGeneration(generation uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	m.generation = generation
	m.health = make(map[string]*providerHealth)
	m.sticky = make(map[string]Sticky)
	m.modelLocks = make(map[ModelKey]*modelLock)
	m.paramBlock = make(map[ModelKey]map[string]bool)
	m.spread = make(map[string]uint64)
	m.quotas = make(map[string]*provider.QuotaSnapshot)
}

// RestoreSticky merges a detached persisted sticky snapshot into the current
// generation. Restore is a boot-time operation and therefore is not generation
// gated.
func (m *Manager) RestoreSticky(sticky map[string]Sticky) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	for key, value := range sticky {
		m.sticky[key] = value
	}
}

// RestoreHealth restores future-dated cooldowns and model locks, plus all
// learned parameter blocks. A restored circuit receives the threshold failure
// count so its next hard failure immediately re-opens it.
func (m *Manager) RestoreHealth(health map[string]PersistedHealth, now time.Time, threshold int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	for name, persisted := range health {
		if now.Before(persisted.RateLimitedUntil) || now.Before(persisted.CircuitOpenUntil) {
			state := m.health[name]
			if state == nil {
				state = &providerHealth{}
				m.health[name] = state
			}
			if now.Before(persisted.RateLimitedUntil) {
				state.rateLimitedUntil = persisted.RateLimitedUntil
				state.rateLimitKind = ParseRateLimitKind(persisted.RateLimitKind)
			}
			if now.Before(persisted.CircuitOpenUntil) {
				state.circuitOpenUntil = persisted.CircuitOpenUntil
				state.consecutiveFailures = threshold
			}
		}
		for model, until := range persisted.ModelLocks {
			if now.Before(until) {
				m.modelLocks[ModelKey{Provider: name, Model: model}] = &modelLock{
					failures:    1,
					lockedUntil: until,
				}
			}
		}
		for model, params := range persisted.ParamBlock {
			key := ModelKey{Provider: name, Model: model}
			blocked := m.paramBlock[key]
			if blocked == nil {
				blocked = make(map[string]bool)
				m.paramBlock[key] = blocked
			}
			for _, param := range params {
				blocked[param] = true
			}
		}
	}
}

// SnapshotForPersist copies generation, quota, route-keyed sticky, health,
// model locks, and parameter blocks while holding the single manager mutex.
func (m *Manager) SnapshotForPersist(routeKeys map[string]bool, now time.Time) PersistSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()

	snapshot := PersistSnapshot{
		Generation: m.generation,
		Quotas:     cloneQuotas(m.quotas),
		Sticky:     make(map[string]Sticky),
		Health:     m.snapshotHealthLocked(now),
	}
	for key, value := range m.sticky {
		if routeKeys[key] {
			snapshot.Sticky[key] = value
		}
	}
	return snapshot
}

func (m *Manager) snapshotHealthLocked(now time.Time) map[string]PersistedHealth {
	out := make(map[string]PersistedHealth)
	names := make(map[string]bool, len(m.health)+len(m.modelLocks)+len(m.paramBlock))
	for name := range m.health {
		names[name] = true
	}
	for key := range m.modelLocks {
		names[key.Provider] = true
	}
	for key := range m.paramBlock {
		names[key.Provider] = true
	}

	for name := range names {
		persisted := PersistedHealth{}
		if state := m.health[name]; state != nil {
			persisted.RateLimitedUntil = state.rateLimitedUntil
			persisted.CircuitOpenUntil = state.circuitOpenUntil
			if now.Before(state.rateLimitedUntil) {
				persisted.RateLimitKind = state.rateLimitKind.String()
			}
		}
		if persisted.RateLimitedUntil.IsZero() && persisted.CircuitOpenUntil.IsZero() {
			if !hasModelState(name, m.modelLocks, m.paramBlock) {
				continue
			}
		}
		out[name] = persisted
	}

	for key, entry := range m.modelLocks {
		persisted := out[key.Provider]
		if persisted.ModelLocks == nil {
			persisted.ModelLocks = make(map[string]time.Time)
		}
		persisted.ModelLocks[key.Model] = entry.lockedUntil
		out[key.Provider] = persisted
	}
	for key, values := range m.paramBlock {
		if len(values) == 0 {
			continue
		}
		persisted := out[key.Provider]
		if persisted.ParamBlock == nil {
			persisted.ParamBlock = make(map[string][]string)
		}
		params := make([]string, 0, len(values))
		for param := range values {
			params = append(params, param)
		}
		sort.Strings(params)
		persisted.ParamBlock[key.Model] = params
		out[key.Provider] = persisted
	}
	return out
}

func hasModelState(
	providerName string,
	locks map[ModelKey]*modelLock,
	params map[ModelKey]map[string]bool,
) bool {
	for key := range locks {
		if key.Provider == providerName {
			return true
		}
	}
	for key, values := range params {
		if key.Provider == providerName && len(values) > 0 {
			return true
		}
	}
	return false
}

func (m *Manager) MergeQuotas(snapshots map[string]*provider.QuotaSnapshot, generation uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	if !m.generationMatchesLocked(generation) {
		return false
	}
	for name, snapshot := range snapshots {
		m.quotas[name] = cloneQuota(snapshot)
	}
	return true
}

func (m *Manager) SetQuota(name string, snapshot *provider.QuotaSnapshot, generation uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	if !m.generationMatchesLocked(generation) {
		return false
	}
	m.quotas[name] = cloneQuota(snapshot)
	return true
}

func (m *Manager) Quota(name string) *provider.QuotaSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneQuota(m.quotas[name])
}

func (m *Manager) Quotas() map[string]*provider.QuotaSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneQuotas(m.quotas)
}

func (m *Manager) ClearQuotas(generation uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	if !m.generationMatchesLocked(generation) {
		return false
	}
	m.quotas = make(map[string]*provider.QuotaSnapshot)
	return true
}

func cloneQuotas(in map[string]*provider.QuotaSnapshot) map[string]*provider.QuotaSnapshot {
	out := make(map[string]*provider.QuotaSnapshot, len(in))
	for key, value := range in {
		out[key] = cloneQuota(value)
	}
	return out
}

func cloneQuota(in *provider.QuotaSnapshot) *provider.QuotaSnapshot {
	if in == nil {
		return nil
	}
	out := *in
	out.Notes = append([]string(nil), in.Notes...)
	out.Windows = append([]provider.QuotaWindow(nil), in.Windows...)
	for i := range out.Windows {
		out.Windows[i].Details = append([]provider.QuotaDetail(nil), in.Windows[i].Details...)
	}
	return &out
}

func (m *Manager) SetSticky(key string, sticky Sticky, generation uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	if !m.generationMatchesLocked(generation) {
		return false
	}
	m.sticky[key] = sticky
	return true
}

func (m *Manager) Sticky(key string) (Sticky, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	value, ok := m.sticky[key]
	return value, ok
}

func (m *Manager) SetPin(route string, pin Pin) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	m.pins[route] = pin
}

func (m *Manager) ClearPin(route string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.pins[route]
	delete(m.pins, route)
	return ok
}

// Pins returns active pins without deleting expired entries.
func (m *Manager) Pins(now time.Time) map[string]Pin {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]Pin, len(m.pins))
	for route, pin := range m.pins {
		if pin.Active(now) {
			out[route] = pin
		}
	}
	return out
}

func (m *Manager) PinForces(exposed string, targets []Target, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	pin, ok := m.pins[exposed]
	if !ok || !pin.Active(now) {
		return false
	}
	for _, target := range targets {
		if target.Provider == pin.Provider || target.Parent == pin.Provider {
			return true
		}
	}
	return false
}

// ResetHealth clears circuit/rate-limit state and model locks, preserving
// sticky, pins, learned parameters, spread counters, and quotas.
func (m *Manager) ResetHealth(name string, parentOf map[string]string) (cleared []string, locks int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	match := func(providerName string) bool {
		return name == "" || providerName == name || parentOf[providerName] == name
	}
	for providerName := range m.health {
		if match(providerName) {
			delete(m.health, providerName)
			cleared = append(cleared, providerName)
		}
	}
	for key := range m.modelLocks {
		if match(key.Provider) {
			delete(m.modelLocks, key)
			locks++
		}
	}
	sort.Strings(cleared)
	return cleared, locks
}

func (m *Manager) ResolverSpreadStart(parent string, n int, generation uint64) int {
	if n <= 0 {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	if !m.generationMatchesLocked(generation) {
		return 0
	}
	start := int(m.spread[parent] % uint64(n))
	m.spread[parent]++
	return start
}

func (m *Manager) TargetHealthy(providerName, model string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.health[providerName]
	return (state == nil || state.available(now)) && !m.modelLockedLocked(providerName, model, now)
}

// TakeHalfOpenSlot uses the current clock to preserve the existing request-path
// API. A stale request is allowed to finish but cannot mutate the new state.
func (m *Manager) TakeHalfOpenSlot(name string, generation uint64) bool {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.generationMatchesLocked(generation) {
		return true
	}
	state := m.health[name]
	if state == nil {
		return true
	}
	if now.Before(state.rateLimitedUntil) {
		return false
	}
	if state.circuitOpenUntil.IsZero() {
		return true
	}
	if now.Before(state.circuitOpenUntil) {
		return false
	}
	if state.halfOpenInFlight {
		return false
	}
	state.halfOpenInFlight = true
	return true
}

func (m *Manager) ReleaseHalfOpenSlot(name string, generation uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.generationMatchesLocked(generation) {
		return
	}
	if state := m.health[name]; state != nil {
		state.halfOpenInFlight = false
	}
}

func (m *Manager) RecordSuccess(name, model string, generation uint64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.generationMatchesLocked(generation) {
		return
	}
	if state := m.health[name]; state != nil {
		state.consecutiveFailures = 0
		state.circuitOpenUntil = time.Time{}
		state.halfOpenInFlight = false
	}
	delete(m.modelLocks, ModelKey{Provider: name, Model: model})
}

func (m *Manager) RecordFailure(name string, threshold int, cooldown time.Duration, generation uint64) {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	if !m.generationMatchesLocked(generation) {
		return
	}
	state := m.health[name]
	if state == nil {
		state = &providerHealth{}
		m.health[name] = state
	}
	state.consecutiveFailures++
	state.halfOpenInFlight = false
	if state.consecutiveFailures >= threshold {
		state.circuitOpenUntil = now.Add(cooldown)
	}
}

func (m *Manager) modelLockedLocked(providerName, model string, now time.Time) bool {
	entry := m.modelLocks[ModelKey{Provider: providerName, Model: model}]
	return entry != nil && now.Before(entry.lockedUntil)
}

func (m *Manager) ModelLocked(providerName, model string, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.modelLockedLocked(providerName, model, now)
}

func (m *Manager) RecordModelFailure(providerName, model string, lockout time.Duration, generation uint64) {
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	if !m.generationMatchesLocked(generation) {
		return
	}
	key := ModelKey{Provider: providerName, Model: model}
	entry := m.modelLocks[key]
	if entry == nil {
		entry = &modelLock{}
		m.modelLocks[key] = entry
	}
	entry.failures++
	entry.lockedUntil = now.Add(lockout)
}

func (m *Manager) CooldownState(targets []Target, now time.Time) (allDown, allRateLimited bool, earliest time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(targets) == 0 {
		return false, false, time.Time{}
	}
	for _, target := range targets {
		state := m.health[target.Provider]
		if state == nil || state.available(now) {
			return false, false, time.Time{}
		}
	}

	allDown, allRateLimited = true, true
	for _, target := range targets {
		state := m.health[target.Provider]
		rateLimited := now.Before(state.rateLimitedUntil)
		circuitOpen := !state.circuitOpenUntil.IsZero() && now.Before(state.circuitOpenUntil)
		var until time.Time
		switch {
		case rateLimited && circuitOpen:
			allRateLimited = false
			until = state.rateLimitedUntil
			if state.circuitOpenUntil.After(until) {
				until = state.circuitOpenUntil
			}
		case rateLimited:
			until = state.rateLimitedUntil
		case circuitOpen:
			allRateLimited = false
			until = state.circuitOpenUntil
		default:
			allRateLimited = false
			until = now
		}
		if until.Before(now) {
			until = now
		}
		if earliest.IsZero() || until.Before(earliest) {
			earliest = until
		}
	}
	return allDown, allRateLimited, earliest
}

func (m *Manager) HasRecoveredUntried(targets []Target, tried map[string]bool, now time.Time) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, target := range targets {
		state := m.health[target.Provider]
		if (state == nil || state.available(now)) && !tried[target.Provider] {
			return true
		}
	}
	return false
}

func (m *Manager) LearnParamBlock(providerName, model, param string, generation uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	if !m.generationMatchesLocked(generation) {
		return false
	}
	key := ModelKey{Provider: providerName, Model: model}
	blocked := m.paramBlock[key]
	if blocked == nil {
		blocked = make(map[string]bool)
		m.paramBlock[key] = blocked
	}
	isNew := !blocked[param]
	blocked[param] = true
	return isNew
}

func (m *Manager) ParamBlock(providerName, model string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	values := m.paramBlock[ModelKey{Provider: providerName, Model: model}]
	out := make([]string, 0, len(values))
	for param := range values {
		out = append(out, param)
	}
	sort.Strings(out)
	return out
}

func (m *Manager) ParamBlocked(providerName, model, param string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.paramBlock[ModelKey{Provider: providerName, Model: model}][param]
}

func (m *Manager) RecordRateLimit(name string, until time.Time, kind RateLimitKind, generation uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	if !m.generationMatchesLocked(generation) {
		return false
	}
	state := m.health[name]
	if state == nil {
		state = &providerHealth{}
		m.health[name] = state
	}
	state.halfOpenInFlight = false
	if until.After(state.rateLimitedUntil) {
		state.rateLimitedUntil = until
		state.rateLimitKind = kind
	}
	return true
}

type scheduleState struct {
	quotas          map[string]*provider.QuotaSnapshot
	sticky          map[string]Sticky
	pins            map[string]Pin
	spread          map[string]uint64
	targetAvailable func(Target, time.Time) bool
}

func targetScheduleFacts(
	target Target,
	snapshot *provider.QuotaSnapshot,
	now time.Time,
	maxAge time.Duration,
) ScheduleFacts {
	billing := target.BillingOverride
	if billing == provider.BillingUnknown {
		switch {
		case snapshot == nil,
			snapshot.Billing == provider.BillingUnknown,
			snapshot.Err != "",
			now.Sub(snapshot.AsOf) > maxAge:
			billing = provider.BillingUnknown
		default:
			billing = snapshot.Billing
		}
	}

	peak := target.PeakMultiplier
	if peak < 1 {
		peak = 1
	}
	surplus := 0.0
	if snapshot != nil {
		surplus = snapshot.Surplus(now, peak)
	}
	return ScheduleFacts{Billing: billing, Surplus: surplus}
}

func schedulingTier(billing provider.BillingClass) int {
	switch billing {
	case provider.BillingPlan:
		return 0
	case provider.BillingPayG:
		return 2
	default:
		return 1
	}
}

// DecideOrder atomically projects Manager-owned quotas and computes scheduling
// order from quota, health, pin, sticky, model-lock, and spread state. It may
// evict stale session sticky entries and advance pool spread only for a
// committed, current-generation call. Sticky itself is never written here; the
// caller commits StickyProvider with SetSticky after the decision.
func (m *Manager) DecideOrder(input ScheduleInput) ScheduleResult {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()

	state := scheduleState{
		quotas: m.quotas,
		sticky: m.sticky,
		pins:   m.pins,
		spread: m.spread,
		targetAvailable: func(target Target, now time.Time) bool {
			health := m.health[target.Provider]
			return (health == nil || health.available(now)) &&
				!m.modelLockedLocked(target.Provider, target.Model, now)
		},
	}
	commit := input.Commit && m.generationMatchesLocked(input.Generation)
	return decideOrder(input, state, commit)
}

func decideOrder(input ScheduleInput, state scheduleState, commit bool) ScheduleResult {
	stickyKey := input.SessionKey
	if stickyKey == "" {
		stickyKey = input.Exposed
	}
	if commit {
		for key, value := range state.sticky {
			if input.RouteKeys[key] {
				continue
			}
			if input.Now.Sub(value.Since) > input.Dwell {
				delete(state.sticky, key)
			}
		}
	}

	candidates := make([]scheduleCandidate, 0, len(input.Targets))
	facts := make([]ScheduleFacts, len(input.Targets))
	for i, target := range input.Targets {
		facts[i] = targetScheduleFacts(
			target,
			state.quotas[target.Provider],
			input.Now,
			input.QuotaMaxAge,
		)
		candidates = append(candidates, scheduleCandidate{
			target:  target,
			index:   i,
			tier:    schedulingTier(facts[i].Billing),
			surplus: facts[i].Surplus,
		})
	}

	pinned := false
	if pin, ok := state.pins[input.Exposed]; ok && pin.Active(input.Now) {
		matches := candidates[:0]
		for _, candidate := range candidates {
			if candidate.target.Provider == pin.Provider || candidate.target.Parent == pin.Provider {
				matches = append(matches, candidate)
			}
		}
		if len(matches) > 0 {
			candidates = matches
			pinned = true
		}
	}

	available := candidates[:0]
	for _, candidate := range candidates {
		if pinned {
			available = append(available, candidate)
			continue
		}
		if state.targetAvailable == nil || state.targetAvailable(candidate.target, input.Now) {
			available = append(available, candidate)
		}
	}

	sort.SliceStable(available, func(i, j int) bool {
		left, right := available[i], available[j]
		if left.tier != right.tier {
			return left.tier < right.tier
		}
		if left.target.Priority != right.target.Priority {
			return left.target.Priority < right.target.Priority
		}
		return left.surplus > right.surplus
	})

	current := state.sticky[stickyKey]
	currentIndex := -1
	for i, candidate := range available {
		if candidate.target.Provider == current.Provider {
			currentIndex = i
			break
		}
	}

	keepSticky := false
	if current.Provider != "" && currentIndex >= 0 {
		if input.Now.Sub(current.Since) < input.Dwell {
			keepSticky = true
		} else if len(available) > 0 {
			best := available[0]
			currentTarget := available[currentIndex]
			switch {
			case best.target.Provider == current.Provider:
				keepSticky = true
			case best.tier < currentTarget.tier:
				keepSticky = false
			case best.target.Priority < currentTarget.target.Priority:
				keepSticky = false
			case best.surplus-currentTarget.surplus >= input.SwitchMargin:
				keepSticky = false
			default:
				keepSticky = true
			}
		}
	}

	ordered := make([]scheduleCandidate, 0, len(available))
	stickyProvider := ""
	if keepSticky {
		ordered = append(ordered, available[currentIndex])
		if stickyKey != input.Exposed {
			stickyProvider = current.Provider
		}
	} else if len(available) > 0 {
		pick := available[0]
		// Spread only within the already-winning pool rank. A lower-ranked pool
		// must never leapfrog the sorted best target merely because the route
		// contains a virtual account somewhere later in the list.
		if parent := pick.target.Parent; parent != "" {
			band := poolBandByID(
				available,
				parent,
				pick.tier,
				pick.target.Priority,
			)
			start := int(state.spread[parent] % uint64(len(band)))
			if commit {
				state.spread[parent]++
			}
			pick = band[start]
		}
		ordered = append(ordered, pick)
		stickyProvider = pick.target.Provider
	}

	for _, candidate := range available {
		if len(ordered) > 0 && candidate.target.Provider == ordered[0].target.Provider {
			continue
		}
		ordered = append(ordered, candidate)
	}

	result := ScheduleResult{
		Order:          make([]int, 0, len(ordered)),
		StickyProvider: stickyProvider,
		Facts:          facts,
	}
	for _, candidate := range ordered {
		result.Order = append(result.Order, candidate.index)
	}
	return result
}

func poolBandByID(
	candidates []scheduleCandidate,
	parent string,
	tier int,
	priority int,
) []scheduleCandidate {
	band := make([]scheduleCandidate, 0)
	for _, candidate := range candidates {
		if candidate.target.Parent == parent &&
			candidate.tier == tier &&
			candidate.target.Priority == priority {
			band = append(band, candidate)
		}
	}
	sort.SliceStable(band, func(i, j int) bool {
		return band[i].target.Provider < band[j].target.Provider
	})
	return band
}

func (m *Manager) Dashboard(now time.Time) DashboardSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()

	snapshot := DashboardSnapshot{
		Generation: m.generation,
		Providers:  make(map[string]ProviderStatus, len(m.health)),
		ModelLocks: make(map[string][]ModelLockStatus),
		Sticky:     make(map[string]Sticky, len(m.sticky)),
		Pins:       make(map[string]Pin, len(m.pins)),
		Quotas:     cloneQuotas(m.quotas),
		capturedAt: now,
		spread:     make(map[string]uint64, len(m.spread)),
	}
	for name, state := range m.health {
		circuitState := "closed"
		switch {
		case now.Before(state.circuitOpenUntil):
			circuitState = "open"
		case state.halfOpenInFlight:
			circuitState = "half_open"
		}
		snapshot.Providers[name] = ProviderStatus{
			ConsecutiveFailures: state.consecutiveFailures,
			CircuitState:        circuitState,
			Available:           state.available(now),
			CircuitOpenUntil:    state.circuitOpenUntil,
			RateLimitedUntil:    state.rateLimitedUntil,
			RateLimitKind:       state.rateLimitKind,
			HalfOpenInFlight:    state.halfOpenInFlight,
		}
	}
	for key, entry := range m.modelLocks {
		if !now.Before(entry.lockedUntil) {
			continue
		}
		snapshot.ModelLocks[key.Provider] = append(
			snapshot.ModelLocks[key.Provider],
			ModelLockStatus{
				Provider:    key.Provider,
				Model:       key.Model,
				Failures:    entry.failures,
				LockedUntil: entry.lockedUntil,
			},
		)
	}
	for providerName := range snapshot.ModelLocks {
		sort.Slice(snapshot.ModelLocks[providerName], func(i, j int) bool {
			return snapshot.ModelLocks[providerName][i].Model <
				snapshot.ModelLocks[providerName][j].Model
		})
	}
	for key, value := range m.sticky {
		snapshot.Sticky[key] = value
	}
	for route, pin := range m.pins {
		if pin.Active(now) {
			snapshot.Pins[route] = pin
		}
	}
	for parent, counter := range m.spread {
		snapshot.spread[parent] = counter
	}
	return snapshot
}

// PreviewOrder derives a read-only schedule from this exact detached
// dashboard snapshot. It never re-enters Manager, so health/quota/pin/sticky
// and the displayed order cannot come from different mutations or generations.
func (snapshot DashboardSnapshot) PreviewOrder(input ScheduleInput) ScheduleResult {
	input.Commit = false
	input.Generation = snapshot.Generation
	if !snapshot.capturedAt.IsZero() {
		input.Now = snapshot.capturedAt
	}
	state := scheduleState{
		quotas: snapshot.Quotas,
		sticky: snapshot.Sticky,
		pins:   snapshot.Pins,
		spread: snapshot.spread,
		targetAvailable: func(target Target, now time.Time) bool {
			if status, ok := snapshot.Providers[target.Provider]; ok && !status.Available {
				return false
			}
			for _, lock := range snapshot.ModelLocks[target.Provider] {
				if lock.Model == target.Model && now.Before(lock.LockedUntil) {
					return false
				}
			}
			return true
		},
	}
	return decideOrder(input, state, false)
}
