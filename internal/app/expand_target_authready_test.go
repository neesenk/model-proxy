package app

// expand_target_authready_test.go — pins the routing-eligibility contract
// ("无凭据的 provider 不进调度链、不进 GET /v1/models，登录后 reload 自动回归",
// README): a provider built file-backed WITHOUT any credential (configured but
// never logged in — opencode-go/zhipu before their first `login`) must not
// enter the effective routing table, while a credential-free provider
// (static-family) stays routable whenever built. Login (+ reload) restores the
// route — the AuthReady seam is consulted at build/reload time only.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	configdomain "model-proxy/internal/config"
)

// loginAPIKeyFixtures isolates HOME and writes 1-entry credential pools for
// every name→providerID pair, so construction-time AuthReady passes. The
// tests patched to use it were written against the OLD contract (unlogged
// providers entering the effective table); the routing-eligibility gate
// means they now need a credential fixture to exercise their mechanics.
func loginAPIKeyFixtures(t *testing.T, pairs ...[2]string) {
	t.Helper()
	setPoolHome(t, t.TempDir())
	for _, p := range pairs {
		writePoolFile(t, p[0], p[1], "sk-fixture-"+p[0])
	}
}

// loginOAuthFixture writes an OAuth store fixture (aqp SSO cookie / codex
// tokens) under an isolated HOME so construction-time AuthReady passes for
// OAuth providers, whose credential lives outside the pool namespace.
func loginOAuthFixture(t *testing.T, name, providerID string) {
	t.Helper()
	home := t.TempDir()
	setPoolHome(t, home)
	dir := filepath.Join(home, ".model-proxy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	var body string
	switch providerID {
	case "aqp":
		body = `{"account_id":"a","sso_session_cookie":"SSO_C=fixture"}`
	case "codex":
		body = `{"tokens":{"access_token":"at-fixture","refresh_token":"rt-fixture","id_token":"it-fixture","account_id":"acct"}}`
	default:
		t.Fatalf("no OAuth fixture shape for provider id %q", providerID)
	}
	if err := os.WriteFile(filepath.Join(dir, name+"_oauth_auth.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// authReadyTestConfig: "zhipu" is a REAL apikey provider id (its impl embeds
// ApiKeyBase → carries the AuthReady marker); "single" uses the static-family
// test id (no marker → routable whenever built).
func authReadyTestConfig(singleURL string) *configdomain.Config {
	return &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"zhipu":  {OpenAIBaseURL: "https://x", Provider: "zhipu"},
			"single": {OpenAIBaseURL: singleURL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
			// Mixed: credential-less zhipu dropped, static-family survives.
			"m": {
				{Provider: "zhipu", Model: "m", Priority: 1},
				{Provider: "single", Model: "m", Priority: 2},
			},
			// Only zhipu: the whole route leaves the effective table.
			"solo": {
				{Provider: "zhipu", Model: "solo", Priority: 1},
			},
		},
	}
}

// TestExpandTargetDropsCredentiallessProviders pins all three halves: the
// credential-less apikey provider's targets are dropped (mixed route keeps
// its other targets; solo route disappears entirely from the effective table
// and /v1/models), and the static-family target stays routable without any
// credential.
func TestExpandTargetDropsCredentiallessProviders(t *testing.T) {
	home := t.TempDir()
	setPoolHome(t, home) // no pool/singular files: zhipu is built but not AuthReady

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer up.Close()

	p := newTestProxy(t, authReadyTestConfig(up.URL))
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	if _, ok := p.providers["zhipu"]; !ok {
		t.Fatal("credential-less zhipu must still be BUILT (usage/models-refresh surfaces); only routing drops it")
	}
	if targets := p.expandedRoutes["m"]; len(targets) != 1 || targets[0].Provider != "single" {
		t.Fatalf("mixed route targets = %+v, want only [single] (credential-less zhipu dropped)", targets)
	}
	if _, ok := p.expandedRoutes["solo"]; ok {
		t.Fatal("solo route (only credential-less targets) must leave the effective table entirely")
	}
	if ids := exposedModelIDs(t, px); !contains(ids, "m") {
		t.Fatalf("GET /v1/models = %v, want m (static-family target serves it)", ids)
	}
	if ids := exposedModelIDs(t, px); contains(ids, "solo") {
		t.Fatalf("GET /v1/models = %v, want solo hidden (no servable target)", ids)
	}

	// Login (write plural pools) + reload: the dropped targets come back —
	// the documented "登录后 reload 自动回归". The reload path loads + validates
	// a config FILE, so this half uses real provider ids (test-static is a
	// test-only registry key): zhipu and deepseek, both with fresh pools.
	writePoolFile(t, "zhipu", "zhipu", "sk-1")
	writePoolFile(t, "deepseek", "deepseek", "sk-2")
	cfgYAML := "listen: 127.0.0.1:1\nproviders:\n" +
		"  zhipu: {provider_id: zhipu, openai_base_url: https://x}\n" +
		"  deepseek: {provider_id: deepseek, openai_base_url: https://y}\n" +
		"routes:\n" +
		"  m:\n" +
		"    - {provider: zhipu, model: m, priority: 1}\n" +
		"    - {provider: deepseek, model: m, priority: 2}\n" +
		"  solo:\n" +
		"    - {provider: zhipu, model: solo, priority: 1}\n"
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := p.Reload(cfgPath); err != nil {
		t.Fatalf("reload after login: %v", err)
	}
	if targets := p.expandedRoutes["m"]; len(targets) != 2 {
		t.Fatalf("mixed route targets after login+reload = %+v, want both targets back", targets)
	}
	if _, ok := p.expandedRoutes["solo"]; !ok {
		t.Fatal("solo route must return to the effective table after login+reload")
	}
	if ids := exposedModelIDs(t, px); !contains(ids, "solo") {
		t.Fatalf("GET /v1/models = %v after login, want solo back", ids)
	}
}
