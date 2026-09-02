// proxy_schedule.go — quota-aware scheduling: config route/pin inputs adapted onto the runtime Manager, plus the read-only /debug/schedule status projection from one detached dashboard snapshot.
package app

import (
	"encoding/json"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/provider"
	runtimestate "model-proxy/internal/runtime"
	"sort"
	"time"
)

// pinEntry is a manual route→provider pin (model-proxy pin <route> <provider>
// --ttl). expiresAt zero = no expiry (until unpin). The runtime Manager owns
// storage; this private value is the application/Web compatibility projection.
type pinEntry struct {
	provider  string
	expiresAt time.Time
}

func configuredBillingOverride(value string) provider.BillingClass {
	if value == "pay-as-you-go" {
		return provider.BillingPayG
	}
	return provider.BillingUnknown
}

// schedule returns targets in try-order using quota-aware ranking:
//
//	tier: plan < unknown < payg (pay-as-you-go is strict last-resort)
//	within tier: priority asc (config), then surplus desc (breaks priority ties)
//
// Sticky routing keeps the current provider for sticky_dwell (cache-friendly),
// then re-selects the best unless the best's only edge is a sub-margin surplus gain
// (priority beats surplus; surplus only matters at equal priority).
func (p *Proxy) schedule(cfg *Config, parentOf map[string]string, exposed, sessionKey string, targets []RouteTarget, routeKeys map[string]bool, generations ...uint64) []RouteTarget {
	now := time.Now()
	ordered, stickyToSet := p.decideOrder(cfg, parentOf, exposed, sessionKey, targets, now, true, routeKeys, generations...)
	if stickyToSet != "" {
		// Commit sticky on the SESSION key (fallback to the exposed model for
		// non-session clients), so one conversation parks on one provider and
		// distinct conversations spread across the pool.
		sk := sessionKey
		if sk == "" {
			sk = exposed
		}
		p.runtimeState.SetSticky(
			sk,
			runtimestate.Sticky{Provider: stickyToSet, Since: now},
			runtimestate.GenerationArg(generations),
		)
	}
	return ordered
}

// setPin installs a manual route→provider pin (hot-switch). ttl <= 0 means no
// expiry (pin until clearPin). The provider must be a target of the route (after
// virtual/pool expansion) or setPin returns false — a pin to a provider the route
// can't reach would silently do nothing, so reject it up front with a clear error.
func (p *Proxy) setPin(route, provider string, ttl time.Duration) (pinEntry, bool) {
	now := time.Now()
	p.mu.RLock()
	expanded := p.expandedRoutes
	parentOf := p.parentOf
	p.mu.RUnlock()
	targets, ok := expanded[route]
	if !ok {
		return pinEntry{}, false
	}
	matches := false
	for _, t := range targets {
		if t.Provider == provider || parentOf[t.Provider] == provider {
			matches = true
			break
		}
	}
	if !matches {
		return pinEntry{}, false
	}
	pe := pinEntry{provider: provider}
	if ttl > 0 {
		pe.expiresAt = now.Add(ttl)
	}
	p.runtimeState.SetPin(route, runtimestate.Pin{
		Provider:  pe.provider,
		ExpiresAt: pe.expiresAt,
	})
	return pe, true
}

// clearPin removes a route's manual pin (no-op if none).
func (p *Proxy) clearPin(route string) bool {
	return p.runtimeState.ClearPin(route)
}

// listPins returns the active pins (provider + expiry), dropping expired ones.
func (p *Proxy) listPins() map[string]pinEntry {
	now := time.Now()
	pins := p.runtimeState.Pins(now)
	out := make(map[string]pinEntry, len(pins))
	for route, pin := range pins {
		out[route] = pinEntry{provider: pin.Provider, expiresAt: pin.ExpiresAt}
	}
	return out
}

// pinForces reports whether an active pin for `exposed` is in effect over the
// given ordered targets (i.e. decideOrder narrowed to the pinned provider). When
// true, forward treats the route as pinned-exclusive: request-aware routing is
// skipped (no reroute away from the pin) and tryTarget bypasses the circuit.
func (p *Proxy) pinForces(exposed string, ordered []RouteTarget, parentOf map[string]string) bool {
	targets := make([]runtimestate.Target, len(ordered))
	for index, target := range ordered {
		targets[index] = runtimestate.Target{
			Provider: target.Provider,
			Parent:   parentOf[target.Provider],
			Model:    target.Model,
		}
	}
	return p.runtimeState.PinForces(exposed, targets, time.Now())
}

// decideOrder computes the try-order for targets and the provider to park sticky
// on ("" = leave the current sticky untouched), WITHOUT mutating sticky (the
// counter bump when commit=true is the one exception — it advances the per-parent
// round-robin, not sticky). schedule() commits the sticky on the SESSION key;
// scheduleStatus() (the /debug/schedule endpoint) calls this with commit=false for
// a read-only peek. parentOf resolves pooled virtual ids to their parent's config
// (billing/peak are parent-level, not per-account) AND drives per-parent
// round-robin assignment of new sessions.
func (p *Proxy) decideOrder(cfg *Config, parentOf map[string]string, exposed, sessionKey string, targets []RouteTarget, now time.Time, commit bool, routeKeys map[string]bool, generations ...uint64) (ordered []RouteTarget, stickyToSet string) {
	runtimeTargets := make([]runtimestate.Target, len(targets))
	for index, target := range targets {
		pconf, _ := configdomain.ProviderConfig(cfg, parentOf, target.Provider)
		runtimeTargets[index] = runtimestate.Target{
			Provider:        target.Provider,
			Parent:          parentOf[target.Provider],
			Model:           target.Model,
			Priority:        target.Priority,
			BillingOverride: configuredBillingOverride(pconf.Billing),
			PeakMultiplier:  pconf.PeakMultiplier(now),
		}
	}
	result := p.runtimeState.DecideOrder(runtimestate.ScheduleInput{
		Exposed:           exposed,
		SessionKey:        sessionKey,
		Targets:           runtimeTargets,
		RouteKeys:         routeKeys,
		Dwell:             cfg.Scheduling.Dwell(),
		SwitchMargin:      cfg.Scheduling.SwitchMargin(),
		Now:               now,
		QuotaMaxAge:       p.quotaFreshnessMaxAge(cfg),
		QualityErrWeight:  cfg.Scheduling.QualityErrorWeightValue(),
		QualityTTFTWeight: cfg.Scheduling.QualityTTFTWeightValue(),
		Commit:            commit,
		Generation:        runtimestate.GenerationArg(generations),
	})
	ordered = make([]RouteTarget, 0, len(result.Order))
	for _, index := range result.Order {
		ordered = append(ordered, targets[index])
	}
	return ordered, result.StickyProvider
}

// scheduleStatus builds a read-only JSON snapshot of what each route would
// schedule right now: the first-choice provider, the full ordered list (with
// tier/surplus/availability/peak per provider), and the current sticky selection
// (+ dwell remaining). Used by the /debug/schedule endpoint. It does NOT mutate
// sticky — it previews the detached Manager dashboard snapshot.
//
// Credential pools are surfaced (Task 9 observability): each virtual in `ordered`
// carries its `pool_parent`, and each route whose targets share a pool carries a
// `pools` summary (parent + total accounts + how many are currently available).
// Existing fields are unchanged — consumers that don't read the new fields see
// the same shape as before.
func (p *Proxy) scheduleStatus() []byte {
	now := time.Now()
	p.mu.RLock()
	cfg := p.cfg
	expanded := p.expandedRoutes
	parentOf := p.parentOf
	poolIndex := p.poolIndex
	RuntimeSnapshot := p.runtimeState.Dashboard(now)
	p.mu.RUnlock()
	return scheduleStatusFromSnapshot(
		cfg,
		expanded,
		parentOf,
		poolIndex,
		RuntimeSnapshot,
		now,
	)
}

// scheduleStatusFromSnapshot is deliberately pure with respect to Proxy and
// Manager state. Both /debug/schedule and /api/status pass one dashboard
// snapshot captured alongside one config generation; the displayed ordering,
// health, quota facts, pin, sticky, and spread position therefore cannot mix
// independent runtime reads.
func scheduleStatusFromSnapshot(
	cfg *Config,
	expanded map[string][]RouteTarget,
	parentOf map[string]string,
	poolIndex map[string][]string,
	RuntimeSnapshot runtimestate.DashboardSnapshot,
	now time.Time,
) []byte {
	avail := func(name string) bool {
		state, ok := RuntimeSnapshot.Providers[name]
		return !ok || state.Available
	}

	type provInfo struct {
		Provider   string  `json:"provider"`
		PoolParent string  `json:"pool_parent,omitempty"`
		Priority   int     `json:"priority"`
		Tier       string  `json:"tier"`
		Surplus    float64 `json:"surplus"`
		// QualityPenalty is subtracted from Surplus for ordering — surfacing it
		// explains WHY a quota-rich but degrading provider sank in the order.
		QualityPenalty float64 `json:"quality_penalty"`
		Available      bool    `json:"available"`
		Peak           bool    `json:"peak"`
	}
	type poolInfo struct {
		Parent    string `json:"parent"`
		Accounts  int    `json:"accounts"`
		Available int    `json:"available"`
	}
	type routeInfo struct {
		First      string     `json:"first"`
		Ordered    []provInfo `json:"ordered"`
		Sticky     string     `json:"sticky,omitempty"`
		DwellRem   float64    `json:"sticky_dwell_remaining_sec,omitempty"`
		Pools      []poolInfo `json:"pools,omitempty"`
		Pin        string     `json:"pin,omitempty"`
		PinExpires string     `json:"pin_expires,omitempty"`
	}

	models := map[string]routeInfo{}
	routeKeys := make(map[string]bool, len(expanded))
	for k := range expanded {
		routeKeys[k] = true
	}
	for exposed, targets := range expanded {
		runtimeTargets := make([]runtimestate.Target, len(targets))
		for index, target := range targets {
			pconf, _ := configdomain.ProviderConfig(cfg, parentOf, target.Provider)
			runtimeTargets[index] = runtimestate.Target{
				Provider:        target.Provider,
				Parent:          parentOf[target.Provider],
				Model:           target.Model,
				Priority:        target.Priority,
				BillingOverride: configuredBillingOverride(pconf.Billing),
				PeakMultiplier:  pconf.PeakMultiplier(now),
			}
		}
		decision := RuntimeSnapshot.PreviewOrder(runtimestate.ScheduleInput{
			Exposed:           exposed,
			Targets:           runtimeTargets,
			RouteKeys:         routeKeys,
			Dwell:             cfg.Scheduling.Dwell(),
			SwitchMargin:      cfg.Scheduling.SwitchMargin(),
			Now:               now,
			QuotaMaxAge:       3 * cfg.Scheduling.PollInterval(),
			QualityErrWeight:  cfg.Scheduling.QualityErrorWeightValue(),
			QualityTTFTWeight: cfg.Scheduling.QualityTTFTWeightValue(),
			Generation:        RuntimeSnapshot.Generation,
		})
		ordered := make([]RouteTarget, 0, len(decision.Order))
		for _, index := range decision.Order {
			ordered = append(ordered, targets[index])
		}
		ri := routeInfo{}
		if len(ordered) > 0 {
			ri.First = ordered[0].Provider
		}
		// Surface an active manual pin (hot-switch) so /debug/schedule shows WHY a
		// route is narrowed to one provider, plus its expiry. The pin's effect on
		// `ordered` is already applied inside decideOrder; this just labels it.
		if pin, ok := RuntimeSnapshot.Pins[exposed]; ok {
			ri.Pin = pin.Provider
			ri.PinExpires = pin.ExpiresLabel(now)
		}
		// Track which parents appear in `ordered` so the route-level `pools`
		// summary can be emitted. A parent may have more accounts in poolIndex
		// than are currently in `ordered` (some unavailable) — Accounts uses
		// poolIndex (total), Available counts only those in `ordered`.
		parentSeen := map[string]bool{}
		for orderIndex, t := range ordered {
			targetIndex := decision.Order[orderIndex]
			pconf, _ := configdomain.ProviderConfig(cfg, parentOf, t.Provider)
			parent := parentOf[t.Provider]
			if parent != "" {
				parentSeen[parent] = true
			}
			ri.Ordered = append(ri.Ordered, provInfo{
				Provider:       t.Provider,
				PoolParent:     parent,
				Priority:       t.Priority,
				Tier:           billingClassName(decision.Facts[targetIndex].Billing),
				Surplus:        decision.Facts[targetIndex].Surplus,
				QualityPenalty: decision.Facts[targetIndex].QualityPenalty,
				Available:      avail(t.Provider),
				Peak:           pconf.PeakMultiplier(now) > 1,
			})
		}
		if len(parentSeen) > 0 {
			parents := make([]string, 0, len(parentSeen))
			for pp := range parentSeen {
				parents = append(parents, pp)
			}
			sort.Strings(parents)
			for _, parent := range parents {
				availCount := 0
				for _, t := range ordered {
					if parentOf[t.Provider] == parent && avail(t.Provider) {
						availCount++
					}
				}
				ri.Pools = append(ri.Pools, poolInfo{
					Parent:    parent,
					Accounts:  len(poolIndex[parent]),
					Available: availCount,
				})
			}
		}
		if cur := RuntimeSnapshot.Sticky[exposed]; cur.Provider != "" {
			ri.Sticky = cur.Provider
			if rem := cfg.Scheduling.Dwell() - now.Sub(cur.Since); rem > 0 && rem < cfg.Scheduling.Dwell() {
				ri.DwellRem = rem.Seconds()
			}
		}
		models[exposed] = ri
	}
	out, _ := json.Marshal(map[string]any{"models": models})
	return out
}

// billingClassName renders a BillingClass for the /debug/schedule + doctor output.
func billingClassName(b provider.BillingClass) string {
	switch b {
	case provider.BillingPlan:
		return "plan"
	case provider.BillingPayG:
		return "pay-as-you-go"
	default:
		return "unknown"
	}
}
