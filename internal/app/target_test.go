package app

import (
	"fmt"
	"model-proxy/internal/catalog"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/forward"
	"model-proxy/internal/provider"
	"model-proxy/internal/providerbuild"
	runtimewire "model-proxy/internal/runtime/wirecap"
	"model-proxy/internal/targetexec"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTargetPlanKeepsProtocolAcrossReload(t *testing.T) {
	useStaticProviderPools(t, "p")
	oldUp := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(oldUp.Close)
	newUp := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(newUp.Close)
	prov := configdomain.Provider{Provider: "static", OpenAIBaseURL: oldUp.URL, Models: []string{"m"}}
	next := prov
	next.OpenAIBaseURL = newUp.URL
	dir := t.TempDir()
	state := filepath.Join(dir, "quota_state.json")
	p := newTestProxyAt(t, &configdomain.Config{Proxy: "off",
		Providers: map[string]configdomain.Provider{"p": prov},
		Routes:    map[string][]configdomain.RouteTarget{"route": {{Provider: "p", Model: "m"}}},
	}, state)
	chat := runtimewire.ModelProtocols{Chat: triYes, Responses: triNo, Anthropic: triNo}
	responses := runtimewire.ModelProtocols{Chat: triNo, Responses: triYes, Anthropic: triNo}
	p.modelCaps.Put("p", providerbuild.ProtocolConfigFingerprint(prov), "m", chat, time.Now())
	p.setWireCaps("p", wireCaps{BaseURL: oldUp.URL, Chat: triYes, Responses: triNo})
	old := p.SnapshotRuntime()
	targets := old.ExpandedRoutes["route"]
	if len(targets) != 1 {
		t.Fatalf("expanded route has %d targets, want one", len(targets))
	}
	check := func(snap RuntimeSnapshot, model, base, proto string) {
		t.Helper()
		target := targets[0]
		target.Model = model
		plan, err := forward.PlanTarget(forward.PlanInput{Runtime: snap, Target: target, ClientProto: "anthropic", ClientPath: "/v1/messages"})
		if err != nil {
			t.Fatal(err)
		}
		if plan.BaseURL() != base || string(plan.BackendProtocol()) != proto {
			t.Errorf("generation %d model %q: base %q protocol %q, want %q %q", snap.Generation, model, plan.BaseURL(), plan.BackendProtocol(), base, proto)
		}
	}
	check(old, "m", oldUp.URL, "openai")
	check(old, "", oldUp.URL, "openai")
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(fmt.Sprintf("proxy: off\nproviders:\n  p: {provider_id: static, openai_base_url: %s, models: [m]}\n", newUp.URL)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runtimewire.SaveModelCapsFile(runtimewire.ModelCapsPath(state), map[string]runtimewire.ProviderModelCaps{"p": {
		Fingerprint: providerbuild.ProtocolConfigFingerprint(next), Models: map[string]runtimewire.ModelProtocols{"m": responses},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := p.Reload(cfgPath); err != nil {
		t.Fatal(err)
	}
	fresh := p.SnapshotRuntime()
	if fresh.Generation == old.Generation {
		t.Fatal("reload did not advance the generation")
	}
	check(fresh, "m", newUp.URL, "responses")
	check(fresh, "", newUp.URL, "anthropic")
	p.setWireCaps("p", wireCaps{BaseURL: newUp.URL, Responses: triYes, Chat: triNo})
	check(p.SnapshotRuntime(), "", newUp.URL, "responses")
	check(fresh, "", newUp.URL, "anthropic")
	check(old, "m", oldUp.URL, "openai")
	check(old, "", oldUp.URL, "openai")
	// A late passthrough 404 at the old endpoint must leave the current verdict intact.
	proxyHealthGate{proxy: p, snapshot: old}.NoteWireResponsesMiss(targets[0].Provider, "")
	check(p.SnapshotRuntime(), "", newUp.URL, "responses")
}

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
