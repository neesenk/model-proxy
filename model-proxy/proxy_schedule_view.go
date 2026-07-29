package main

import (
	"encoding/json"
	"sort"
	"time"

	runtimestate "model-proxy/internal/runtime"
	"model-proxy/provider"
)

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
	runtimeSnapshot := p.runtimeState.Dashboard(now)
	p.mu.RUnlock()
	return scheduleStatusFromSnapshot(
		cfg,
		expanded,
		parentOf,
		poolIndex,
		runtimeSnapshot,
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
	runtimeSnapshot runtimestate.DashboardSnapshot,
	now time.Time,
) []byte {
	avail := func(name string) bool {
		state, ok := runtimeSnapshot.Providers[name]
		return !ok || state.Available
	}

	type provInfo struct {
		Provider   string  `json:"provider"`
		PoolParent string  `json:"pool_parent,omitempty"`
		Priority   int     `json:"priority"`
		Tier       string  `json:"tier"`
		Surplus    float64 `json:"surplus"`
		Available  bool    `json:"available"`
		Peak       bool    `json:"peak"`
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
			pconf, _ := providerConfig(cfg, parentOf, target.Provider)
			runtimeTargets[index] = runtimestate.Target{
				Provider:        target.Provider,
				Parent:          parentOf[target.Provider],
				Model:           target.Model,
				Priority:        target.Priority,
				BillingOverride: configuredBillingOverride(pconf.Billing),
				PeakMultiplier:  pconf.PeakMultiplier(now),
			}
		}
		decision := runtimeSnapshot.PreviewOrder(runtimestate.ScheduleInput{
			Exposed:      exposed,
			Targets:      runtimeTargets,
			RouteKeys:    routeKeys,
			Dwell:        cfg.Scheduling.Dwell(),
			SwitchMargin: cfg.Scheduling.SwitchMargin(),
			Now:          now,
			QuotaMaxAge:  3 * cfg.Scheduling.PollInterval(),
			Generation:   runtimeSnapshot.Generation,
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
		if pin, ok := runtimeSnapshot.Pins[exposed]; ok {
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
			pconf, _ := providerConfig(cfg, parentOf, t.Provider)
			parent := parentOf[t.Provider]
			if parent != "" {
				parentSeen[parent] = true
			}
			ri.Ordered = append(ri.Ordered, provInfo{
				Provider:   t.Provider,
				PoolParent: parent,
				Priority:   t.Priority,
				Tier:       billingClassName(decision.Facts[targetIndex].Billing),
				Surplus:    decision.Facts[targetIndex].Surplus,
				Available:  avail(t.Provider),
				Peak:       pconf.PeakMultiplier(now) > 1,
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
		if cur := runtimeSnapshot.Sticky[exposed]; cur.Provider != "" {
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
