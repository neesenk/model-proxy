package runtime

import (
	"sort"
	"time"

	"model-proxy/internal/provider"
)

// penalty combines the decayed quality signals into a surplus-units penalty.
// Zero weights (config-disabled or no data) mean zero penalty, reproducing the
// pre-quality ordering exactly. Penalties below 1e-6 snap to zero: EWMA decay
// converges asymptotically, and a residual 1e-10 must not flip an otherwise
// exact tie.
func (q QualityStatus) penalty(errWeight, ttftWeight float64) float64 {
	ttftNorm := float64(q.TTFTMilliseconds) / float64(ttftReference.Milliseconds())
	if ttftNorm > 1 {
		ttftNorm = 1
	}
	p := errWeight*q.ErrorRate + ttftWeight*ttftNorm
	if p < 1e-6 {
		return 0
	}
	return p
}

type scheduleCandidate struct {
	target Target
	index  int
	tier   int
	score  float64 // quota surplus − quality penalty: the actual ordering key
}

type scheduleState struct {
	quotas          map[string]*provider.QuotaSnapshot
	quality         map[string]QualityStatus
	sticky          map[string]Sticky
	pins            map[string]Pin
	spread          map[string]uint64
	targetAvailable func(Target, time.Time) bool
	// disabled excludes operator-disabled (provider, model) targets from the
	// candidate set BEFORE pin narrowing: the disable override is the stronger
	// operator intent — a pinned route whose pinned target is disabled falls
	// back to normal scheduling among the remaining candidates.
	disabled func(Target) bool
}

func targetScheduleFacts(
	target Target,
	snapshot *provider.QuotaSnapshot,
	now time.Time,
	maxAge time.Duration,
) ScheduleFacts {
	// The tier comes from the MEASURED snapshot only — never from a config
	// label. A provider's `billing:` field is payment-method metadata (what
	// the upstream charges), not a scheduling input: it once overrode the
	// measurement here, which ranked a provider with a measured plan window
	// as a strict last resort (and, via the quota tracker's old skip gate,
	// hid its console-only usage snapshot from the UI).
	billing := provider.BillingUnknown
	switch {
	case snapshot == nil,
		snapshot.Billing == provider.BillingUnknown,
		snapshot.Err != "",
		now.Sub(snapshot.AsOf) > maxAge:
	default:
		billing = snapshot.Billing
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

// effectiveBilling resolves the tier's billing class: a fresh measurement
// wins; without one the config-DECLARED class fills the gap (explicit
// declarations only — undeclared stays unknown, the honest middle tier).
// This is the corrected shape of the removed BillingOverride: the label
// never overrides a measurement, it only deputizes when there is none —
// same-class providers (plan vs plan) then compete on priority/surplus
// instead of being split by mere measurability.
func effectiveBilling(measured, declared provider.BillingClass) provider.BillingClass {
	if measured != provider.BillingUnknown {
		return measured
	}
	return declared
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
	// Project the decayed quality statuses BEFORE the lock: the state is
	// copy-on-write immutable, so the map allocation + EWMA math per provider
	// normally stays outside the scheduling critical section. The source
	// pointer is revalidated under m.mu so a concurrent publication (especially
	// ReplaceGeneration) cannot pair newer locked state with older quality.
	projectedQuality := m.projectQualitySnapshot(input.Now)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	quality := m.reconcileQualityProjectionLocked(projectedQuality, input.Now)

	state := scheduleState{
		quotas:  m.quotas,
		quality: quality,
		sticky:  m.sticky,
		pins:    m.pins,
		spread:  m.spread,
		targetAvailable: func(target Target, now time.Time) bool {
			health := m.health[target.Provider]
			return (health == nil || health.available(now)) &&
				!m.modelLockedLocked(target.Provider, target.Model, now) &&
				!quotaExhausted(m.quotas[target.Provider], now, input.QuotaMaxAge)
		},
		disabled: m.targetDisabledLocked,
	}
	commit := input.Commit && m.generationMatchesLocked(input.Generation)
	return decideOrder(input, state, commit)
}

// projectQuality projects an immutable EWMA snapshot to detached statuses
// decayed to `now` (a provider that stopped failing must not carry a stale
// penalty). Pure — no lock, no Manager state.
func projectQuality(state map[string]providerQuality, now time.Time) map[string]QualityStatus {
	if len(state) == 0 {
		return nil
	}
	out := make(map[string]QualityStatus, len(state))
	for name, q := range state {
		errRate, ttftNorm := q.decayed(now)
		out[name] = QualityStatus{
			ErrorRate:        errRate,
			TTFTMilliseconds: int64(ttftNorm * float64(ttftReference.Milliseconds())),
		}
	}
	return out
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
		facts[i].QualityPenalty = state.quality[target.Provider].penalty(
			input.QualityErrWeight,
			input.QualityTTFTWeight,
		)
		candidates = append(candidates, scheduleCandidate{
			target: target,
			index:  i,
			tier:   schedulingTier(effectiveBilling(facts[i].Billing, target.Billing)),
			score:  facts[i].Surplus - facts[i].QualityPenalty,
		})
	}

	// Operator disabled-model override: drop disabled targets from the
	// candidate set entirely (before pin narrowing — see scheduleState.disabled).
	// Facts keep their original input indices, so the projection stays aligned.
	if state.disabled != nil {
		kept := candidates[:0]
		for _, candidate := range candidates {
			if !state.disabled(candidate.target) {
				kept = append(kept, candidate)
			}
		}
		candidates = kept
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
		return left.score > right.score
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
			case best.score-currentTarget.score >= input.SwitchMargin:
				// The quality penalty folds into the score, so a degrading
				// sticky account escapes through the SAME margin gate — no
				// separate escape path to keep in sync.
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
	// The quality map loads atomically; projecting (per-provider EWMA walk)
	// outside m.mu keeps the common-path lock critical section minimal. The
	// source pointer is revalidated after locking, matching DecideOrder.
	projectedQuality := m.projectQualitySnapshot(now)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	quality := m.reconcileQualityProjectionLocked(projectedQuality, now)

	snapshot := DashboardSnapshot{
		Generation: m.generation,
		Providers:  make(map[string]ProviderStatus, len(m.health)),
		ModelLocks: make(map[string][]ModelLockStatus),
		Sticky:     make(map[string]Sticky, len(m.sticky)),
		Pins:       make(map[string]Pin, len(m.pins)),
		Quotas:     cloneQuotas(m.quotas),
		Quality:    quality,
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
			Frozen:              state.frozen,
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
	snapshot.disabled = cloneDisabledModels(m.disabledModels)
	return snapshot
}

// quotaExhausted reports whether a FRESH plan snapshot's ultimate budget
// window is measured to exactly zero. Such a target is skipped BEFORE session
// sticky is honored: sticking to a known-exhausted account just eats a
// guaranteed 429 before failing over. The check reads the ultimate WINDOW
// (never the snapshot's top-level RemainingPct): a plan snapshot without a
// measured ultimate window — RemainingPct unset/-1 — proves nothing and must
// fail open to the reactive 429 cooldown, as must a stale or errored
// snapshot. PayG/unknown billing has no window to exhaust.
func quotaExhausted(snapshot *provider.QuotaSnapshot, now time.Time, maxAge time.Duration) bool {
	return !QuotaExhaustedUntil(snapshot, now, maxAge).IsZero()
}

// QuotaExhaustedUntil reports when a freshly quota-exhausted target becomes
// schedulable again, or the zero time when the target is not (provably)
// exhausted. The recovery bound is the EARLIER of the ultimate window's
// measured reset and the snapshot's staleness horizon (AsOf+maxAge): past the
// horizon the snapshot proves nothing and the reactive 429 cooldown takes
// over, so clients are told to re-probe rather than wait out a possibly
// far-future reset on stale data. Like quotaExhausted, this reads only the
// ultimate window (never the snapshot's top-level RemainingPct): a plan
// snapshot without a measured ultimate window fails open, as do stale or
// errored snapshots and non-plan billing.
func QuotaExhaustedUntil(snapshot *provider.QuotaSnapshot, now time.Time, maxAge time.Duration) time.Time {
	if snapshot == nil || snapshot.Billing != provider.BillingPlan || snapshot.Err != "" {
		return time.Time{}
	}
	for i := range snapshot.Windows {
		if window := &snapshot.Windows[i]; window.Ultimate && window.RemainingPct == 0 {
			if now.Sub(snapshot.AsOf) > maxAge {
				return time.Time{}
			}
			until := snapshot.AsOf.Add(maxAge)
			if window.ResetsAt.After(now) && window.ResetsAt.Before(until) {
				until = window.ResetsAt
			}
			if until.Before(now) {
				until = now
			}
			return until
		}
	}
	return time.Time{}
}

// PreviewOrder derives a read-only schedule from this exact detached
// dashboard snapshot. It never re-enters Manager, so health/quota/pin/sticky
// and the displayed order cannot come from different mutations or generations.
// input.IgnorePins renders the chain as if no operator pin existed (the
// dashboard overlays the pin on the default chain instead of showing the
// pin-narrowed chain).
func (snapshot DashboardSnapshot) PreviewOrder(input ScheduleInput) ScheduleResult {
	input.Commit = false
	input.Generation = snapshot.Generation
	if !snapshot.capturedAt.IsZero() {
		input.Now = snapshot.capturedAt
	}
	pins := snapshot.Pins
	if input.IgnorePins {
		pins = nil
	}
	state := scheduleState{
		quotas:  snapshot.Quotas,
		quality: snapshot.Quality,
		sticky:  snapshot.Sticky,
		pins:    pins,
		spread:  snapshot.spread,
		targetAvailable: func(target Target, now time.Time) bool {
			if status, ok := snapshot.Providers[target.Provider]; ok && !status.Available {
				return false
			}
			for _, lock := range snapshot.ModelLocks[target.Provider] {
				if lock.Model == target.Model && now.Before(lock.LockedUntil) {
					return false
				}
			}
			return !quotaExhausted(snapshot.Quotas[target.Provider], now, input.QuotaMaxAge)
		},
		disabled: snapshot.disabled.targetDisabled,
	}
	return decideOrder(input, state, false)
}
