package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// implicit_routes_test.go covers synthesizeImplicitRoutesFrom (login-aware
// auto-routing for models not in routes). login status is injected directly so
// the pure core is tested without credential files.

func TestSynthesizeImplicitRoutes_SingleProviderAutoRoute(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{
			"zhipu": {Provider: "zhipu", Models: []string{"glm-4.6", "glm-5.2"}},
		},
		Routes: map[string][]RouteTarget{
			"glm-5.2": {{Provider: "zhipu", Model: "glm-5.2", Priority: 1}}, // explicit
		},
	}
	implicit, warnings := synthesizeImplicitRoutesFrom(cfg, map[string]bool{"zhipu": true})

	// glm-4.6: not routed, single logged-in provider → implicit route, no warning.
	got, ok := implicit["glm-4.6"]
	if !ok {
		t.Fatal("glm-4.6 should get an implicit route")
	}
	if got.Provider != "zhipu" || got.Model != "glm-4.6" {
		t.Errorf("implicit glm-4.6 = %+v, want provider=zhipu model=glm-4.6", got)
	}
	// glm-5.2 is explicitly routed → no implicit.
	if _, dup := implicit["glm-5.2"]; dup {
		t.Error("explicitly-routed glm-5.2 should not get an implicit route")
	}
	if len(warnings) != 0 {
		t.Errorf("single-provider implicit route should not warn: %v", warnings)
	}
}

func TestSynthesizeImplicitRoutes_MultipleProvidersWarnsAndPicksFirst(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{
			"aqp":   {Provider: "aqp", Models: []string{"foo"}}, // alphabetically first
			"zhipu": {Provider: "zhipu", Models: []string{"foo"}},
		},
	}
	implicit, warnings := synthesizeImplicitRoutesFrom(cfg, map[string]bool{"aqp": true, "zhipu": true})

	got, ok := implicit["foo"]
	if !ok || got.Provider != "aqp" {
		t.Errorf("foo should auto-route to alphabetically-first 'aqp', got %+v ok=%v", got, ok)
	}
	if len(warnings) != 1 {
		t.Fatalf("want 1 ambiguity warning, got %d: %v", len(warnings), warnings)
	}
	w := warnings[0]
	if !strings.Contains(w, "foo") || !strings.Contains(w, "aqp") || !strings.Contains(w, "zhipu") {
		t.Errorf("warning should name model + both providers: %q", w)
	}
}

func TestSynthesizeImplicitRoutes_SkipsNotLoggedIn(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{
			"zhipu": {Provider: "zhipu", Models: []string{"glm-4.6"}},
		},
	}
	// zhipu NOT logged in → no implicit route, no warning.
	implicit, warnings := synthesizeImplicitRoutesFrom(cfg, map[string]bool{})
	if len(implicit) != 0 {
		t.Errorf("no logged-in providers → no implicit routes, got %v", implicit)
	}
	if len(warnings) != 0 {
		t.Errorf("no logged-in providers → no warnings, got %v", warnings)
	}
}

func TestSynthesizeImplicitRoutes_PrefersLoggedInAmongMultiple(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{
			"aqp":   {Provider: "aqp", Models: []string{"foo"}},   // not logged in
			"zhipu": {Provider: "zhipu", Models: []string{"foo"}}, // logged in
		},
	}
	// Only zhipu logged in → route to zhipu, single candidate → no warning.
	implicit, warnings := synthesizeImplicitRoutesFrom(cfg, map[string]bool{"zhipu": true})
	got, ok := implicit["foo"]
	if !ok || got.Provider != "zhipu" {
		t.Errorf("foo should route to the only logged-in provider zhipu, got %+v ok=%v", got, ok)
	}
	if len(warnings) != 0 {
		t.Errorf("single logged-in candidate → no warning, got %v", warnings)
	}
}

// TestImplicitRoute_ForwardsUnroutedLoggedInModel: an end-to-end check that a
// model NOT in routes but in a logged-in provider's models list is forwarded
// (was 502 before implicit routes). Uses a zhipu apikey provider + a mock
// upstream /chat/completions, with a real cred file under a temp HOME.
func TestImplicitRoute_ForwardsUnroutedLoggedInModel(t *testing.T) {
	// upstream records the model name it receives.
	var gotModel string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotModel = extractModel(b)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"x","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hi"}}]}`))
	}))
	defer up.Close()

	home := t.TempDir()
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	os.WriteFile(filepath.Join(credDir, "zhipu_apikey.json"), []byte(`{"api_key":"k"}`), 0o600)
	prev := os.Getenv("HOME")
	os.Setenv("HOME", home)
	defer os.Setenv("HOME", prev)

	cfg := &Config{
		Listen: "127.0.0.1:0",
		Providers: map[string]Provider{
			"zhipu": {Provider: "zhipu", OpenAIBaseURL: up.URL, Models: []string{"glm-5.2", "glm-4.6"}},
		},
		Routes: map[string][]RouteTarget{
			"glm-5.2": {{Provider: "zhipu", Model: "glm-5.2", Priority: 1}}, // explicit; glm-4.6 is NOT routed
		},
	}
	p := newTestProxy(t, cfg)
	// glm-4.6 should have been auto-routed to zhipu.
	if _, ok := p.implicitRoutes["glm-4.6"]; !ok {
		t.Fatalf("expected implicit route for glm-4.6, got implicit=%v", p.implicitRoutes)
	}

	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()
	resp, err := http.Post(px.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"glm-4.6","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("glm-4.6 (implicit route) status=%d want 200", resp.StatusCode)
	}
	if gotModel != "glm-4.6" {
		t.Errorf("upstream received model=%q want glm-4.6", gotModel)
	}

	// scheduleStatus should list the implicit route.
	st := string(p.scheduleStatus())
	if !strings.Contains(st, "glm-4.6") {
		t.Errorf("scheduleStatus should list implicit route glm-4.6: %s", st)
	}
}

// TestTakeover_IncludesImplicitRoutes: a model served only via an implicit
// route must appear in the opencode takeover config (parity with /v1/models).
func TestTakeover_IncludesImplicitRoutes(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Providers: map[string]Provider{
			"zhipu": {Provider: "zhipu", OpenAIBaseURL: "http://x", Models: []string{"glm-5.2", "glm-4.6"}},
		},
		Routes: map[string][]RouteTarget{
			"glm-5.2": {{Provider: "zhipu", Model: "glm-5.2", Priority: 1}}, // explicit; glm-4.6 implicit-only
		},
		Takeover: Takeover{Opencode: filepath.Join(dir, "oc.json"), ProxyURL: "http://x", ProviderID: "model-proxy"},
	}
	os.WriteFile(cfg.Takeover.Opencode, []byte(`{}`), 0o644)
	implicit := map[string]RouteTarget{"glm-4.6": {Provider: "zhipu", Model: "glm-4.6", Priority: 1}}
	if err := rewriteOpencode(cfg, nil, implicit); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(cfg.Takeover.Opencode)
	if !strings.Contains(string(b), "glm-4.6") {
		t.Errorf("opencode config should include implicit-route model glm-4.6:\n%s", b)
	}
}

// TestImplicitRoute_ListedInV1Models: implicitly-routable models appear in
// GET /v1/models so clients can discover them.
func TestImplicitRoute_ListedInV1Models(t *testing.T) {
	home := t.TempDir()
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	os.WriteFile(filepath.Join(credDir, "zhipu_apikey.json"), []byte(`{"api_key":"k"}`), 0o600)
	prev := os.Getenv("HOME")
	os.Setenv("HOME", home)
	defer os.Setenv("HOME", prev)

	cfg := &Config{
		Listen: "127.0.0.1:0",
		Providers: map[string]Provider{
			"zhipu": {Provider: "zhipu", OpenAIBaseURL: "http://x", Models: []string{"glm-5.2", "glm-4.6"}},
		},
		Routes: map[string][]RouteTarget{
			"glm-5.2": {{Provider: "zhipu", Model: "glm-5.2", Priority: 1}},
		},
	}
	p := newTestProxy(t, cfg)
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()
	resp, err := http.Get(px.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "glm-4.6") {
		t.Errorf("/v1/models should list implicit route glm-4.6: %s", body)
	}
	if !strings.Contains(string(body), "glm-5.2") {
		t.Errorf("/v1/models should still list explicit glm-5.2: %s", body)
	}
}
