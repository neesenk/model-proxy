package app

import (
	"fmt"
	"io"
	"model-proxy/internal/protocol"
	"model-proxy/internal/takeover"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// implicit_routes_test.go covers DeriveRoutesFrom / RouteTable /
// BuildExpandedRoutes (config-only route derivation: per-provider priority,
// model aliases, aggregation by exposed name, explicit-route override).

func TestDeriveRoutesFrom_AggregatesByExposedNameWithProviderPriority(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{
			"kimi-code":  {Provider: "kimi-code", Models: []string{"k3", "kimi-for-coding"}, Priority: 1, Alias: map[string]string{"k3": "kimi-k3"}},
			"aqp":        {Provider: "aqp", Models: []string{"kimi-k3", "glm-5.3"}, Priority: 2},
			"volcengine": {Provider: "volcengine", Models: []string{"kimi-k3"}, Priority: 3},
		},
	}
	derived := DeriveRoutesFrom(cfg)

	// kimi-k3 aggregates three providers (kimi-code under its k3 alias); each
	// target keeps the REAL upstream model name and inherits its provider's
	// priority; ordering is (priority, provider).
	want := []RouteTarget{
		{Provider: "kimi-code", Model: "k3", Priority: 1},
		{Provider: "aqp", Model: "kimi-k3", Priority: 2},
		{Provider: "volcengine", Model: "kimi-k3", Priority: 3},
	}
	if got := derived["kimi-k3"]; !reflect.DeepEqual(got, want) {
		t.Errorf("derived kimi-k3 = %+v, want %+v", got, want)
	}
	// Non-aliased models keep their own name.
	if got := derived["glm-5.3"]; len(got) != 1 || got[0].Provider != "aqp" || got[0].Model != "glm-5.3" {
		t.Errorf("derived glm-5.3 = %+v, want single aqp target", got)
	}
	if _, aliased := derived["k3"]; aliased {
		t.Error("aliased model k3 must not stay exposed under its real name")
	}
	if _, ok := derived["kimi-for-coding"]; !ok {
		t.Error("kimi-for-coding should be derived under its own name")
	}
}

func TestRouteTable_ExplicitRouteOverridesDerived(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{
			"aqp":   {Provider: "aqp", Models: []string{"foo"}, Priority: 2},
			"zhipu": {Provider: "zhipu", Models: []string{"foo", "bar"}, Priority: 1},
		},
		Routes: map[string][]RouteTarget{
			// Explicit override wins wholesale for this name.
			"foo": {{Provider: "zhipu", Model: "foo"}},
			// An explicit name with no derived counterpart is kept as-is.
			"hard": {{Provider: "fusion", Model: "hard-coding"}},
		},
	}
	table := RouteTable(cfg)
	if got := table["foo"]; len(got) != 1 || got[0].Provider != "zhipu" {
		t.Errorf("explicit foo route should override the derived aggregation, got %+v", got)
	}
	// Explicit targets without their own priority inherit the provider's.
	if got := table["foo"]; len(got) != 1 || got[0].Priority != 1 {
		t.Errorf("explicit foo target should inherit zhipu priority 1, got %+v", got)
	}
	if got := table["bar"]; len(got) != 1 || got[0].Provider != "zhipu" || got[0].Priority != 1 {
		t.Errorf("derived bar = %+v, want single zhipu target with priority 1", got)
	}
	if got := table["hard"]; len(got) != 1 || got[0].Provider != "fusion" || got[0].Model != "hard-coding" {
		t.Errorf("explicit fusion route = %+v", got)
	}
}

func TestBuildExpandedRoutes_FansOutDerivedAndExplicit(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{
			"aqp":   {Provider: "aqp", Models: []string{"m1"}, Priority: 2},
			"zhipu": {Provider: "zhipu", Models: []string{"m1"}, Priority: 1},
		},
		Routes: map[string][]RouteTarget{
			"m2": {{Provider: "aqp", Model: "m1"}},
		},
	}
	expand := func(t RouteTarget) []RouteTarget {
		if t.Provider == "zhipu" {
			return []RouteTarget{{Provider: "zhipu", Model: t.Model, Priority: t.Priority}, {Provider: "zhipu#2", Model: t.Model, Priority: t.Priority}}
		}
		return []RouteTarget{t}
	}
	out := BuildExpandedRoutes(cfg, DeriveRoutesFrom(cfg), expand)
	if got := out["m1"]; len(got) != 3 {
		t.Errorf("derived m1 should fan out to 3 targets, got %+v", got)
	}
	if got := out["m2"]; len(got) != 1 || got[0].Provider != "aqp" || got[0].Priority != 2 {
		t.Errorf("explicit m2 = %+v, want aqp target with inherited priority 2", got)
	}
}

// TestDerivedRoute_ForwardsAliasedModel end-to-end: a model exposed under an
// alias is forwarded under its REAL upstream name.
func TestDerivedRoute_ForwardsAliasedModel(t *testing.T) {
	var gotModel string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotModel = protocol.ExtractModel(b)
		w.Header().Set("content-type", "application/json")
		fmt.Fprint(w, `{"id":"x","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
	}))
	defer up.Close()

	home := t.TempDir()
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	os.WriteFile(filepath.Join(credDir, "zhipu_apikey.json"), []byte(`{"api_key":"k"}`), 0o600)
	t.Setenv("HOME", home)

	cfg := &Config{
		Listen: "127.0.0.1:0",
		Providers: map[string]Provider{
			"zhipu": {Provider: "zhipu", OpenAIBaseURL: up.URL, Models: []string{"k3"}, Priority: 1, Alias: map[string]string{"k3": "kimi-k3"}},
		},
	}
	p := newTestProxy(t, cfg)
	if _, ok := p.derivedRoutes["kimi-k3"]; !ok {
		t.Fatalf("expected derived route for kimi-k3, got derived=%v", p.derivedRoutes)
	}

	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()
	resp, err := http.Post(px.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"kimi-k3","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("kimi-k3 (derived route) status=%d want 200", resp.StatusCode)
	}
	if gotModel != "k3" {
		t.Errorf("upstream received model=%q want real name k3", gotModel)
	}

	if st := string(p.scheduleStatus()); !strings.Contains(st, "kimi-k3") {
		t.Errorf("scheduleStatus should list derived route kimi-k3: %s", st)
	}
}

// TestTakeover_IncludesDerivedRoutes: a model exposed only via a derived route
// must appear in the opencode takeover config (parity with /v1/models).
func TestTakeover_IncludesDerivedRoutes(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{
		Providers: map[string]Provider{
			"zhipu": {Provider: "zhipu", OpenAIBaseURL: "http://x", Models: []string{"glm-4.6"}},
		},
		Takeover: Takeover{Opencode: filepath.Join(dir, "oc.json"), ProxyURL: "http://x", ProviderID: "model-proxy"},
	}
	os.WriteFile(cfg.Takeover.Opencode, []byte(`{}`), 0o644)
	routes := RouteTable(cfg)
	if _, ok := routes["glm-4.6"]; !ok {
		t.Fatalf("route table should include glm-4.6: %v", routes)
	}
	if err := takeover.RewriteOpencode(cfg, nil, routes); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(cfg.Takeover.Opencode)
	if !strings.Contains(string(b), "glm-4.6") {
		t.Errorf("opencode config should include derived-route model glm-4.6:\n%s", b)
	}
}

// TestDerivedRoute_ListedInV1Models: derived models appear in GET /v1/models
// so clients can discover them.
func TestDerivedRoute_ListedInV1Models(t *testing.T) {
	home := t.TempDir()
	credDir := filepath.Join(home, ".model-proxy")
	os.MkdirAll(credDir, 0o700)
	os.WriteFile(filepath.Join(credDir, "zhipu_apikey.json"), []byte(`{"api_key":"k"}`), 0o600)
	t.Setenv("HOME", home)

	cfg := &Config{
		Listen: "127.0.0.1:0",
		Providers: map[string]Provider{
			"zhipu": {Provider: "zhipu", OpenAIBaseURL: "http://x", Models: []string{"glm-5.2", "glm-4.6"}, Alias: map[string]string{"glm-5.2": "glm-main"}},
		},
		Routes: map[string][]RouteTarget{
			"glm-explicit": {{Provider: "zhipu", Model: "glm-5.2"}},
		},
	}
	p := newTestProxy(t, cfg)
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()
	resp, err := http.Get(px.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	for _, id := range []string{"glm-main", "glm-4.6", "glm-explicit"} {
		if !strings.Contains(string(body), fmt.Sprintf("%q", id)) {
			t.Errorf("/v1/models should list %s: %s", id, body)
		}
	}
	if strings.Contains(string(body), `"glm-5.2"`) {
		t.Errorf("/v1/models should not list aliased-away glm-5.2: %s", body)
	}
}
