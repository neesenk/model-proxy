package app

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"model-proxy/internal/accounts"
	"model-proxy/internal/provider"
)

// buildproviders_pool_test.go covers the credential-pool unrolling in
// buildProviders: a pooled provider (>=2 accounts) is split into N virtual
// providers ("name#<id>"), each bound to a distinct key in all THREE binding
// points (embedded ApiKeyBase forward path, pcfg.Auth FetchModels path, and the
// Usage/Quota closures). Single-account pools keep the plain name unchanged.

// writePoolFile writes a plural credential pool for `name` with the given keys.
// Each key becomes its own pool account (ID derived via accountIDFor so virtual
// ids are stable + unique).
func writePoolFile(t *testing.T, name, providerID string, keys ...string) {
	t.Helper()
	p := CredentialPool{Version: 1}
	for _, k := range keys {
		c := AccountCred{APIKey: k}
		p.Accounts = append(p.Accounts, PoolAccount{
			ID:      accounts.AccountID(providerID, c),
			Label:   k,
			APIKey:  k,
			AddedAt: "2026-07-08",
		})
	}
	if err := AccountStore().Save(name, providerID, p); err != nil {
		t.Fatal(err)
	}
}

// TestBuildProvidersUnrollsPool verifies a 3-account zhipu pool is unrolled
// into 3 virtuals, the parent name is NOT a runnable provider key, and each
// virtual injects its OWN exact key via AuthHeaders (binding point #1: the
// embedded ApiKeyBase forward path — zhipu inherits ApiKeyBase.AuthHeaders).
func TestBuildProvidersUnrollsPool(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	writePoolFile(t, "zhipu", "zhipu", "KEY-A", "KEY-B", "KEY-C")

	cfg := &Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]Provider{
			"zhipu": {OpenAIBaseURL: "https://z", Provider: "zhipu"},
		},
	}
	p := newTestProxy(t, cfg)

	// Parent is not a runnable provider; 3 virtuals are.
	if _, ok := p.providers["zhipu"]; ok {
		t.Fatal("parent zhipu should not be in providers map when pooled")
	}
	if got := len(p.providers); got != 3 {
		t.Fatalf("want 3 virtuals in providers map, got %d (%v)", got, keysOf(p.providers))
	}
	// poolIndex records the parent → sorted virtual ids; parentOf inverts it.
	vids := p.poolIndex["zhipu"]
	if len(vids) != 3 {
		t.Fatalf("poolIndex[zhipu] len = %d, want 3 (%v)", len(vids), vids)
	}
	// Each virtual injects its own exact key (distinct).
	want := map[string]bool{"Bearer KEY-A": true, "Bearer KEY-B": true, "Bearer KEY-C": true}
	seen := map[string]bool{}
	for _, vid := range vids {
		impl, ok := p.providers[vid]
		if !ok {
			t.Fatalf("virtual %q in poolIndex but not in providers map", vid)
		}
		req := httptest.NewRequest(http.MethodGet, "https://z/m", nil)
		if err := impl.AuthHeaders(req); err != nil {
			t.Fatalf("virtual %s AuthHeaders: %v", vid, err)
		}
		tok := req.Header.Get("Authorization")
		if !want[tok] {
			t.Fatalf("virtual %s injected %q, want one of Bearer KEY-A/B/C", vid, tok)
		}
		if seen[tok] {
			t.Fatalf("token %q injected by two virtuals (keys not distinct across virtuals)", tok)
		}
		seen[tok] = true
		if p.parentOf[vid] != "zhipu" {
			t.Fatalf("parentOf[%s]=%q want zhipu", vid, p.parentOf[vid])
		}
	}
	// The runtime manager lazily owns the pool spread counter.
	if got := p.runtimeState.ResolverSpreadStart("zhipu", len(vids), 0); got != 0 {
		t.Fatalf("initial pool spread index = %d, want 0", got)
	}
}

// TestBuildProvidersSingleAccountKeepsPlainName verifies backward compat: when
// only the legacy singular <name>_apikey.json exists (no plural pool), the
// provider keeps its plain name, poolIndex/parentOf stay empty, and the key is
// still read from the FILE (not bound in-memory) — so Login/Logout file
// semantics are byte-for-byte unchanged.
func TestBuildProvidersSingleAccountKeepsPlainName(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	// Legacy singular file (loadPool wraps it as a 1-entry pool → single-acct).
	os.WriteFile(filepath.Join(dir, ".model-proxy", "zhipu_apikey.json"),
		[]byte(`{"api_key":"SOLO"}`), 0o600)

	cfg := &Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]Provider{
			"zhipu": {OpenAIBaseURL: "https://z", Provider: "zhipu"},
		},
	}
	p := newTestProxy(t, cfg)
	if _, ok := p.providers["zhipu"]; !ok {
		t.Fatal("single account must keep plain name zhipu")
	}
	if len(p.poolIndex) != 0 {
		t.Fatalf("poolIndex should be empty for single accounts, got %v", p.poolIndex)
	}
	if len(p.parentOf) != 0 {
		t.Fatalf("parentOf should be empty for single accounts, got %v", p.parentOf)
	}
	// The single account reads the key from the FILE (file-backed, not bound) —
	// this is the legacy path, so Logout will still delete the on-disk file.
	impl := p.providers["zhipu"]
	req := httptest.NewRequest(http.MethodGet, "https://z/m", nil)
	if err := impl.AuthHeaders(req); err != nil {
		t.Fatalf("AuthHeaders: %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer SOLO" {
		t.Fatalf("single-account Authorization = %q, want Bearer SOLO (file-backed)", got)
	}
}

// TestBuildProvidersUnrollsDeepseekDualAuth verifies binding point #1 for a
// provider that OVERRIDES ApiKeyBase.AuthHeaders with its own dual-scheme
// implementation (deepseek sets BOTH Authorization: Bearer AND x-api-key). This
// is the CRITICAL guard: the embedded ApiKeyBase must be bound for providers
// whose AuthHeaders reads via p.LoadKey(), not via cfg.Auth.
func TestBuildProvidersUnrollsDeepseekDualAuth(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	writePoolFile(t, "deepseek", "deepseek", "DS-1", "DS-2")

	cfg := &Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]Provider{
			"deepseek": {OpenAIBaseURL: "https://ds", Provider: "deepseek"},
		},
	}
	p := newTestProxy(t, cfg)
	if _, ok := p.providers["deepseek"]; ok {
		t.Fatal("parent deepseek should not be in providers map when pooled")
	}
	want := map[string]bool{"Bearer DS-1": true, "Bearer DS-2": true}
	for _, vid := range p.poolIndex["deepseek"] {
		req := httptest.NewRequest(http.MethodGet, "https://ds/m", nil)
		impl := p.providers[vid]
		if err := impl.AuthHeaders(req); err != nil {
			t.Fatalf("virtual %s AuthHeaders: %v", vid, err)
		}
		bearer := req.Header.Get("Authorization")
		apiKey := req.Header.Get("x-api-key")
		// Dual-scheme: Bearer and x-api-key MUST carry the SAME bound key.
		if !want[bearer] {
			t.Fatalf("virtual %s Authorization = %q, want one of Bearer DS-1/DS-2", vid, bearer)
		}
		wantKey := strings.TrimPrefix(bearer, "Bearer ")
		if apiKey != wantKey {
			t.Fatalf("virtual %s x-api-key = %q, want %q (must match Bearer)", vid, apiKey, wantKey)
		}
		delete(want, bearer)
	}
	if len(want) != 0 {
		t.Fatalf("missing deepseek tokens: %v", want)
	}
}

// keysOf returns the providers-map keys (for readable failure output only —
// order not used for assertions).
func keysOf(m map[string]provider.Provider) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestBuildProvidersSingleEntryPluralPoolBindsKey is the C1 regression: a
// fresh `login zhipu` writes ONLY the plural <name>_apikeys.json (via savePool)
// — the legacy singular <name>_apikey.json is NOT created. The 1-entry plural
// pool must bind the account's key in-memory under the plain name so a forward
// actually authenticates; before the fix, buildOne got an EMPTY cred, the
// embedded ApiKeyBase stayed file-backed reading the (non-existent) singular,
// and every request 502'd with "not logged in". Drives a real forward through
// an httptest upstream and asserts the exact Bearer token it receives.
func TestBuildProvidersSingleEntryPluralPoolBindsKey(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	writePoolFile(t, "zhipu", "zhipu", "ONLY")
	// Sanity: the plural file exists and the singular does NOT — this is the
	// post-`login` state the regression guards.
	if _, err := os.Stat(filepath.Join(dir, ".model-proxy", "zhipu_apikey.json")); !os.IsNotExist(err) {
		t.Fatalf("precondition: singular zhipu_apikey.json should not exist (stat err=%v)", err)
	}

	var upstreamHit requestHit
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit = captureHit(r)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer up.Close()

	cfg := &Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]Provider{
			"zhipu": {OpenAIBaseURL: up.URL, Provider: "zhipu"},
		},
		Routes: map[string][]RouteTarget{
			"glm-4.6": {{Provider: "zhipu", Model: "glm-4.6"}},
		},
	}
	p := newTestProxy(t, cfg)
	// Plain name is the runnable provider; no virtuals.
	if _, ok := p.providers["zhipu"]; !ok {
		t.Fatal("1-entry plural pool must keep plain name zhipu")
	}
	if len(p.poolIndex) != 0 || len(p.parentOf) != 0 {
		t.Fatalf("1-entry pool should not populate poolIndex/parentOf, got %v / %v", p.poolIndex, p.parentOf)
	}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	postOK(t, px.URL+"/v1/chat/completions", `{"model":"glm-4.6","messages":[]}`)

	if upstreamHit.auth != "Bearer ONLY" {
		t.Fatalf("upstream Authorization = %q, want Bearer ONLY (the bound pool key)", upstreamHit.auth)
	}
	if upstreamHit.model != "glm-4.6" {
		t.Fatalf("upstream model = %q, want glm-4.6 (rewriteModel)", upstreamHit.model)
	}
}
