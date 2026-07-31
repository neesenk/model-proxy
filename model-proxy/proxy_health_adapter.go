package main

import (
	"time"

	runtimestate "model-proxy/internal/runtime"
	"model-proxy/internal/targetexec"
)

func (p *Proxy) takeHalfOpenSlot(name string, generations ...uint64) bool {
	return p.runtimeState.TakeHalfOpenSlot(
		name,
		runtimeGenerationArg(generations),
	)
}

func (p *Proxy) releaseHalfOpenSlot(name string, generations ...uint64) {
	p.runtimeState.ReleaseHalfOpenSlot(
		name,
		runtimeGenerationArg(generations),
	)
}

func (p *Proxy) recordSuccess(name, model string, generations ...uint64) {
	p.runtimeState.RecordSuccess(
		name,
		model,
		runtimeGenerationArg(generations),
	)
}

// recordFailure increments a provider's consecutive failures and opens the
// circuit (for cooldown) once the threshold is reached. Clears any half-open slot.
func (p *Proxy) recordFailure(name string, sched Scheduling, generations ...uint64) {
	p.runtimeState.RecordFailure(
		name,
		sched.Threshold(),
		sched.Cooldown(),
		runtimeGenerationArg(generations),
	)
}

// modelLocked is the lock-taking variant for tryTarget's entry check.
func (p *Proxy) modelLocked(provider, model string, now time.Time) bool {
	return p.runtimeState.ModelLocked(provider, model, now)
}

// recordModelFailure locks (provider, model) for model_lockout. Model-level
// failures (404 / model-denied / empty 200) never touch the account's circuit
// breaker — the account may serve its other models fine.
func (p *Proxy) recordModelFailure(provider, model string, sched Scheduling, generations ...uint64) {
	p.runtimeState.RecordModelFailure(
		provider,
		model,
		sched.ModelLockoutDuration(),
		runtimeGenerationArg(generations),
	)
}

// resetHealth clears frozen runtime health state (circuit-open cooldowns,
// rate-limit cooldowns, model lockouts) so the named provider — or every
// provider when name == "" — is retried immediately instead of waiting out a
// possibly hours-long cooldown (quota-exhausted 429s). Learned param
// blocklists, sticky routes, and pins are NOT cleared (request-shape
// knowledge / routing decisions, not frozen health). A pooled parent name
// matches all its virtual accounts (same matching as pins). Returns the
// cleared provider names + the number of model locks removed.
func (p *Proxy) resetHealth(name string) (cleared []string, locks int) {
	p.mu.RLock()
	parentOf := p.parentOf
	p.mu.RUnlock()
	return p.runtimeState.ResetHealth(name, parentOf)
}

// cooldownState inspects a route's target providers' health for the wait-retry
// decision: allDown = EVERY target's provider is currently unavailable
// (rate-limited or circuit-open / half-open probe in flight); allRateLimited =
// none of the down providers is there for circuit reasons (pure rate-limit —
// the honest terminal status is then 429, not 502); earliest = soonest
// cooldown expiry (clamped to now for half-open probes, so callers don't wait
// on a probe that's already deciding). Model-level locks are not consulted —
// they make schedule drop the target, which leads here via the ordinary
// all-failed path.
func (p *Proxy) cooldownState(targets []RouteTarget, now time.Time) (allDown, allRateLimited bool, earliest time.Time) {
	runtimeTargets := make([]runtimestate.Target, len(targets))
	for index, target := range targets {
		runtimeTargets[index] = runtimestate.Target{
			Provider: target.Provider,
			Model:    target.Model,
		}
	}
	return p.runtimeState.CooldownState(runtimeTargets, now)
}

// hasRecoveredUntried reports the TOCTOU case: a target is available now but was
// NOT tried in the failed pass — its cooldown lapsed mid-pass while a sibling
// re-failed, OR every target recovered simultaneously after schedule dropped them
// all. The caller answers with an immediate zero-wait re-schedule instead of a
// terminal error. No "at least one other target still cooling" precondition: that
// made the all-recover-simultaneously case terminally fail. The round budget in
// forward (≤2 retries) bounds the loop.
func (p *Proxy) hasRecoveredUntried(targets []RouteTarget, tried map[string]bool, now time.Time) bool {
	runtimeTargets := make([]runtimestate.Target, len(targets))
	for index, target := range targets {
		runtimeTargets[index] = runtimestate.Target{
			Provider: target.Provider,
			Model:    target.Model,
		}
	}
	return p.runtimeState.HasRecoveredUntried(runtimeTargets, tried, now)
}

// learnParamBlock records an upstream-rejected top-level request parameter for
// a (provider, model); subsequent requests strip it preemptively
// (applyParamBlock). Scoped per MODEL: one model's quirk (e.g. reasoning
// models rejecting temperature) must not strip params for its siblings.
// Reports whether the parameter is newly learned (for log-once).
func (p *Proxy) learnParamBlock(provider, model, param string, generations ...uint64) (isNew bool) {
	return p.runtimeState.LearnParamBlock(
		provider,
		model,
		param,
		runtimeGenerationArg(generations),
	)
}

// applyParamBlock strips every learned-unsupported top-level parameter for the
// (provider, model) from the outgoing body. Best-effort: a non-JSON body (or
// one the params aren't in) passes through unchanged.
func (p *Proxy) applyParamBlock(provider, model string, body []byte) []byte {
	for _, param := range p.runtimeState.ParamBlock(provider, model) {
		if nb, did := targetexec.StripTopLevelParam(body, param); did {
			body = nb
		}
	}
	return body
}

// recordRateLimit marks a provider rate-limited until `until` (extends if later),
// records the exhaustion class for display, and clears any half-open slot. Does
// not count toward the circuit. It then triggers an async quota refresh of the
// provider so its snapshot is fresh when the rate-limit clears.
func (p *Proxy) recordRateLimit(name string, until time.Time, kind runtimestate.RateLimitKind, generations ...uint64) {
	generation := runtimeGenerationArg(generations)
	if !p.runtimeState.RecordRateLimit(name, until, kind, generation) {
		return
	}
	if p.quota != nil {
		// refreshAsync is tracked + stop-aware: a 429-triggered refresh can't
		// outlive Proxy.Close (no persist after the final flush).
		p.quota.RefreshAsync(name, generation)
	}
}
