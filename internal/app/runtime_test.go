package app

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"model-proxy/internal/accounts"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/provider"
	"model-proxy/internal/providerbuild"
	runtimestate "model-proxy/internal/runtime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---- runtime_assembly_test.go ----

func TestApplicationRuntimeOwnsIsolatedLifecycle(t *testing.T) {
	for _, test := range []struct {
		name      string
		web       bool
		uiStatus  int
		taskCount int
	}{
		{name: "Web disabled", uiStatus: http.StatusBadGateway},
		{name: "Web enabled", web: true, uiStatus: http.StatusOK, taskCount: 1},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			writeFreshApplicationCatalog(t, home)
			cfg := &configdomain.Config{
				Listen:    "127.0.0.1:0",
				Providers: map[string]configdomain.Provider{},
				Stats:     configdomain.StatsConfig{DBPath: filepath.Join(home, "stats.db")},
				Web:       configdomain.WebConfig{Enabled: test.web},
			}
			runtime := NewRuntime(cfg, "test-config.yaml", "")
			t.Cleanup(runtime.Close)

			if runtime.Proxy == nil || runtime.StartupConfig != cfg || runtime.Handler == nil {
				t.Fatal("applicationRuntime did not retain its concrete proxy/config/mux owners")
			}
			if runtime.ConfigPath != "test-config.yaml" {
				t.Fatalf("applicationRuntime configPath = %q, want test-config.yaml", runtime.ConfigPath)
			}
			if runtime.Proxy.StatsStore() == nil || runtime.Proxy.Flusher() == nil {
				t.Fatal("newApplicationRuntime did not start persisted runtime services")
			}
			catalog := runtime.Proxy.CatalogSnapshot()
			if catalog == nil || catalog.Count() != 1 {
				t.Fatalf("newApplicationRuntime catalog count = %v, want 1 from isolated cache", catalog)
			}
			if got := len(runtime.TransportTasks); got != test.taskCount {
				t.Fatalf("applicationRuntime transport tasks = %d, want %d", got, test.taskCount)
			}

			health := httptest.NewRecorder()
			runtime.Handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/health", nil))
			if health.Code != http.StatusOK {
				t.Fatalf("assembled root handler /health status = %d, want 200", health.Code)
			}
			ui := httptest.NewRecorder()
			runtime.Handler.ServeHTTP(ui, httptest.NewRequest(http.MethodGet, "/ui/", nil))
			if ui.Code != test.uiStatus {
				t.Fatalf("assembled /ui/ status = %d, want %d", ui.Code, test.uiStatus)
			}

			if test.taskCount != 0 {
				stop := make(chan struct{})
				done := make(chan struct{})
				go func() {
					runtime.TransportTasks[0](stop)
					close(done)
				}()
				close(stop)
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("Web transport task did not stop and join")
				}
			}

			stopped := make(chan struct{})
			if admitted := runtime.Proxy.Lifecycle().Run(func(stop <-chan struct{}) {
				<-stop
				close(stopped)
			}); !admitted {
				t.Fatal("isolated Proxy lifecycle did not admit owned task")
			}
			runtime.Close()
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Fatal("applicationRuntime.Close did not stop and join its Proxy lifecycle")
			}
			// Proxy.Close is idempotent; applicationRuntime must preserve that
			// property rather than layering a competing callback/task owner.
			runtime.Close()
			if runtime.Proxy.Lifecycle().Run(func(<-chan struct{}) {}) {
				t.Fatal("applicationRuntime.Close admitted work after closing its Proxy")
			}
		})
	}
}

func TestApplicationRuntimeReloadSummaryContract(t *testing.T) {
	file, _ := parseGoFile(t, "runtime.go")
	reload := namedMethod(t, file, "Runtime", "Reload")
	if got := selectorCallCountInNode(reload.Body, "Reload"); got != 1 {
		t.Errorf("applicationRuntime.reload nested reload calls = %d, want exactly runtime.Proxy.Reload", got)
	}
	if got := namedCallCountInNode(reload.Body, "SnapshotRuntime"); got != 1 {
		t.Errorf("applicationRuntime.reload SnapshotRuntime calls = %d, want exactly 1 successful reload summary", got)
	}
	if got := selectorCallCountInNode(reload.Body, "ProviderNames"); got != 1 {
		t.Errorf("applicationRuntime.reload Config.ProviderNames calls = %d, want exactly 1 successful reload summary", got)
	}
	if got := selectorCallCountInNode(reload.Body, "RouteNames"); got != 1 {
		t.Errorf("applicationRuntime.reload Config.RouteNames calls = %d, want exactly 1 successful reload summary", got)
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	writeFreshApplicationCatalog(t, home)
	configPath := filepath.Join(home, "config.yaml")
	if err := os.WriteFile(configPath, []byte(`listen: 127.0.0.1:17834
providers:
  demo:
    provider_id: zhipu
    openai_base_url: https://example.invalid/v1
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &configdomain.Config{
		Listen:    "127.0.0.1:17833",
		Providers: map[string]configdomain.Provider{},
		Stats:     configdomain.StatsConfig{DBPath: filepath.Join(home, "stats.db")},
	}
	runtime := NewRuntime(cfg, configPath, "")
	t.Cleanup(runtime.Close)
	runtime.Reload()
	snapshot := runtime.Proxy.SnapshotRuntime()
	if snapshot.Cfg.Listen != "127.0.0.1:17834" {
		t.Fatalf("reload runtime listen = %q, want 127.0.0.1:17834", snapshot.Cfg.Listen)
	}
	if provider := snapshot.Cfg.Providers["demo"]; provider.Provider != "zhipu" {
		t.Fatalf("reload provider demo = %#v, want provider_id zhipu", provider)
	}
	if runtime.StartupConfig.Listen != "127.0.0.1:17833" {
		t.Fatalf("startupConfig listen mutated to %q during reload", runtime.StartupConfig.Listen)
	}
}

func writeFreshApplicationCatalog(t *testing.T, home string) {
	t.Helper()
	dir := filepath.Join(home, ".model-proxy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{"fetched_at":"` + time.Now().UTC().Format(time.RFC3339Nano) +
		`","etag":"","by_name":{"test-model":{"ctx":4096,"out":1024,"in":["text"],"out_mod":["text"]}}}`)
	if err := os.WriteFile(filepath.Join(dir, "models_cache.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// selectorCallCountInNode counts <x>.<name>(...) call sites under n.
func selectorCallCountInNode(n ast.Node, name string) int {
	count := 0
	ast.Inspect(n, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
			count++
		}
		return true
	})
	return count
}

// -- minimal local AST helpers (the shared set lives in internal/archtest;
// this package only needs the few below) --

func parseGoFile(t *testing.T, path string) (*ast.File, *token.FileSet) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	return f, fset
}

func namedMethod(t *testing.T, f *ast.File, receiver, name string) *ast.FuncDecl {
	t.Helper()
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Name.Name != name {
			continue
		}
		for _, field := range fn.Recv.List {
			if simpleTypeName(field.Type) == receiver || simpleTypeName(field.Type) == "*"+receiver {
				return fn
			}
		}
	}
	t.Fatalf("method %s.%s not found", receiver, name)
	return nil
}

func simpleTypeName(expr ast.Expr) string {
	switch typ := expr.(type) {
	case *ast.Ident:
		return typ.Name
	case *ast.StarExpr:
		return "*" + simpleTypeName(typ.X)
	default:
		return fmt.Sprintf("%T", expr)
	}
}

func namedCallCountInNode(n ast.Node, name string) int {
	count := 0
	ast.Inspect(n, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			if fun.Name == name {
				count++
			}
		case *ast.SelectorExpr:
			if fun.Sel.Name == name {
				count++
			}
		}
		return true
	})
	return count
}

// ---- runtime_state_support_test.go ----

// The helpers in this file keep root integration tests on the public runtime
// boundary. Pure state-machine details belong in internal/runtime tests.

type rateLimitKind = runtimestate.RateLimitKind

const (
	rlTransient = runtimestate.Transient
	rlQuota     = runtimestate.Quota
	rlDaily     = runtimestate.Daily
)

func seedRuntimeRateLimit(
	t testing.TB,
	p *Proxy,
	providerName string,
	until time.Time,
	kind rateLimitKind,
) {
	t.Helper()
	if !p.runtimeState.RecordRateLimit(providerName, until, kind, 0) {
		t.Fatalf("seed rate limit for %q was rejected", providerName)
	}
}

func seedRuntimeCircuit(
	t testing.TB,
	p *Proxy,
	providerName string,
	until time.Time,
) {
	t.Helper()
	// RecordFailure opens the circuit with the Manager's own cooldown window;
	// the helper's `until` only documents intent, matching the adapter path.
	p.runtimeState.RecordFailure(
		providerName,
		1,
		time.Hour,
		0,
	)
}

func seedRuntimeSticky(
	t testing.TB,
	p *Proxy,
	key, providerName string,
	since time.Time,
) {
	t.Helper()
	if !p.runtimeState.SetSticky(
		key,
		runtimestate.Sticky{Provider: providerName, Since: since},
		0,
	) {
		t.Fatalf("seed sticky %q was rejected", key)
	}
}

// ---- build_seams_test.go ----

// testBuildOpts wires the production providerbuild environment seams for app
// tests that drive provider construction through Proxy fixtures.
func testBuildOpts() providerbuild.BuildOptions {
	return providerbuild.BuildOpts()
}

// ---- buildproviders_pool_test.go ----

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
	p := accounts.Pool{Version: 1}
	for _, k := range keys {
		c := accounts.Credentials{APIKey: k}
		p.Accounts = append(p.Accounts, accounts.Account{
			ID:      accounts.AccountID(providerID, c),
			Label:   k,
			APIKey:  k,
			AddedAt: "2026-07-08",
		})
	}
	if err := accounts.NewStore(accounts.HomeDir()).Save(name, providerID, p); err != nil {
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

	cfg := &configdomain.Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]configdomain.Provider{
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

	cfg := &configdomain.Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]configdomain.Provider{
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

	cfg := &configdomain.Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]configdomain.Provider{
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

	cfg := &configdomain.Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]configdomain.Provider{
			"zhipu": {OpenAIBaseURL: up.URL, Provider: "zhipu"},
		},
		Routes: map[string][]configdomain.RouteTarget{
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

// ---- buildproviders_credential_boundary_test.go ----

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

func credentialBoundaryConfig(providerName, providerID, upstreamURL string) *configdomain.Config {
	return &configdomain.Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]configdomain.Provider{
			providerName: {
				Provider:      providerID,
				OpenAIBaseURL: upstreamURL,
				Models:        []string{"target-model"},
			},
		},
		Routes: map[string][]configdomain.RouteTarget{
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
	proxyServer := httptest.NewServer(http.HandlerFunc(p.Handler))
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
	if err := os.WriteFile(accounts.NewStore(accounts.HomeDir()).PoolPath(name), []byte(`{`), 0o600); err != nil {
		t.Fatal(err)
	}

	upstream := newCredentialBoundaryUpstream(t)
	cfg := credentialBoundaryConfig(name, "zhipu", upstream.server.URL)
	p := newTestProxy(t, cfg)
	assertCredentialBoundaryUnavailable(t, p, upstream, name)
}

func TestBuildProviders_EmptyAPIKeyPluralFailsClosedWithoutLegacyFallback(t *testing.T) {
	setPoolHome(t, t.TempDir())
	const name = "zhipu"
	if err := os.WriteFile(legacyPoolPath(name), []byte(`{"api_key":"LEGACY"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(accounts.NewStore(accounts.HomeDir()).PoolPath(name),
		[]byte(`{"version":1,"accounts":[{"id":"invalid","api_key":""}]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	upstream := newCredentialBoundaryUpstream(t)
	cfg := credentialBoundaryConfig(name, "zhipu", upstream.server.URL)
	p := newTestProxy(t, cfg)
	assertCredentialBoundaryUnavailable(t, p, upstream, name)
}

func TestBuildProviders_EmptyPluralIsCredentialTombstone(t *testing.T) {
	setPoolHome(t, t.TempDir())
	const name = "zhipu"
	if err := os.WriteFile(legacyPoolPath(name), []byte(`{"api_key":"LEGACY"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := accounts.NewStore(accounts.HomeDir()).Save(name, "zhipu", accounts.Pool{Version: 1}); err != nil {
		t.Fatal(err)
	}

	upstream := newCredentialBoundaryUpstream(t)
	cfg := credentialBoundaryConfig(name, "zhipu", upstream.server.URL)
	p := newTestProxy(t, cfg)
	assertCredentialBoundaryUnavailable(t, p, upstream, name)
}

func TestBuildProviders_LegacyOnlyRemainsFileBacked(t *testing.T) {
	setPoolHome(t, t.TempDir())
	const name = "zhipu"
	if err := os.WriteFile(legacyPoolPath(name), []byte(`{"api_key":"LEGACY"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(accounts.NewStore(accounts.HomeDir()).PoolPath(name)); !os.IsNotExist(err) {
		t.Fatalf("plural precondition failed: %v", err)
	}

	upstream := newCredentialBoundaryUpstream(t)
	cfg := credentialBoundaryConfig(name, "zhipu", upstream.server.URL)
	p := newTestProxy(t, cfg)
	if _, ok := p.providers[name]; !ok {
		t.Fatal("legacy-only zhipu must retain its file-backed runtime provider")
	}
	proxyServer := httptest.NewServer(http.HandlerFunc(p.Handler))
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
	p := newTestProxy(t, cfg)
	if _, ok := p.providers[name]; !ok {
		t.Fatal("single plural static account must bind to the plain provider name")
	}
	proxyServer := httptest.NewServer(http.HandlerFunc(p.Handler))
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
				if err := os.WriteFile(accounts.NewStore(accounts.HomeDir()).PoolPath(providerName), []byte(`{`), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "empty plural",
			prepare: func(t *testing.T, providerName string) {
				if err := accounts.NewStore(accounts.HomeDir()).Save(providerName, "static", accounts.Pool{Version: 1}); err != nil {
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
	built := providerbuild.BuildProviders(cfg, accounts.NewStore(accounts.HomeDir()), testBuildOpts())
	if len(built.Providers) != 0 || len(built.PoolIndex) != 0 ||
		len(built.ParentOf) != 0 || len(built.Eligible) != 0 {
		t.Fatalf("failed provider build leaked runtime state: %+v", built)
	}

	p := newTestProxy(t, cfg)
	assertCredentialBoundaryUnavailable(t, p, upstream, name)
}

func TestBuildProviders_OAuthProviderIgnoresAccountPoolFiles(t *testing.T) {
	setPoolHome(t, t.TempDir())
	const name = "codex-work"
	writePoolFile(t, name, "codex", "UNRELATED-API-KEY")
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]configdomain.Provider{
			name: {Provider: "codex", OpenAIBaseURL: "https://example.invalid"},
		},
	}
	p := newTestProxy(t, cfg)
	if _, ok := p.providers[name]; !ok {
		t.Fatal("OAuth provider must ignore unrelated API-key pool files")
	}
}

// ---- volcengine_creds_test.go ----

// volcengine_creds_test.go covers the app-side view of the volcengine
// credential pool (per-account AK/SK unrolled into virtuals). The pool login
// wrapper tests (runVolcengineLoginWithInput: writes the
// {api_key, access_key, secret_key} triple, dedup by AccessKey) moved to
// internal/cli/login with the login core extraction. LoadVolcengineCreds is
// tested in internal/providerbuild; usage display and quota behavior belong
// to the provider package.

// --- Task 10: per-account AK/SK in the credential pool ---

// writeVolcenginePool writes a volcengine credential pool (plural file) where
// each account carries the full {api_key, access_key, secret_key} triple. The
// generic writePoolFile helper only writes APIKey, so volcengine needs its own.
func writeVolcenginePool(t *testing.T, name string, accts ...accounts.Account) {
	t.Helper()
	p := accounts.Pool{Version: 1, Accounts: accts}
	if err := accounts.NewStore(accounts.HomeDir()).Save(name, "volcengine", p); err != nil {
		t.Fatal(err)
	}
}

// TestVolcenginePoolPerAccountAK verifies a 2-account volcengine pool (DISTINCT
// AccessKeys) is unrolled into 2 virtuals keyed by access_key. The virtual id
// suffix MUST be the AccessKey (account-level), not the api_key hash.
func TestVolcenginePoolPerAccountAK(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	writeVolcenginePool(t, "volcengine",
		accounts.Account{ID: "AK1", Label: "a", APIKey: "k1", AccessKey: "AK1", SecretKey: "SK1", AddedAt: "x"},
		accounts.Account{ID: "AK2", Label: "b", APIKey: "k2", AccessKey: "AK2", SecretKey: "SK2", AddedAt: "x"},
	)
	cfg := &configdomain.Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]configdomain.Provider{
			"volcengine": {OpenAIBaseURL: "https://v", Provider: "volcengine"},
		},
	}
	px := newTestProxy(t, cfg)
	if got := len(px.poolIndex["volcengine"]); got != 2 {
		t.Fatalf("poolIndex[volcengine] len = %d, want 2 (%v)", got, px.poolIndex["volcengine"])
	}
	want := map[string]bool{
		"volcengine#" + accounts.AccountID("volcengine", accounts.Credentials{AccessKey: "AK1"}): true,
		"volcengine#" + accounts.AccountID("volcengine", accounts.Credentials{AccessKey: "AK2"}): true,
	}
	for _, vid := range px.poolIndex["volcengine"] {
		if !want[vid] {
			t.Fatalf("unexpected virtual %q (want hashed volcengine ids)", vid)
		}
	}
}
