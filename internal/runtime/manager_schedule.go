package runtime

import (
	"sort"
	"time"

	"model-proxy/provider"
)

type scheduleCandidate struct {
	target  Target
	index   int
	tier    int
	surplus float64
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
