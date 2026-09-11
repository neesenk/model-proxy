package runtime

import (
	"sort"
	"time"

	"model-proxy/internal/provider"
)

// QuotaRecoveredClearCooldown clears a provider's 429 rate-limit cooldown when
// a freshly committed quota snapshot is POSITIVE evidence that its budget is
// available: error-free, plan-billed, inside the freshness window, and with
// remaining budget in every window. A 429 cooldown is a PREDICTION of when the
// budget recovers (reset hint / Retry-After / kind default); the quota poll is
// the MEASUREMENT — when the measurement contradicts the prediction, the
// prediction must not keep the provider frozen (the bug: zhipu's 5h window
// reset while a stale 429 hint kept it "limited" for 80+ more minutes).
//
// Scope is deliberately narrow: only rateLimitedUntil is cleared — the
// operator freeze (frozen) and the failure-count circuit (circuitOpenUntil)
// are independent signals a quota measurement says nothing about. Any cooldown
// KIND is cleared: providers like zhipu return ambiguous 429 bodies
// ("并发限制/余额不足") that classify as transient while actually being budget
// exhaustion, and the transient backoff default (rate_backoff, 30s) is cheap
// to re-learn if the 429 was a genuine per-request rate limit. The caller
// gates by generation; returns the sorted names whose cooldown was cleared
// (for logging/tests). Callers: the periodic poll (PollAll) and the manual
// per-account refresh (PollOne) — NOT the 429-triggered RefreshOne, which
// must not fight the backoff it just observed.
func (m *Manager) QuotaRecoveredClearCooldown(names []string, now time.Time, maxAge time.Duration, generation uint64) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.generationMatchesLocked(generation) {
		return nil
	}
	var cleared []string
	for _, name := range names {
		if !quotaProvesBudgetAvailable(m.quotas[name], now, maxAge) {
			continue
		}
		state := m.health[name]
		if state == nil || !now.Before(state.rateLimitedUntil) {
			continue
		}
		state.rateLimitedUntil = time.Time{}
		state.rateLimitKind = Transient
		cleared = append(cleared, name)
	}
	sort.Strings(cleared)
	return cleared
}

// quotaProvesBudgetAvailable reports whether a quota snapshot positively
// proves the provider's budget is usable right now: plan billing, no fetch
// error, fresh inside the freshness window, and no exhausted window (every
// window carries remaining budget — an exhausted 5h or weekly window would
// keep the 429 prediction alive). Absence of proof (stale, errored, unknown
// billing) is NOT proof of recovery and must not clear anything.
func quotaProvesBudgetAvailable(snapshot *provider.QuotaSnapshot, now time.Time, maxAge time.Duration) bool {
	if snapshot == nil || snapshot.Err != "" || snapshot.Billing != provider.BillingPlan {
		return false
	}
	if maxAge > 0 && now.Sub(snapshot.AsOf) > maxAge {
		return false
	}
	if len(snapshot.Windows) == 0 {
		return false
	}
	for i := range snapshot.Windows {
		if snapshot.Windows[i].RemainingPct <= 0 {
			return false
		}
	}
	return true
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

// DeleteQuota removes a single quota snapshot when the generation matches.
// Used by the quota tracker to drop providers that are no longer polled
// (e.g. pay-as-you-go providers) without clearing the whole map.
func (m *Manager) DeleteQuota(name string, generation uint64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ensureLocked()
	if !m.generationMatchesLocked(generation) {
		return false
	}
	delete(m.quotas, name)
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
