package runtime

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"model-proxy/internal/provider"
)

func TestPinsResetAndResolverHelpers(t *testing.T) {
	t.Parallel()

	now := time.Now()
	m := newTestManager(4)
	targets := []Target{
		{Provider: "direct"},
		{Provider: "pool#a", Parent: "pool"},
	}

	m.SetPin("route", Pin{Provider: "direct"})
	if !m.PinForces("route", targets, now) {
		t.Fatal("direct provider pin did not force the route")
	}
	m.SetPin("route", Pin{Provider: "pool"})
	if !m.PinForces("route", targets, now) {
		t.Fatal("pool parent pin did not force the route")
	}
	m.SetPin("route", Pin{Provider: "missing"})
	if m.PinForces("route", targets, now) {
		t.Fatal("pin with no matching target forced the route")
	}
	m.SetPin("route", Pin{Provider: "direct", ExpiresAt: now})
	if m.PinForces("route", targets, now) {
		t.Fatal("expired pin forced the route")
	}
	if !m.ClearPin("route") || m.ClearPin("route") {
		t.Fatal("ClearPin result did not distinguish present and absent pins")
	}

	m.RecordFailure("direct", 1, time.Hour, 4)
	m.RecordFailure("pool#a", 1, time.Hour, 4)
	m.RecordFailure("other", 1, time.Hour, 4)
	m.RecordModelFailure("direct", "z", time.Hour, 4)
	m.RecordModelFailure("pool#a", "a", time.Hour, 4)
	m.RecordModelFailure("other", "m", time.Hour, 4)
	m.SetSticky("route", Sticky{Provider: "direct", Since: now}, 4)
	m.SetPin("route", Pin{Provider: "direct"})
	m.LearnParamBlock("direct", "z", "temperature", 4)
	m.SetQuota("direct", &provider.QuotaSnapshot{Plan: "paid"}, 4)
	if got := m.ResolverSpreadStart("pool", 0, 4); got != 0 {
		t.Fatalf("zero-sized resolver spread = %d, want 0", got)
	}
	if got := m.ResolverSpreadStart("pool", 2, 3); got != 0 {
		t.Fatalf("stale resolver spread = %d, want stable zero", got)
	}
	if got := []int{
		m.ResolverSpreadStart("pool", 2, 4),
		m.ResolverSpreadStart("pool", 2, 4),
		m.ResolverSpreadStart("pool", 2, 4),
	}; !reflect.DeepEqual(got, []int{0, 1, 0}) {
		t.Fatalf("resolver spread sequence = %v, want [0 1 0] after stale call", got)
	}

	cleared, locks := m.ResetHealth("pool", map[string]string{"pool#a": "pool"})
	if !reflect.DeepEqual(cleared, []string{"pool#a"}) || locks != 1 {
		t.Fatalf("pool reset = cleared %v locks %d, want [pool#a], 1", cleared, locks)
	}
	if !m.ModelLocked("direct", "z", now) || !m.ModelLocked("other", "m", now) {
		t.Fatal("pool reset cleared unrelated model locks")
	}
	cleared, locks = m.ResetHealth("", nil)
	if !reflect.DeepEqual(cleared, []string{"direct", "other"}) || locks != 2 {
		t.Fatalf("global reset = cleared %v locks %d", cleared, locks)
	}
	if _, ok := m.Sticky("route"); !ok || len(m.Pins(now)) != 1 ||
		!m.ParamBlocked("direct", "z", "temperature") ||
		m.Quota("direct").Plan != "paid" {
		t.Fatal("ResetHealth cleared state outside health/model locks")
	}
}

func TestHealthCircuitHalfOpenAndSuccess(t *testing.T) {
	t.Parallel()

	now := time.Now()
	m := newTestManager(9)
	if !m.TargetHealthy("new", "m", now) {
		t.Fatal("unknown provider should be healthy")
	}
	if !m.TakeHalfOpenSlot("new", 9) {
		t.Fatal("provider without health state rejected")
	}

	m.mu.Lock()
	m.health["rate"] = &providerHealth{rateLimitedUntil: now.Add(time.Hour)}
	m.health["closed"] = &providerHealth{}
	m.health["open"] = &providerHealth{circuitOpenUntil: now.Add(time.Hour)}
	m.health["half"] = &providerHealth{circuitOpenUntil: now.Add(-time.Hour)}
	m.health["busy"] = &providerHealth{
		circuitOpenUntil: now.Add(-time.Hour),
		halfOpenInFlight: true,
	}
	m.health["success"] = &providerHealth{
		consecutiveFailures: 3,
		circuitOpenUntil:    now.Add(-time.Hour),
		rateLimitedUntil:    now.Add(time.Hour),
		rateLimitKind:       Daily,
		halfOpenInFlight:    true,
	}
	m.modelLocks[ModelKey{Provider: "model", Model: "locked"}] = &modelLock{
		failures:    2,
		lockedUntil: now.Add(time.Hour),
	}
	m.modelLocks[ModelKey{Provider: "success", Model: "m"}] = &modelLock{
		failures:    1,
		lockedUntil: now.Add(time.Hour),
	}
	m.mu.Unlock()

	if m.TargetHealthy("rate", "m", now) ||
		m.TargetHealthy("open", "m", now) ||
		m.TargetHealthy("busy", "m", now) ||
		m.TargetHealthy("model", "locked", now) {
		t.Fatal("unavailable provider/model reported healthy")
	}
	if !m.TargetHealthy("closed", "m", now) ||
		!m.TargetHealthy("half", "m", now) ||
		!m.TargetHealthy("model", "other", now) {
		t.Fatal("healthy provider/model reported unavailable")
	}
	if m.TakeHalfOpenSlot("rate", 9) ||
		m.TakeHalfOpenSlot("open", 9) ||
		m.TakeHalfOpenSlot("busy", 9) {
		t.Fatal("rate-limited, open, or busy provider acquired half-open slot")
	}
	if !m.TakeHalfOpenSlot("closed", 9) {
		t.Fatal("closed circuit rejected request")
	}
	if !m.TakeHalfOpenSlot("half", 9) {
		t.Fatal("recovered circuit did not acquire half-open slot")
	}
	if m.TakeHalfOpenSlot("half", 9) {
		t.Fatal("second request acquired occupied half-open slot")
	}
	m.ReleaseHalfOpenSlot("half", 9)
	if !m.TakeHalfOpenSlot("half", 9) {
		t.Fatal("released half-open slot was not reusable")
	}
	m.ReleaseHalfOpenSlot("missing", 9)

	m.RecordSuccess("success", "m", 9)
	status := m.Dashboard(now).Providers["success"]
	// A success from an in-flight request must not erase a later 429's cooldown:
	// rate limiting is independent of circuit recovery and has its own horizon.
	if status.ConsecutiveFailures != 0 || status.CircuitState != "closed" ||
		status.HalfOpenInFlight || m.ModelLocked("success", "m", now) ||
		!status.RateLimitedUntil.Equal(now.Add(time.Hour)) || status.RateLimitKind != Daily {
		t.Fatalf("success state = %+v, want reset circuit/model with preserved rate limit", status)
	}

	m.RecordFailure("threshold", 2, time.Hour, 9)
	first := m.Dashboard(now).Providers["threshold"]
	if first.ConsecutiveFailures != 1 || first.CircuitState != "closed" {
		t.Fatalf("first failure opened circuit: %+v", first)
	}
	m.RecordFailure("threshold", 2, time.Hour, 9)
	second := m.Dashboard(now).Providers["threshold"]
	if second.ConsecutiveFailures != 2 || second.CircuitState != "open" {
		t.Fatalf("threshold failure did not open circuit: %+v", second)
	}

	// A failed half-open probe must release the only probe slot as it reopens
	// the circuit; otherwise every later request would remain blocked forever.
	m.RecordFailure("half", 1, time.Hour, 9)
	half := m.Dashboard(time.Now()).Providers["half"]
	if half.ConsecutiveFailures != 1 || half.CircuitState != "open" ||
		half.HalfOpenInFlight || half.CircuitOpenUntil.Before(time.Now()) {
		t.Fatalf("half-open failure did not reopen and release slot: %+v", half)
	}
}

func TestModelParamAndRateLimitGenerationResults(t *testing.T) {
	t.Parallel()

	now := time.Now()
	m := newTestManager(5)
	if m.ModelLocked("p", "m", now) {
		t.Fatal("missing model lock reported active")
	}
	m.RecordModelFailure("p", "m", time.Hour, 5)
	m.RecordModelFailure("p", "m", 2*time.Hour, 5)
	if !m.ModelLocked("p", "m", now) || m.ModelLocked("p", "m", now.Add(3*time.Hour)) {
		t.Fatal("model lock horizon was not honored")
	}
	if !m.LearnParamBlock("p", "m", "z", 5) ||
		!m.LearnParamBlock("p", "m", "a", 5) ||
		m.LearnParamBlock("p", "m", "a", 5) {
		t.Fatal("parameter learning new/duplicate result is incorrect")
	}
	if got := m.ParamBlock("p", "m"); !reflect.DeepEqual(got, []string{"a", "z"}) {
		t.Fatalf("parameter block = %v, want [a z]", got)
	}
	if !m.ParamBlocked("p", "m", "a") || m.ParamBlocked("p", "m", "other") {
		t.Fatal("ParamBlocked returned incorrect result")
	}

	staleUntil := now.Add(time.Hour)
	if m.RecordRateLimit("stale", staleUntil, Daily, 4) {
		t.Fatal("stale RecordRateLimit reported committed")
	}
	if _, ok := m.Dashboard(now).Providers["stale"]; ok {
		t.Fatal("stale RecordRateLimit mutated health")
	}
	if !m.RecordRateLimit("current", now.Add(2*time.Hour), Daily, 5) {
		t.Fatal("current RecordRateLimit reported rejected")
	}
	if !m.RecordRateLimit("zero", now.Add(time.Hour), Quota, 0) {
		t.Fatal("generation-zero RecordRateLimit reported rejected")
	}
	if !m.RecordRateLimit("current", now.Add(time.Hour), Quota, 5) {
		t.Fatal("shorter RecordRateLimit reported rejected")
	}
	current := m.Dashboard(now).Providers["current"]
	if current.RateLimitKind != Daily ||
		!current.RateLimitedUntil.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("shorter rate limit overwrote longer horizon: %+v", current)
	}
}

func TestCooldownAndRecoveredState(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 7, 29, 14, 0, 0, 0, time.UTC)
	targets := []Target{{Provider: "a"}, {Provider: "b"}}
	m := newTestManager(1)
	quotaMaxAge := 15 * time.Minute

	if down, rate, earliest := m.CooldownState(nil, now, quotaMaxAge); down || rate || !earliest.IsZero() {
		t.Fatalf("empty cooldown state = %v %v %v", down, rate, earliest)
	}
	if down, rate, earliest := m.CooldownState(targets, now, quotaMaxAge); down || rate || !earliest.IsZero() {
		t.Fatalf("healthy cooldown state = %v %v %v", down, rate, earliest)
	}

	m.mu.Lock()
	m.health["a"] = &providerHealth{rateLimitedUntil: now.Add(20 * time.Minute)}
	m.health["b"] = &providerHealth{rateLimitedUntil: now.Add(10 * time.Minute)}
	m.mu.Unlock()
	down, rate, earliest := m.CooldownState(targets, now, quotaMaxAge)
	if !down || !rate || !earliest.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("all-rate cooldown = %v %v %v", down, rate, earliest)
	}

	m.mu.Lock()
	m.health["a"] = &providerHealth{
		rateLimitedUntil: now.Add(5 * time.Minute),
		circuitOpenUntil: now.Add(30 * time.Minute),
	}
	m.health["b"] = &providerHealth{circuitOpenUntil: now.Add(15 * time.Minute)}
	m.mu.Unlock()
	down, rate, earliest = m.CooldownState(targets, now, quotaMaxAge)
	if !down || rate || !earliest.Equal(now.Add(15*time.Minute)) {
		t.Fatalf("mixed cooldown = %v %v %v", down, rate, earliest)
	}

	m.mu.Lock()
	m.health["a"] = &providerHealth{circuitOpenUntil: now.Add(-time.Minute)}
	m.health["b"] = &providerHealth{circuitOpenUntil: now.Add(time.Minute)}
	m.mu.Unlock()
	if down, rate, earliest = m.CooldownState(targets, now, quotaMaxAge); down || rate || !earliest.IsZero() {
		t.Fatalf("recovered provider did not short-circuit cooldown: %v %v %v", down, rate, earliest)
	}
	if !m.HasRecoveredUntried(targets, map[string]bool{"b": true}, now, quotaMaxAge) {
		t.Fatal("untried recovered provider was not detected")
	}
	if m.HasRecoveredUntried(targets, map[string]bool{"a": true, "b": true}, now, quotaMaxAge) {
		t.Fatal("fully tried target set reported a recovered untried provider")
	}
	m.mu.Lock()
	m.health["a"].halfOpenInFlight = true
	m.mu.Unlock()
	if m.HasRecoveredUntried(targets, map[string]bool{}, now, quotaMaxAge) {
		t.Fatal("busy/open targets reported a recovered untried provider")
	}
}

// TestCooldownStateTreatsQuotaExhaustionAsRateLimitedDown: the scheduling skip
// means an exhausted target never receives the upstream 429 that would teach
// health, so CooldownState must fold the fresh snapshot in as a rate-limit
// class down reason — otherwise an all-exhausted route reads "available" and
// the forward loop terminates 502 with no Retry-After. Recovery is bounded by
// the EARLIER of the window reset and the snapshot staleness horizon.
func TestCooldownStateTreatsQuotaExhaustionAsRateLimitedDown(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	maxAge := 15 * time.Minute
	targets := []Target{{Provider: "plan"}}
	m := newTestManager(1)

	exhausted := func(asOf time.Time, resetsAt time.Time) *provider.QuotaSnapshot {
		return &provider.QuotaSnapshot{
			Billing: provider.BillingPlan, AsOf: asOf,
			Windows: []provider.QuotaWindow{{Ultimate: true, RemainingPct: 0, ResetsAt: resetsAt}},
		}
	}

	// Fresh exhaustion, reset far in the future: recovery = staleness horizon.
	m.SetQuota("plan", exhausted(now, now.Add(2*time.Hour)), 1)
	down, rate, earliest := m.CooldownState(targets, now, maxAge)
	if !down || !rate || !earliest.Equal(now.Add(maxAge)) {
		t.Fatalf("fresh exhaustion cooldown = %v %v %v, want allDown+rateLimited until AsOf+maxAge", down, rate, earliest)
	}
	// Recovery within the horizon: the nearer window reset wins.
	m.SetQuota("plan", exhausted(now, now.Add(5*time.Minute)), 1)
	down, rate, earliest = m.CooldownState(targets, now, maxAge)
	if !down || !rate || !earliest.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("near-reset cooldown = %v %v %v, want until the window reset", down, rate, earliest)
	}
	// Stale snapshot: proves nothing, fail open (target reads available).
	m.SetQuota("plan", exhausted(now.Add(-maxAge-time.Minute), now.Add(time.Minute)), 1)
	if down, rate, earliest = m.CooldownState(targets, now, maxAge); down || rate || !earliest.IsZero() {
		t.Fatalf("stale exhaustion cooldown = %v %v %v, want fail-open", down, rate, earliest)
	}

	// Mixed: one exhausted + one healthy → not all down.
	m.SetQuota("plan", exhausted(now, now.Add(5*time.Minute)), 1)
	mixed := []Target{{Provider: "plan"}, {Provider: "healthy"}}
	if down, rate, _ = m.CooldownState(mixed, now, maxAge); down || rate {
		t.Fatalf("mixed cooldown = %v %v, want available sibling short-circuits", down, rate)
	}

	// Exhaustion combines with a health rate-limit: down until the LATER of the two.
	m.mu.Lock()
	m.health["plan"] = &providerHealth{rateLimitedUntil: now.Add(3 * time.Minute)}
	m.mu.Unlock()
	m.SetQuota("plan", exhausted(now, now.Add(5*time.Minute)), 1)
	down, rate, earliest = m.CooldownState(targets, now, maxAge)
	if !down || !rate || !earliest.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("quota+health cooldown = %v %v %v, want until the later reason clears", down, rate, earliest)
	}

	// A circuit-open sibling keeps the honest 502 class (allRateLimited=false);
	// the target recovers only when BOTH reasons clear (the later one).
	m.mu.Lock()
	m.health["plan"] = &providerHealth{circuitOpenUntil: now.Add(4 * time.Minute)}
	m.mu.Unlock()
	down, rate, earliest = m.CooldownState(targets, now, maxAge)
	if !down || rate || !earliest.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("quota+circuit cooldown = %v %v %v, want allDown+circuit class until the later reason clears", down, rate, earliest)
	}
}

// TestHasRecoveredUntriedIgnoresQuotaExhausted: an exhausted target the
// scheduler deliberately skipped is not a "recovered untried" target —
// treating it as one spins idle rescheduling rounds on an all-exhausted route.
func TestHasRecoveredUntriedIgnoresQuotaExhausted(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	maxAge := 15 * time.Minute
	targets := []Target{{Provider: "plan"}}
	m := newTestManager(1)
	m.SetQuota("plan", &provider.QuotaSnapshot{
		Billing: provider.BillingPlan, AsOf: now,
		Windows: []provider.QuotaWindow{{Ultimate: true, RemainingPct: 0, ResetsAt: now.Add(time.Hour)}},
	}, 1)
	if m.HasRecoveredUntried(targets, map[string]bool{}, now, maxAge) {
		t.Fatal("quota-exhausted untried target reported as recovered")
	}
	// Fail open on staleness: a stale snapshot must not suppress the ordinary
	// TOCTOU recovery signal.
	m.SetQuota("plan", &provider.QuotaSnapshot{
		Billing: provider.BillingPlan, AsOf: now.Add(-maxAge - time.Minute),
		Windows: []provider.QuotaWindow{{Ultimate: true, RemainingPct: 0, ResetsAt: now.Add(time.Hour)}},
	}, 1)
	if !m.HasRecoveredUntried(targets, map[string]bool{}, now, maxAge) {
		t.Fatal("stale exhaustion suppressed the ordinary recovered-untried signal")
	}
}

// TestFreezeHealthMatchSemantics: freeze matches like ResetHealth (direct
// name, pooled parent → all virtual accounts) but iterates the KNOWN universe,
// so a never-failed provider (no health entry) is frozen too; an unknown name
// matches nothing — and unlike ResetHealth, an EMPTY name matches nothing too
// (freeze deliberately has no freeze-all; unfreeze keeps no-arg = all).
func TestFreezeHealthMatchSemantics(t *testing.T) {
	t.Parallel()

	m := newTestManager(7)
	parentOf := map[string]string{"pool#a": "pool", "pool#b": "pool"}
	known := []string{"direct", "pool#a", "pool#b", "other"}

	if got := m.FreezeHealth("missing", parentOf, known); len(got) != 0 {
		t.Fatalf("unknown freeze = %v, want empty", got)
	}
	if got := m.FreezeHealth("", parentOf, known); len(got) != 0 {
		t.Fatalf("empty-name freeze = %v, want empty (no freeze-all)", got)
	}
	if got := m.Dashboard(time.Now()).Providers; len(got) != 0 {
		t.Fatalf("unknown/empty freeze created health entries: %v", got)
	}

	if got := m.FreezeHealth("direct", parentOf, known); !reflect.DeepEqual(got, []string{"direct"}) {
		t.Fatalf("direct freeze = %v, want [direct]", got)
	}
	if got := m.FreezeHealth("pool", parentOf, known); !reflect.DeepEqual(got, []string{"pool#a", "pool#b"}) {
		t.Fatalf("pooled parent freeze = %v, want [pool#a pool#b]", got)
	}
	// Re-freezing is idempotent and still reports the match.
	if got := m.FreezeHealth("pool", parentOf, known); !reflect.DeepEqual(got, []string{"pool#a", "pool#b"}) {
		t.Fatalf("repeat freeze = %v, want [pool#a pool#b]", got)
	}

	now := time.Now()
	providers := m.Dashboard(now).Providers
	for _, name := range []string{"direct", "pool#a", "pool#b"} {
		status := providers[name]
		if !status.Frozen || status.Available {
			t.Errorf("%s status = %+v, want frozen and unavailable", name, status)
		}
		if m.TargetHealthy(name, "m", now) {
			t.Errorf("%s frozen but TargetHealthy", name)
		}
	}
	if providers["other"].Frozen {
		t.Error("unmatched provider was frozen")
	}
}

// TestFreezeHealthSchedulingAndLifecycle: a frozen provider is dropped by
// scheduling and stays frozen across success/failure/rate-limit recording —
// only ResetHealth (unfreeze) clears it, and freeze never touches quality
// EWMA, model locks, quotas, sticky, or pins.
func TestFreezeHealthSchedulingAndLifecycle(t *testing.T) {
	t.Parallel()

	// Real clock: RecordSuccess/RecordModelFailure stamp time.Now()
	// internally, so a fake `now` would misalign their horizons.
	now := time.Now()
	m := newTestManager(7)
	known := []string{"a", "b"}
	targets := []Target{{Provider: "a"}, {Provider: "b"}}

	order := func() []string {
		result := m.DecideOrder(ScheduleInput{
			Exposed: "route", Targets: targets, Now: now, QuotaMaxAge: time.Hour,
		})
		names := make([]string, 0, len(result.Order))
		for _, index := range result.Order {
			names = append(names, targets[index].Provider)
		}
		return names
	}
	if got := order(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("pre-freeze order = %v", got)
	}
	m.FreezeHealth("a", nil, known)
	if got := order(); !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("frozen provider not excluded from scheduling: %v", got)
	}

	// Freeze preserves every other state dimension on the entry.
	m.RecordFailure("a", 3, time.Hour, 7) // quality signal + failure count
	m.RecordModelFailure("a", "m1", time.Hour, 7)
	m.SetQuota("a", &provider.QuotaSnapshot{Plan: "paid"}, 7)
	m.SetSticky("route", Sticky{Provider: "a", Since: now}, 7)
	m.SetPin("route2", Pin{Provider: "a"})

	// Freeze itself never prunes the quality EWMA (unlike ResetHealth).
	qualityBefore := m.Dashboard(now).Quality["a"]
	m.FreezeHealth("a", nil, known)
	if got := m.Dashboard(now).Quality["a"]; got != qualityBefore {
		t.Errorf("freeze touched quality EWMA: %+v → %+v", qualityBefore, got)
	}

	// Success/failure/rate-limit recording must NOT clear the freeze. (Success
	// is recorded on m0 so the m1 model lock — cleared by RecordSuccess on the
	// SAME model by long-standing design — survives for the assertion below.)
	m.RecordSuccess("a", "m0", 7)
	m.RecordFailure("a", 1, time.Minute, 7)
	m.RecordRateLimit("a", now.Add(time.Minute), Transient, 7)
	if !m.Dashboard(now).Providers["a"].Frozen || m.TargetHealthy("a", "other-model", now) {
		t.Fatal("recording cleared the operator freeze")
	}
	if !m.ModelLocked("a", "m1", now) || m.Quota("a").Plan != "paid" {
		t.Error("freeze disturbed model locks or quotas")
	}
	if _, ok := m.Sticky("route"); !ok || len(m.Pins(now)) != 1 {
		t.Error("freeze disturbed sticky/pins")
	}

	// Unfreeze clears the freeze (and the rest of the health entry) but keeps
	// quality-adjacent state semantics of ResetHealth.
	cleared, _ := m.ResetHealth("a", nil)
	if !reflect.DeepEqual(cleared, []string{"a"}) {
		t.Fatalf("reset cleared = %v, want [a]", cleared)
	}
	if _, ok := m.Dashboard(now).Providers["a"]; ok {
		t.Error("health entry survived ResetHealth")
	}
	if got := order(); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("post-unfreeze order = %v", got)
	}
}

// TestCooldownStateClassifiesFrozenAsNonRateLimited: a frozen target is down
// but NOT rate-limit class — an all-frozen route must terminate as 502
// (allRateLimited=false) with earliest recovery "now" (no wait-retry spin:
// DecideFailure only waits when earliest is strictly in the future).
func TestCooldownStateClassifiesFrozenAsNonRateLimited(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	targets := []Target{{Provider: "a"}, {Provider: "b"}}
	m := newTestManager(1)
	m.FreezeHealth("a", nil, []string{"a", "b"})
	m.FreezeHealth("b", nil, []string{"a", "b"})

	down, rate, earliest := m.CooldownState(targets, now, 15*time.Minute)
	if !down || rate || !earliest.Equal(now) {
		t.Fatalf("all-frozen cooldown = %v %v %v, want allDown + non-rate-limit + now", down, rate, earliest)
	}
	if m.HasRecoveredUntried(targets, map[string]bool{}, now, 15*time.Minute) {
		t.Fatal("frozen target reported as recovered untried")
	}
	// A rate-limited sibling keeps its own horizon; the frozen one stays
	// non-rate-limit class.
	m.RecordRateLimit("b", now.Add(5*time.Minute), Quota, 1)
	down, rate, earliest = m.CooldownState(targets, now, 15*time.Minute)
	if !down || rate || !earliest.Equal(now) {
		t.Fatalf("frozen+rate-limited cooldown = %v %v %v, want frozen target keeps 502 class", down, rate, earliest)
	}
}

// TestFrozenHealthPersistRoundTrip: the operator freeze round-trips through
// SnapshotForPersist/RestoreHealth — including a frozen-ONLY entry (no
// cooldowns, no model state), which the zero-time drop rule must keep — and
// the JSON shape uses `frozen` with omitempty.
func TestFrozenHealthPersistRoundTrip(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	m := newTestManager(11)
	m.FreezeHealth("frozen-only", nil, []string{"frozen-only"})
	m.FreezeHealth("frozen-circuit", nil, []string{"frozen-circuit"})
	m.mu.Lock()
	m.health["frozen-circuit"].circuitOpenUntil = now.Add(time.Hour)
	m.mu.Unlock()
	m.RecordRateLimit("plain", now.Add(time.Hour), Quota, 11)

	snapshot := m.SnapshotForPersist(nil, now)
	if !snapshot.Health["frozen-only"].Frozen || !snapshot.Health["frozen-circuit"].Frozen {
		t.Fatalf("frozen flags missing from snapshot: %+v", snapshot.Health)
	}
	if snapshot.Health["plain"].Frozen {
		t.Error("non-frozen provider serialized as frozen")
	}

	data, err := json.Marshal(snapshot.Health["frozen-only"])
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	// Go's omitempty does not drop zero time.Time values, so the two cooldown
	// fields are always present; the freeze must serialize as frozen:true and
	// carry no model_locks/param_block.
	if string(raw["frozen"]) != "true" {
		t.Fatalf("frozen-only entry JSON = %s, want frozen:true", data)
	}
	if _, ok := raw["model_locks"]; ok {
		t.Fatalf("frozen-only entry carries model_locks: %s", data)
	}
	if _, ok := raw["param_block"]; ok {
		t.Fatalf("frozen-only entry carries param_block: %s", data)
	}
	if data, _ := json.Marshal(snapshot.Health["plain"]); strings.Contains(string(data), `"frozen"`) {
		t.Fatalf("omitempty violated for non-frozen entry: %s", data)
	}

	restored := newTestManager(12)
	restored.RestoreHealth(snapshot.Health, now, 4)
	status := restored.Dashboard(now).Providers
	if !status["frozen-only"].Frozen || status["frozen-only"].Available {
		t.Errorf("frozen-only entry not restored: %+v", status["frozen-only"])
	}
	if !status["frozen-circuit"].Frozen || !status["frozen-circuit"].CircuitOpenUntil.Equal(now.Add(time.Hour)) {
		t.Errorf("frozen+circuit entry not restored: %+v", status["frozen-circuit"])
	}
	if restored.TargetHealthy("frozen-only", "m", now) {
		t.Error("restored freeze does not block scheduling")
	}
	// Expired-cooldown entries still drop unless frozen: restore at a later
	// clock must keep the freeze but shed the elapsed circuit.
	later := now.Add(2 * time.Hour)
	restoredLate := newTestManager(12)
	restoredLate.RestoreHealth(snapshot.Health, later, 4)
	late := restoredLate.Dashboard(later).Providers
	if !late["frozen-circuit"].Frozen || late["frozen-circuit"].CircuitState != "closed" {
		t.Errorf("late restore = %+v, want frozen with expired circuit shed", late["frozen-circuit"])
	}
}
