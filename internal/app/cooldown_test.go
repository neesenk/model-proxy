package app

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

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
	// Make "a" cooling without invoking the quota refresh side effect.
	seedRuntimeRateLimit(t, p, "a", time.Now().Add(time.Hour), rlTransient)

	routeKeys := map[string]bool{"m": true}
	targets := cfg.Routes["m"]
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	res := p.serveOnce(serveRequest{
		runtime: RuntimeSnapshot{
			Cfg: cfg, Providers: p.providers, PoolIndex: p.poolIndex,
			ParentOf: p.parentOf, ExpandedRoutes: p.expandedRoutes,
		},
		proto: "openai", upPath: "/chat/completions", exposed: "m", calledModel: "m",
		targets: targets, routeKeys: routeKeys, writer: httptest.NewRecorder(),
		request: r, requestID: "req-1",
	}, &serveState{})

	if len(res.effectiveTargets) != 1 || res.effectiveTargets[0].Provider != "b" {
		t.Errorf("effectiveTargets=%+v, want [{b}] (schedule dropped cooling a)", res.effectiveTargets)
	}
}
