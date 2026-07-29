package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

type credentialBoundaryUpstream struct {
	server   *httptest.Server
	hits     atomic.Int32
	requests chan credentialBoundaryHit
}

type credentialBoundaryHit struct {
	requestHit
	apiKey string
}

func newCredentialBoundaryUpstream(t *testing.T) *credentialBoundaryUpstream {
	t.Helper()
	upstream := &credentialBoundaryUpstream{
		requests: make(chan credentialBoundaryHit, 8),
	}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream.hits.Add(1)
		hit := credentialBoundaryHit{
			requestHit: captureHit(r),
			apiKey:     r.Header.Get("x-api-key"),
		}
		upstream.requests <- hit
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

func credentialBoundaryConfig(providerName, providerID, upstreamURL string) *Config {
	return &Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]Provider{
			providerName: {
				Provider:      providerID,
				OpenAIBaseURL: upstreamURL,
				Models:        []string{"target-model"},
			},
		},
		Routes: map[string][]RouteTarget{
			"alias": {{Provider: providerName, Model: "target-model"}},
		},
	}
}

func assertCredentialBoundaryUnavailable(t *testing.T, p *Proxy, upstream *credentialBoundaryUpstream, providerName string) {
	t.Helper()
	if _, ok := p.providers[providerName]; ok {
		t.Fatalf("provider %q must not have a runnable implementation", providerName)
	}
	if len(p.poolIndex) != 0 || len(p.parentOf) != 0 {
		t.Fatalf("unavailable provider populated pool identity maps: %v / %v", p.poolIndex, p.parentOf)
	}
	if _, ok := p.implicitRoutes["target-model"]; ok {
		t.Fatal("unavailable provider became eligible for an implicit route")
	}

	proxyServer := httptest.NewServer(http.HandlerFunc(p.handler))
	defer proxyServer.Close()
	status, body := post(t, proxyServer.URL+"/v1/chat/completions",
		`{"model":"alias","messages":[]}`)
	if status != http.StatusBadGateway || !strings.Contains(body, `all targets failed for model "alias"`) {
		t.Fatalf("forward = status %d body %q, want 502 target failure", status, body)
	}
	if got := upstream.hits.Load(); got != 0 {
		t.Fatalf("credential boundary leaked into %d upstream request(s)", got)
	}
}

func TestBuildProviders_CorruptPluralFailsClosedWithoutLegacyFallback(t *testing.T) {
	setPoolHome(t, t.TempDir())
	const name = "zhipu"
	if err := os.WriteFile(legacyPoolPath(name), []byte(`{"api_key":"LEGACY"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(poolPath(name), []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}

	upstream := newCredentialBoundaryUpstream(t)
	cfg := credentialBoundaryConfig(name, "zhipu", upstream.server.URL)
	if loggedInProviders(cfg)[name] {
		t.Fatal("corrupt plural must not be reported as logged in")
	}
	p := newTestProxy(t, cfg)
	assertCredentialBoundaryUnavailable(t, p, upstream, name)
}

func TestBuildProviders_EmptyAPIKeyPluralFailsClosedWithoutLegacyFallback(t *testing.T) {
	setPoolHome(t, t.TempDir())
	const name = "zhipu"
	if err := os.WriteFile(legacyPoolPath(name), []byte(`{"api_key":"LEGACY"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(poolPath(name),
		[]byte(`{"version":1,"accounts":[{"id":"invalid","api_key":""}]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	upstream := newCredentialBoundaryUpstream(t)
	cfg := credentialBoundaryConfig(name, "zhipu", upstream.server.URL)
	if loggedInProviders(cfg)[name] {
		t.Fatal("invalid plural must not be reported as logged in")
	}
	p := newTestProxy(t, cfg)
	assertCredentialBoundaryUnavailable(t, p, upstream, name)
}

func TestBuildProviders_EmptyPluralIsCredentialTombstone(t *testing.T) {
	setPoolHome(t, t.TempDir())
	const name = "zhipu"
	if err := os.WriteFile(legacyPoolPath(name), []byte(`{"api_key":"LEGACY"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := savePool(name, "zhipu", credentialPool{Version: 1}); err != nil {
		t.Fatal(err)
	}

	upstream := newCredentialBoundaryUpstream(t)
	cfg := credentialBoundaryConfig(name, "zhipu", upstream.server.URL)
	if loggedInProviders(cfg)[name] {
		t.Fatal("empty plural tombstone must not be reported as logged in")
	}
	p := newTestProxy(t, cfg)
	assertCredentialBoundaryUnavailable(t, p, upstream, name)
}

func TestBuildProviders_LegacyOnlyRemainsFileBacked(t *testing.T) {
	setPoolHome(t, t.TempDir())
	const name = "zhipu"
	if err := os.WriteFile(legacyPoolPath(name), []byte(`{"api_key":"LEGACY"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(poolPath(name)); !os.IsNotExist(err) {
		t.Fatalf("plural precondition failed: %v", err)
	}

	upstream := newCredentialBoundaryUpstream(t)
	cfg := credentialBoundaryConfig(name, "zhipu", upstream.server.URL)
	if !loggedInProviders(cfg)[name] {
		t.Fatal("valid zhipu legacy credential must be reported as logged in")
	}
	p := newTestProxy(t, cfg)
	if _, ok := p.providers[name]; !ok {
		t.Fatal("legacy-only zhipu must retain its file-backed runtime provider")
	}
	if route, ok := p.implicitRoutes["target-model"]; !ok || route.Provider != name {
		t.Fatalf("legacy-only provider implicit route = %+v, found=%v", route, ok)
	}
	proxyServer := httptest.NewServer(http.HandlerFunc(p.handler))
	defer proxyServer.Close()
	status, body := post(t, proxyServer.URL+"/v1/chat/completions",
		`{"model":"alias","messages":[]}`)
	if status != http.StatusOK {
		t.Fatalf("forward = status %d body %q, want 200", status, body)
	}
	if got := upstream.hits.Load(); got != 1 {
		t.Fatalf("upstream hits = %d, want 1", got)
	}
	hit := <-upstream.requests
	if hit.path != "/chat/completions" || hit.model != "target-model" || hit.auth != "Bearer LEGACY" {
		t.Fatalf("upstream hit = %+v, want rewritten model with exact legacy auth", hit)
	}
}

func TestBuildProviders_StaticProviderUsesPluralCredentialWithoutLegacyFile(t *testing.T) {
	setPoolHome(t, t.TempDir())
	const name = "static-up"
	writePoolFile(t, name, "static", "STATIC-POOL")
	if _, err := os.Stat(legacyPoolPath(name)); !os.IsNotExist(err) {
		t.Fatalf("singular precondition failed: %v", err)
	}

	upstream := newCredentialBoundaryUpstream(t)
	cfg := credentialBoundaryConfig(name, "static", upstream.server.URL)
	if !loggedInProviders(cfg)[name] {
		t.Fatal("valid static plural credential must be reported as logged in")
	}
	p := newTestProxy(t, cfg)
	if _, ok := p.providers[name]; !ok {
		t.Fatal("single plural static account must bind to the plain provider name")
	}
	if route, ok := p.implicitRoutes["target-model"]; !ok || route.Provider != name {
		t.Fatalf("static plural implicit route = %+v, found=%v", route, ok)
	}
	proxyServer := httptest.NewServer(http.HandlerFunc(p.handler))
	defer proxyServer.Close()
	status, body := post(t, proxyServer.URL+"/v1/chat/completions",
		`{"model":"alias","messages":[]}`)
	if status != http.StatusOK {
		t.Fatalf("forward = status %d body %q, want 200", status, body)
	}
	if got := upstream.hits.Load(); got != 1 {
		t.Fatalf("upstream hits = %d, want 1", got)
	}
	hit := <-upstream.requests
	if hit.path != "/chat/completions" || hit.model != "target-model" ||
		hit.auth != "Bearer STATIC-POOL" || hit.apiKey != "" {
		t.Fatalf("upstream hit = %+v, want exact pool-bound static auth", hit)
	}
}

func TestBuildProviders_StaticInvalidCredentialSourcesFailClosed(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T, providerName string)
	}{
		{name: "missing"},
		{
			name: "legacy",
			prepare: func(t *testing.T, providerName string) {
				if err := os.WriteFile(legacyPoolPath(providerName), []byte(`{"api_key":"LEGACY"}`), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "corrupt plural",
			prepare: func(t *testing.T, providerName string) {
				if err := os.WriteFile(poolPath(providerName), []byte(`{`), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "empty plural",
			prepare: func(t *testing.T, providerName string) {
				if err := savePool(providerName, "static", credentialPool{Version: 1}); err != nil {
					t.Fatal(err)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setPoolHome(t, t.TempDir())
			const name = "static-up"
			if tc.prepare != nil {
				tc.prepare(t, name)
			}

			upstream := newCredentialBoundaryUpstream(t)
			cfg := credentialBoundaryConfig(name, "static", upstream.server.URL)
			if loggedInProviders(cfg)[name] {
				t.Fatalf("static %s source must not be reported as a runnable login", tc.name)
			}
			p := newTestProxy(t, cfg)
			assertCredentialBoundaryUnavailable(t, p, upstream, name)
		})
	}
}

func TestBuildProviders_FailedProviderConstructionIsNotRouteEligible(t *testing.T) {
	setPoolHome(t, t.TempDir())
	const name = "unsupported-upstream"
	writePoolFile(t, name, "unsupported-provider-id", "VALID-POOL-KEY")

	upstream := newCredentialBoundaryUpstream(t)
	cfg := credentialBoundaryConfig(name, "unsupported-provider-id", upstream.server.URL)
	built := buildProviders(cfg)
	if len(built.providers) != 0 || len(built.poolIndex) != 0 ||
		len(built.parentOf) != 0 || len(built.eligible) != 0 {
		t.Fatalf("failed provider build leaked runtime state: %+v", built)
	}

	p := newTestProxy(t, cfg)
	assertCredentialBoundaryUnavailable(t, p, upstream, name)
}

func TestBuildProviders_OAuthProviderIgnoresAccountPoolFiles(t *testing.T) {
	setPoolHome(t, t.TempDir())
	const name = "codex-work"
	writePoolFile(t, name, "codex", "UNRELATED-API-KEY")
	cfg := &Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]Provider{
			name: {Provider: "codex", OpenAIBaseURL: "https://example.invalid"},
		},
	}
	if loggedInProviders(cfg)[name] {
		t.Fatal("API-key pool must not make an OAuth provider eligible for implicit routes")
	}
	p := newTestProxy(t, cfg)
	if _, ok := p.providers[name]; !ok {
		t.Fatal("OAuth provider must ignore unrelated API-key pool files")
	}
}
