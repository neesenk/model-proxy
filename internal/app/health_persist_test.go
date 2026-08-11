package app

import (
	"path/filepath"
	"testing"
	"time"

	runtimestate "model-proxy/internal/runtime"
)

// TestHealthPersist_RoundTrip: frozen health state (rate-limit + kind, circuit,
// model locks, param blocklist) persists to quota_state.json and is restored
// by a fresh Proxy; expired cooldowns are dropped.
func TestHealthPersist_RoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := &Config{Providers: map[string]Provider{"a": {OpenAIBaseURL: "http://x", Provider: testProviderID}}}
	statePath := filepath.Join(t.TempDir(), "quota_state.json")

	p1 := newTestProxyAt(t, cfg, statePath)
	now := time.Now()
	rateLimitUntil := now.Add(2 * time.Hour)
	p1.recordRateLimit("a", rateLimitUntil, rlQuota)
	modelLockBefore := time.Now().Add(time.Hour)
	p1.recordModelFailure("a", "m1", Scheduling{ModelLockout: "1h"})
	modelLockAfter := time.Now().Add(time.Hour)
	p1.learnParamBlock("a", "m1", "max_tokens")
	// Circuit on a second provider + an already-expired rate limit (must drop).
	circuitUntil := now.Add(10 * time.Minute)
	p1.runtimeState.RestoreHealth(map[string]runtimestate.PersistedHealth{
		"b": {CircuitOpenUntil: circuitUntil},
		"c": {RateLimitedUntil: now.Add(-time.Minute)},
	}, now, cfg.Scheduling.Threshold())
	if err := p1.quota.Persist(); err != nil {
		t.Fatalf("persist source state: %v", err)
	}
	p1.Close()

	p2 := newTestProxyAt(t, cfg, statePath)
	runtimeSnapshot := p2.runtimeState.Dashboard(now)
	h, ok := runtimeSnapshot.Providers["a"]
	if !ok {
		t.Fatalf("provider a rate-limit not restored: %+v", runtimeSnapshot.Providers)
	}
	if !h.RateLimitedUntil.Equal(rateLimitUntil) {
		t.Errorf("rateLimitedUntil = %s, want %s", h.RateLimitedUntil, rateLimitUntil)
	}
	if h.RateLimitKind != rlQuota {
		t.Errorf("rateLimitKind = %v want rlQuota", h.RateLimitKind)
	}
	locks := runtimeSnapshot.ModelLocks["a"]
	if len(locks) != 1 || locks[0].Model != "m1" {
		t.Errorf("model lock (a,m1) not restored: %+v", locks)
	} else if locks[0].LockedUntil.Before(modelLockBefore) || locks[0].LockedUntil.After(modelLockAfter) {
		t.Errorf("model lock until %s outside [%s, %s]", locks[0].LockedUntil, modelLockBefore, modelLockAfter)
	}
	if !p2.runtimeState.ParamBlocked("a", "m1", "max_tokens") {
		t.Error("param blocklist for a not restored")
	}

	hb, ok := runtimeSnapshot.Providers["b"]
	if !ok {
		t.Fatalf("provider b circuit not restored: %+v", runtimeSnapshot.Providers)
	}
	if !hb.CircuitOpenUntil.Equal(circuitUntil) {
		t.Errorf("circuitOpenUntil = %s, want %s", hb.CircuitOpenUntil, circuitUntil)
	}
	if hb.ConsecutiveFailures != cfg.Scheduling.Threshold() {
		t.Errorf("restored circuit failures = %d, want threshold %d (next failure re-opens)",
			hb.ConsecutiveFailures, cfg.Scheduling.Threshold())
	}

	if _, ok := runtimeSnapshot.Providers["c"]; ok {
		t.Error("expired rate-limit on c must be dropped, not restored")
	}
}

// TestHealthPersist_FingerprintMismatch: health frozen under one config must
// NOT restore onto a different config that reuses the same provider names
// (test binaries share the state file; daemons restart with edited configs).
func TestHealthPersist_FingerprintMismatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfgA := &Config{Providers: map[string]Provider{"a": {OpenAIBaseURL: "http://x", Provider: testProviderID}}}
	statePath := filepath.Join(t.TempDir(), "quota_state.json")
	p1 := newTestProxyAt(t, cfgA, statePath)
	p1.recordRateLimit("a", time.Now().Add(2*time.Hour), rlQuota)
	if err := p1.quota.Persist(); err != nil {
		t.Fatalf("persist source state: %v", err)
	}
	p1.Close()

	// Same provider NAME, different upstream URL → different fingerprint.
	cfgB := &Config{Providers: map[string]Provider{"a": {OpenAIBaseURL: "http://y", Provider: testProviderID}}}
	p2 := newTestProxyAt(t, cfgB, statePath)
	_, frozen := p2.runtimeState.Dashboard(time.Now()).Providers["a"]
	if frozen {
		t.Error("health must not restore under a mismatched config fingerprint")
	}
}

// TestHealthPersist_ParamBlockOnlyProvider: a provider with ONLY learned params
// (never failed → no health entry) must still persist + restore its blocklist.
func TestHealthPersist_ParamBlockOnlyProvider(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := &Config{Providers: map[string]Provider{"a": {OpenAIBaseURL: "http://x", Provider: testProviderID}}}
	statePath := filepath.Join(t.TempDir(), "quota_state.json")
	p1 := newTestProxyAt(t, cfg, statePath)
	p1.learnParamBlock("a", "m1", "max_tokens")
	if err := p1.quota.Persist(); err != nil {
		t.Fatalf("persist source state: %v", err)
	}
	p1.Close()

	p2 := newTestProxyAt(t, cfg, statePath)
	blocked := p2.runtimeState.ParamBlocked("a", "m1", "max_tokens")
	_, hasHealth := p2.runtimeState.Dashboard(time.Now()).Providers["a"]
	if !blocked {
		t.Error("param blocklist must persist even without a health entry")
	}
	if hasHealth {
		t.Error("no health entry should be synthesized for a param-only provider")
	}
}
