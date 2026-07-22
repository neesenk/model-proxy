package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestHasRecoveredUntried_AllRecoveredSimultaneously (bug 4, claim 2): when
// every untried target recovers at the same moment before the terminal check
// (none is STILL cooling), the old anyCooling precondition made
// hasRecoveredUntried return false → a terminal 502 even though a zero-wait
// re-schedule would have succeeded. Dropping anyCooling (relying on the round
// budget to bound the loop) lets that re-schedule happen.
func TestHasRecoveredUntried_AllRecoveredSimultaneously(t *testing.T) {
	p := newQuotaProxy(t,
		map[string]Provider{"a": {}, "b": {}},
		map[string][]RouteTarget{"m": {{Provider: "a"}, {Provider: "b"}}})
	targets := []RouteTarget{{Provider: "a"}, {Provider: "b"}}
	now := time.Now()

	// No health entries → both available (none cooling). None tried this pass.
	if got := p.hasRecoveredUntried(targets, map[string]bool{}, now); !got {
		t.Errorf("all recovered, none tried: hasRecoveredUntried=false, want true (claim 2: must re-schedule, not terminally fail)")
	}
	// Everything tried → nothing recovered-untried.
	if got := p.hasRecoveredUntried(targets, map[string]bool{"a": true, "b": true}, now); got {
		t.Errorf("all tried: hasRecoveredUntried=true, want false")
	}
}

// TestServeOnce_EffectiveTargetsReflectsScheduledSet (bug 4, claim 1): serveOnce
// must return the EFFECTIVE target set it actually used (after scheduling drops
// cooling targets), so forward's cooldown/TOCTOU decisions key on what was
// really considered — not the original route targets. Here target "a" is
// rate-limited, so schedule narrows to ["b"]; effectiveTargets must be ["b"].
func TestServeOnce_EffectiveTargetsReflectsScheduledSet(t *testing.T) {
	p := newQuotaProxy(t,
		map[string]Provider{"a": {}, "b": {}},
		map[string][]RouteTarget{"m": {{Provider: "a"}, {Provider: "b"}}})
	cfg := p.cfg
	// Make "a" cooling (rate-limited) by setting its health directly — avoids
	// recordRateLimit, which spawns refreshOne and needs a fully-initialized
	// tracker that newQuotaProxy doesn't build.
	p.healthMu.Lock()
	p.health["a"] = &providerHealth{rateLimitedUntil: time.Now().Add(time.Hour)}
	p.healthMu.Unlock()

	routeKeys := map[string]bool{"m": true}
	targets := cfg.Routes["m"]
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	res := p.serveOnce(cfg, 0, p.providers, p.poolIndex, p.parentOf, p.expandedRoutes, nil,
		"openai", "/chat/completions", "m", "m", "", targets, routeKeys, false, nil, "",
		httptest.NewRecorder(), r, "", "req-1", nil, &serveState{})

	if len(res.effectiveTargets) != 1 || res.effectiveTargets[0].Provider != "b" {
		t.Errorf("effectiveTargets=%+v, want [{b}] (schedule dropped cooling a)", res.effectiveTargets)
	}
}
