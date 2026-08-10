package app

import (
	"testing"
	"time"

	runtimestate "model-proxy/internal/runtime"
)

// The helpers in this file keep root integration tests on the public runtime
// boundary. Pure state-machine details belong in internal/runtime tests.

type rateLimitKind = runtimestate.RateLimitKind

const (
	rlTransient = runtimestate.Transient
	rlQuota     = runtimestate.Quota
	rlDaily     = runtimestate.Daily
)

func runtimeStatus(
	t testing.TB,
	p *Proxy,
	providerName string,
	now time.Time,
) runtimestate.ProviderStatus {
	t.Helper()
	status, ok := p.runtimeState.Dashboard(now).Providers[providerName]
	if !ok {
		t.Fatalf("runtime status for %q is missing", providerName)
	}
	return status
}

func seedRuntimeRateLimit(
	t testing.TB,
	p *Proxy,
	providerName string,
	until time.Time,
	kind rateLimitKind,
) {
	t.Helper()
	if !p.runtimeState.RecordRateLimit(providerName, until, kind, 0) {
		t.Fatalf("seed rate limit for %q was rejected", providerName)
	}
}

func seedRuntimeCircuit(
	t testing.TB,
	p *Proxy,
	providerName string,
	until time.Time,
) {
	t.Helper()
	// RecordFailure opens the circuit with the Manager's own cooldown window;
	// the helper's `until` only documents intent, matching the adapter path.
	p.runtimeState.RecordFailure(
		providerName,
		1,
		time.Hour,
		0,
	)
}

func seedRuntimeHalfOpen(t testing.TB, p *Proxy, providerName string) {
	t.Helper()
	seedRuntimeCircuit(t, p, providerName, time.Now().Add(-time.Second))
	if !p.runtimeState.TakeHalfOpenSlot(providerName, 0) {
		t.Fatalf("seed half-open slot for %q was rejected", providerName)
	}
}

func seedRuntimeSticky(
	t testing.TB,
	p *Proxy,
	key, providerName string,
	since time.Time,
) {
	t.Helper()
	if !p.runtimeState.SetSticky(
		key,
		runtimestate.Sticky{Provider: providerName, Since: since},
		0,
	) {
		t.Fatalf("seed sticky %q was rejected", key)
	}
}

func runtimeSticky(t testing.TB, p *Proxy, key string) runtimestate.Sticky {
	t.Helper()
	sticky, ok := p.runtimeState.Sticky(key)
	if !ok {
		t.Fatalf("runtime sticky %q is missing", key)
	}
	return sticky
}
