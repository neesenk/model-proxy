package app

import (
	"model-proxy/internal/catalog"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/provider"
	"model-proxy/internal/targetexec"
	"net/http"
	"testing"
	"time"
)

// ---- target_attempt_test.go ----

// TestNewTargetAttemptOnlyGroupsPreparedInputs moved to internal/forward
// (attempt_test.go) with the newTargetAttempt factory it exercises.

// TestCommittedTTFTStaleGenerationDropped (A3 regression): a committed 2xx
// from a PRE-RELOAD in-flight request must not fold its TTFT into the new
// generation's quality EWMA. Pre-fix Committed called recordAttemptQuality
// without a generation, so GenerationArg(nil)=0 made the gate always-true and
// stale TTFT samples polluted the post-reload quality map (the error-rate
// samples were already gated — only this path leaked).
func TestCommittedTTFTStaleGenerationDropped(t *testing.T) {
	p := newTestProxy(t, &configdomain.Config{
		Providers: map[string]configdomain.Provider{},
		Routes:    map[string][]configdomain.RouteTarget{},
	})

	committed := targetexec.AttemptDTO{
		Target:           configdomain.RouteTarget{Provider: "p", Model: "m"},
		Response:         &http.Response{StatusCode: http.StatusOK},
		TTFTMilliseconds: 500,
	}

	// The request started under generation 1 (the constructor's generation);
	// a reload moves the manager to generation 2 while it is in flight.
	stale := targetExecutionEffects{proxy: p, generation: 1}
	p.runtimeState.ReplaceGeneration(2, nil)
	stale.Committed(committed)
	if q := p.runtimeState.Dashboard(time.Now()).Quality; len(q) != 0 {
		t.Fatalf("stale-generation TTFT wrote into the new quality map: %+v", q)
	}

	// Control: a commit on the CURRENT generation lands (decayed by the tiny
	// record→dashboard interval, so assert a tight window around 500ms).
	fresh := targetExecutionEffects{proxy: p, generation: 2}
	fresh.Committed(committed)
	if got := p.runtimeState.Dashboard(time.Now()).Quality["p"].TTFTMilliseconds; got < 490 || got > 500 {
		t.Fatalf("current-generation TTFT = %dms, want ~500ms recorded", got)
	}
}

// ---- dispatch_context_test.go ----

func TestRuntimeSnapshotKeepsOneReloadGeneration(t *testing.T) {
	p := newTestProxy(t, &configdomain.Config{
		Providers: map[string]configdomain.Provider{"old": {Provider: testProviderID}},
		Routes:    map[string][]configdomain.RouteTarget{"m": {{Provider: "old", Model: "old-model"}}},
	})
	p.catalog = catalog.New(nil)

	snapshot := p.SnapshotRuntime()
	if snapshot.Cfg != p.cfg ||
		snapshot.Generation != p.configGeneration.Load() ||
		snapshot.Providers["old"] == nil ||
		len(snapshot.ExpandedRoutes["m"]) != 1 ||
		snapshot.Catalog != p.catalog {
		t.Fatalf("incomplete runtime snapshot: %+v", snapshot)
	}

	newCfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{"new": {Provider: testProviderID}},
		Routes:    map[string][]configdomain.RouteTarget{"m": {{Provider: "new", Model: "new-model"}}},
	}
	p.mu.Lock()
	p.cfg = newCfg
	p.providers = map[string]provider.Provider{}
	p.expandedRoutes = newCfg.Routes
	p.configGeneration.Add(1)
	p.mu.Unlock()

	if snapshot.Cfg.Providers["old"].Provider != testProviderID ||
		snapshot.ExpandedRoutes["m"][0].Provider != "old" ||
		snapshot.Generation == p.configGeneration.Load() {
		t.Fatalf("captured snapshot changed across reload swap: %+v", snapshot)
	}
}
