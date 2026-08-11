package app

import (
	configdomain "model-proxy/internal/config"
	"time"

	runtimestate "model-proxy/internal/runtime"
	"model-proxy/provider"
)

// pinEntry is a manual route→provider pin (model-proxy pin <route> <provider>
// --ttl). expiresAt zero = no expiry (until unpin). The runtime Manager owns
// storage; this private value is the application/Web compatibility projection.
type pinEntry struct {
	provider  string
	expiresAt time.Time
}

// active reports whether the pin is still in effect at now (zero expiresAt =
// never expires).
func (e pinEntry) active(now time.Time) bool {
	return e.expiresAt.IsZero() || now.Before(e.expiresAt)
}

// expiresLabel returns "" (no expiry), a "expires <relative>" hint, or "expired".
func (e pinEntry) expiresLabel(now time.Time) string {
	if e.expiresAt.IsZero() {
		return ""
	}
	d := e.expiresAt.Sub(now)
	if d <= 0 {
		return "expired"
	}
	return "expires in " + d.Round(time.Second).String()
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
		Exposed:      exposed,
		SessionKey:   sessionKey,
		Targets:      runtimeTargets,
		RouteKeys:    routeKeys,
		Dwell:        cfg.Scheduling.Dwell(),
		SwitchMargin: cfg.Scheduling.SwitchMargin(),
		Now:          now,
		QuotaMaxAge:  3 * cfg.Scheduling.PollInterval(),
		Commit:       commit,
		Generation:   runtimestate.GenerationArg(generations),
	})
	ordered = make([]RouteTarget, 0, len(result.Order))
	for _, index := range result.Order {
		ordered = append(ordered, targets[index])
	}
	return ordered, result.StickyProvider
}
