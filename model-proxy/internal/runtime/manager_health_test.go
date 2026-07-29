package runtime

import (
	"reflect"
	"testing"
	"time"

	"model-proxy/provider"
)

func TestPinsResetAndResolverHelpers(t *testing.T) {
	t.Parallel()

	now := time.Now()
	m := NewManager(4)
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
	m := NewManager(9)
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
	m := NewManager(5)
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
	m := NewManager(1)

	if down, rate, earliest := m.CooldownState(nil, now); down || rate || !earliest.IsZero() {
		t.Fatalf("empty cooldown state = %v %v %v", down, rate, earliest)
	}
	if down, rate, earliest := m.CooldownState(targets, now); down || rate || !earliest.IsZero() {
		t.Fatalf("healthy cooldown state = %v %v %v", down, rate, earliest)
	}

	m.mu.Lock()
	m.health["a"] = &providerHealth{rateLimitedUntil: now.Add(20 * time.Minute)}
	m.health["b"] = &providerHealth{rateLimitedUntil: now.Add(10 * time.Minute)}
	m.mu.Unlock()
	down, rate, earliest := m.CooldownState(targets, now)
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
	down, rate, earliest = m.CooldownState(targets, now)
	if !down || rate || !earliest.Equal(now.Add(15*time.Minute)) {
		t.Fatalf("mixed cooldown = %v %v %v", down, rate, earliest)
	}

	m.mu.Lock()
	m.health["a"] = &providerHealth{circuitOpenUntil: now.Add(-time.Minute)}
	m.health["b"] = &providerHealth{circuitOpenUntil: now.Add(time.Minute)}
	m.mu.Unlock()
	if down, rate, earliest = m.CooldownState(targets, now); down || rate || !earliest.IsZero() {
		t.Fatalf("recovered provider did not short-circuit cooldown: %v %v %v", down, rate, earliest)
	}
	if !m.HasRecoveredUntried(targets, map[string]bool{"b": true}, now) {
		t.Fatal("untried recovered provider was not detected")
	}
	if m.HasRecoveredUntried(targets, map[string]bool{"a": true, "b": true}, now) {
		t.Fatal("fully tried target set reported a recovered untried provider")
	}
	m.mu.Lock()
	m.health["a"].halfOpenInFlight = true
	m.mu.Unlock()
	if m.HasRecoveredUntried(targets, map[string]bool{}, now) {
		t.Fatal("busy/open targets reported a recovered untried provider")
	}
}
