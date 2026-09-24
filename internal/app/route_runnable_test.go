// route_runnable_test.go — the effective route table (expandedRoutes) only
// lists targets the forward path could actually serve: a provider whose
// credential pool is an authoritative 0-account tombstone (e.g. zcode after
// logout) has no impl, and its targets are dropped at expansion (schedule
// chains, /v1/models, forward all read the effective table). Adding the
// account (login → reload rebuilds the table) brings the target back. The
// fusion orchestration pseudo-provider is exempt — forward intercepts it
// before the impl lookup.
package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	configdomain "model-proxy/internal/config"
)

// rebuildRoutes mirrors the reload swap for tests that change provider impls
// after construction: expandedRoutes/routeKeys are generation-owned and must
// be rebuilt under the write lock, exactly like Reload does.
func rebuildRoutes(t *testing.T, p *Proxy) {
	t.Helper()
	p.mu.Lock()
	p.expandedRoutes = p.buildExpandedRoutes(authNotReady(p.providers))
	p.routeKeys = routeKeySet(p.expandedRoutes)
	p.mu.Unlock()
}

func runnableTestConfig() *configdomain.Config {
	return &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			// live builds an auth-only impl from config alone (missing-file
			// branch); ghost's pool file is an empty plural tombstone, so
			// buildProviders disables it — the zcode shape.
			"live":  {OpenAIBaseURL: "http://live.example", Provider: testProviderID, Models: []string{"shared", "solo-live"}},
			"ghost": {OpenAIBaseURL: "http://ghost.example", Provider: testProviderID, Models: []string{"shared", "solo-ghost"}},
		},
		Scheduling: configdomain.Scheduling{CircuitThreshold: 3},
	}
}

// TestExpandedRoutesDropAccountlessProviders pins the effective-table rule:
// an impl-less provider's targets are excluded from shared routes, and a
// model served ONLY by impl-less providers leaves the table entirely
// (schedule listing, /v1/models, forward not-found). Rebuilding with an impl
// — the login → reload seam — restores the target.
func TestExpandedRoutesDropAccountlessProviders(t *testing.T) {
	setPoolHome(t, t.TempDir())
	// Zero keys = authoritative empty plural pool: buildProviders disables
	// the provider ("pool … exists but has 0 accounts"), same as zcode live.
	writePoolFile(t, "ghost", testProviderID)

	p := newTestProxy(t, runnableTestConfig())

	// Shared model: only the impl-backed provider remains in the chain.
	targets := p.expandedRoutes["shared"]
	if len(targets) != 1 || targets[0].Provider != "live" {
		t.Fatalf("shared chain = %v, want [live] only (ghost has no account)", targets)
	}

	// Ghost-only model: gone from the effective table entirely.
	if _, ok := p.expandedRoutes["solo-ghost"]; ok {
		t.Fatal("solo-ghost still routed: an unservable model must leave the effective table")
	}

	// End-to-end surfaces agree: /v1/models hides it, schedule omits it,
	// forward answers the not-found terminal.
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()
	ids := exposedModelIDs(t, px)
	if contains(ids, "solo-ghost") {
		t.Fatalf("GET /v1/models = %v, want solo-ghost hidden", ids)
	}
	if !contains(ids, "solo-live") {
		t.Fatal("GET /v1/models lost the servable solo-live model")
	}
	var schedule struct {
		Models map[string]any `json:"models"`
	}
	if err := json.Unmarshal(p.scheduleStatus(), &schedule); err != nil {
		t.Fatalf("parse scheduleStatus: %v", err)
	}
	if _, ok := schedule.Models["solo-ghost"]; ok {
		t.Fatal("schedule status still lists the unservable solo-ghost route")
	}
	if _, ok := schedule.Models["shared"]; !ok {
		t.Fatal("schedule status lost the servable shared route")
	}
	resp, err := http.Post(px.URL+"/v1/chat/completions", "application/json",
		stringReader(`{"model":"solo-ghost","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("client post: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("unservable model status = %d, want 502 not-found", resp.StatusCode)
	}

	// The login seam: an impl for ghost (credential written → reload rebuild)
	// puts its targets back into both routes.
	p.providers["ghost"] = &testProv{key: "g"}
	rebuildRoutes(t, p)
	targets = p.expandedRoutes["shared"]
	if len(targets) != 2 {
		t.Fatalf("shared chain after ghost login = %v, want both providers", targets)
	}
	if ts := p.expandedRoutes["solo-ghost"]; len(ts) != 1 || ts[0].Provider != "ghost" {
		t.Fatalf("solo-ghost after login = %v, want [ghost]", ts)
	}
	if ids := exposedModelIDs(t, px); !contains(ids, "solo-ghost") {
		t.Fatalf("GET /v1/models = %v after login, want solo-ghost exposed again", ids)
	}
}

// TestExpandedRoutesKeepFusionPseudoProvider pins the exemption: "fusion" has
// no provider impl by design (forward intercepts the target before the impl
// lookup), so its route targets must survive the runnable filter.
func TestExpandedRoutesKeepFusionPseudoProvider(t *testing.T) {
	cfg := runnableTestConfig()
	cfg.Routes = map[string][]configdomain.RouteTarget{
		"panel": {{Provider: "fusion", Model: "recipe"}},
	}
	p := newTestProxy(t, cfg)
	targets := p.expandedRoutes["panel"]
	if len(targets) != 1 || targets[0].Provider != "fusion" || targets[0].Model != "recipe" {
		t.Fatalf("fusion route = %v, want the pseudo-provider target preserved", targets)
	}
}
