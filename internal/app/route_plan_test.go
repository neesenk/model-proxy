package app

import (
	"encoding/json"
	"fmt"
	"io"
	responsecache "model-proxy/internal/cache"
	"model-proxy/internal/protocol"
	"model-proxy/internal/routing"
	"model-proxy/internal/takeover"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- route_preview_test.go ----

// TestDebugRoute_Preview: POST /debug/route answers where a request would go
// right now — route resolution, ordered targets, per-target fit — without an
// upstream call and without mutating scheduler state (sticky must not latch).
func TestDebugRoute_Preview(t *testing.T) {
	cfg, _ := LoadConfigFromBytes("test", []byte(`listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x}
  deepseek: {provider_id: deepseek, openai_base_url: https://y}
routes:
  claude-glm: [{provider: zhipu, model: glm}, {provider: deepseek, model: ds}]
  glm: [{provider: zhipu, model: glm}, {provider: deepseek, model: ds}]
`))
	p := newTestProxy(t, cfg)
	post := func(body string, headers map[string]string) map[string]any {
		req := httptest.NewRequest(http.MethodPost, "/debug/route", strings.NewReader(body))
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		p.Handler(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("preview status = %d, body = %s", rec.Code, rec.Body)
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("preview json: %v, body = %s", err, rec.Body)
		}
		return out
	}

	// anthropic default proto: the claude-* alias is an explicit route.
	out := post(`{"model":"claude-glm","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`, nil)
	if out["route_found"] != true || out["exposed"] != "claude-glm" {
		t.Fatalf("alias route preview = %v", out)
	}
	ordered, ok := out["ordered"].([]any)
	if !ok || len(ordered) != 2 {
		t.Fatalf("ordered = %v, want both route targets", out["ordered"])
	}
	first := ordered[0].(map[string]any)
	if first["provider"] != "zhipu" || first["model"] != "glm" {
		t.Errorf("first ordered target = %v", first)
	}
	if _, has := first["fits"]; !has {
		t.Errorf("fit verdict missing: %v", first)
	}

	// provider/model prefix: decomposed to the bare route, narrowed to that
	// provider, and reported like a force-provider.
	out = post(`{"model":"deepseek/glm","messages":[]}`, nil)
	if out["route_found"] != true || out["exposed"] != "glm" || out["provider_prefix"] != "deepseek" {
		t.Fatalf("provider-prefixed preview = %v", out)
	}
	ordered = out["ordered"].([]any)
	if len(ordered) != 1 || ordered[0].(map[string]any)["provider"] != "deepseek" {
		t.Fatalf("provider-prefixed ordered = %v", out["ordered"])
	}

	// Unknown model: route_found false, no error status.
	out = post(`{"model":"no-such","messages":[]}`, nil)
	if out["route_found"] != false {
		t.Fatalf("unknown model preview = %v", out)
	}

	// Missing model field: reported in the payload.
	out = post(`{"messages":[]}`, nil)
	if out["route_found"] != false || out["error"] == nil {
		t.Fatalf("missing model preview = %v", out)
	}

	// force-provider participates; a typo'd provider is surfaced, not fatal.
	out = post(`{"model":"glm","messages":[]}`, map[string]string{"x-mp-force-provider": "deepseek"})
	ordered = out["ordered"].([]any)
	if len(ordered) != 1 || ordered[0].(map[string]any)["provider"] != "deepseek" {
		t.Fatalf("forced preview ordered = %v", out["ordered"])
	}
	out = post(`{"model":"glm","messages":[]}`, map[string]string{"x-mp-force-provider": "typo"})
	if out["route_found"] != true || out["error"] == nil || len(out["ordered"].([]any)) != 0 {
		t.Fatalf("typo force preview = %v", out)
	}

	// No scheduler mutation: the same preview twice yields the same first
	// target (a committed schedule would advance pool spread / could latch
	// sticky), and the manager reports no sticky for the route.
	before := post(`{"model":"glm","messages":[]}`, map[string]string{"x-claude-code-session-id": "s1"})
	after := post(`{"model":"glm","messages":[]}`, map[string]string{"x-claude-code-session-id": "s1"})
	if before["sticky"] != nil || after["sticky"] != nil {
		t.Errorf("preview latched sticky: before=%v after=%v", before["sticky"], after["sticky"])
	}
	firstOf := func(out map[string]any) string {
		return out["ordered"].([]any)[0].(map[string]any)["provider"].(string)
	}
	if firstOf(before) != firstOf(after) {
		t.Errorf("preview mutated scheduling: %s → %s", firstOf(before), firstOf(after))
	}

	// Bad proto → 400.
	req := httptest.NewRequest(http.MethodPost, "/debug/route?proto=gemini", strings.NewReader(`{"model":"glm"}`))
	rec := httptest.NewRecorder()
	p.Handler(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad proto status = %d, want 400", rec.Code)
	}
}

// The preview's cache probe is read-only: polling /debug/route must not book
// misses into the operational hit-rate stats (Peek, not Lookup) — a poller
// watching the preview would otherwise grind the metrics down.
func TestDebugRoute_PreviewCacheProbeIsReadOnly(t *testing.T) {
	cfg, _ := LoadConfigFromBytes("test", []byte(`listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x}
routes:
  glm: [{provider: zhipu, model: glm}]
cache:
  enabled: true
  ttl: 1m
`))
	p := newTestProxy(t, cfg)
	if p.cache == nil {
		t.Fatal("config cache: section must enable the store")
	}
	body := `{"model":"glm","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	post := func(query string) map[string]any {
		req := httptest.NewRequest(http.MethodPost, "/debug/route"+query, strings.NewReader(body))
		rec := httptest.NewRecorder()
		p.Handler(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("preview status = %d, body = %s", rec.Code, rec.Body)
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("preview json: %v, body = %s", err, rec.Body)
		}
		return out
	}

	// Prime entries for the REAL inbound endpoints. The diagnostic path and its
	// ?proto control query must never participate in the key.
	for _, path := range []string{"/v1/messages", "/v1/chat/completions", "/v1/responses"} {
		keyReq := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		p.cache.Put(responsecache.Key(keyReq, []byte(body)), "m", http.StatusOK,
			http.Header{"Content-Type": {"application/json"}}, []byte(`{}`), time.Now())
	}
	for _, query := range []string{"", "?proto=openai", "?proto=responses"} {
		for i := 0; i < 3; i++ {
			if out := post(query); out["cache"] != "hit" {
				t.Fatalf("preview %s #%d cache state = %v, want hit", query, i, out["cache"])
			}
		}
	}
	if got := p.cache.Stats(); got.Hits != 0 || got.Misses != 0 {
		t.Errorf("preview probe mutated stats: %+v, want zero counters", got)
	}
}

func TestDebugRoute_PreviewAppliesGuardBeforeCache(t *testing.T) {
	base := `listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x}
routes:
  glm: [{provider: zhipu, model: glm}]
cache: {enabled: true, ttl: 1m}
guard: {secrets: %s, audit: false}
`
	body := []byte(guardRequestBody())
	preview := func(t *testing.T, action string, primeBody []byte) map[string]any {
		t.Helper()
		cfg, err := LoadConfigFromBytes("test", []byte(fmt.Sprintf(base, action)))
		if err != nil {
			t.Fatal(err)
		}
		p := newTestProxy(t, cfg)
		keyReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(primeBody)))
		p.cache.Put(responsecache.Key(keyReq, primeBody), "m", http.StatusOK,
			http.Header{"Content-Type": {"application/json"}}, []byte(`{}`), time.Now())
		req := httptest.NewRequest(http.MethodPost, "/debug/route", strings.NewReader(string(body)))
		rec := httptest.NewRecorder()
		p.Handler(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("preview status = %d, body = %s", rec.Code, rec.Body)
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("preview json: %v", err)
		}
		return out
	}

	t.Run("redact hashes forwarded body", func(t *testing.T) {
		cfg, err := LoadConfigFromBytes("test", []byte(fmt.Sprintf(base, "redact")))
		if err != nil {
			t.Fatal(err)
		}
		p := newTestProxy(t, cfg)
		redacted := p.guardScanner.Redact(body)
		out := preview(t, "redact", redacted)
		if out["cache"] != "hit" {
			t.Fatalf("redacted cache state = %v, want hit", out["cache"])
		}
		guardState := out["guard"].(map[string]any)
		if guardState["body_redacted"] != true || guardState["blocked"] != false {
			t.Fatalf("redact guard state = %+v", guardState)
		}
	})

	t.Run("block bypasses cache and scheduling", func(t *testing.T) {
		out := preview(t, "block", body)
		if out["cache"] != "bypass (guard block)" {
			t.Fatalf("block cache state = %v", out["cache"])
		}
		if ordered, ok := out["ordered"].([]any); !ok || len(ordered) != 0 {
			t.Fatalf("blocked preview ordered = %#v, want empty", out["ordered"])
		}
		guardState := out["guard"].(map[string]any)
		if guardState["blocked"] != true {
			t.Fatalf("block guard state = %+v", guardState)
		}
	})

	t.Run("strong path block bypasses cache", func(t *testing.T) {
		pathBody := []byte(`{"model":"glm","messages":[{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"~/.ssh/id_rsa"}}]}]}`)
		cfg, err := LoadConfigFromBytes("test", []byte(`listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x}
routes:
  glm: [{provider: zhipu, model: glm}]
cache: {enabled: true, ttl: 1m}
guard: {secrets: off, paths: block, audit: false}
`))
		if err != nil {
			t.Fatal(err)
		}
		p := newTestProxy(t, cfg)
		keyReq := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(string(pathBody)))
		p.cache.Put(responsecache.Key(keyReq, pathBody), "m", http.StatusOK,
			http.Header{"Content-Type": {"application/json"}}, []byte(`{}`), time.Now())
		req := httptest.NewRequest(http.MethodPost, "/debug/route", strings.NewReader(string(pathBody)))
		rec := httptest.NewRecorder()
		p.Handler(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("path preview status = %d, body = %s", rec.Code, rec.Body)
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if out["cache"] != "bypass (guard block)" {
			t.Fatalf("path block cache state = %v", out["cache"])
		}
		guardState := out["guard"].(map[string]any)
		if guardState["blocked"] != true || guardState["strong_paths"] == nil {
			t.Fatalf("path block guard state = %+v", guardState)
		}
	})
}

// ---- implicit_routes_test.go ----

// implicit_routes_test.go keeps the Proxy-level coverage of derived routes
// (end-to-end forwarding, /v1/models listing, takeover parity). The pure
// config→route-table unit tests live in internal/routing alongside
// DeriveRoutesFrom / RouteTable / BuildExpandedRoutes.

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
		Listen: "127.0.0.1:15721",
		Providers: map[string]Provider{
			"zhipu": {Provider: "zhipu", OpenAIBaseURL: "http://x", Models: []string{"glm-4.6"}},
		},
	}
	file := filepath.Join(dir, "oc.json")
	os.WriteFile(file, []byte(`{}`), 0o644)
	routes := routing.RouteTable(cfg)
	if _, ok := routes["glm-4.6"]; !ok {
		t.Fatalf("route table should include glm-4.6: %v", routes)
	}
	tpl, err := takeover.TemplateByName("opencode", "")
	if err != nil {
		t.Fatal(err)
	}
	tpl.File = file
	if err := tpl.Rewrite(cfg, nil, routes); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(file)
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

// ---- resolve_proxy_test.go ----

// TestResolver_ExpandAndPick: the unified provider resolver — pool expansion
// (Expand), session-sticky + health-aware single-virtual pick (Pick). Non-pooled
// providers pass through; unknown / not-built providers yield no runnable virtual.
func TestResolver_ExpandAndPick(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	writePoolFile(t, "zhipu", "zhipu", "KEY-A", "KEY-B")
	cfg := &Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]Provider{
			"zhipu": {OpenAIBaseURL: "https://z", Provider: "zhipu"},
		},
	}
	p := newTestProxy(t, cfg)
	r := newResolver(p, p.providers, p.poolIndex)

	// Expand: pooled parent → both virtuals, Model/Priority/Protocol preserved.
	got := r.Expand(RouteTarget{Provider: "zhipu", Model: "glm", Priority: 2, Protocol: "openai"})
	if len(got) != 2 {
		t.Fatalf("Expand pooled = %d targets, want 2", len(got))
	}
	for i, rt := range got {
		if rt.Provider == "zhipu" {
			t.Errorf("Expand[%d] returned the parent name, not a virtual", i)
		}
		if rt.Model != "glm" || rt.Priority != 2 || rt.Protocol != "openai" {
			t.Errorf("Expand[%d] = %+v, want Model/Priority/Protocol preserved", i, rt)
		}
		if p.parentOf[rt.Provider] != "zhipu" {
			t.Errorf("Expand[%d] %q is not a zhipu virtual", i, rt.Provider)
		}
	}

	// Pick with a session key is STICKY: the same key always lands on the same
	// virtual (cache-warm for a conversation).
	sessA, ok := r.Pick(RouteTarget{Provider: "zhipu", Model: "glm"}, "session-A")
	if !ok {
		t.Fatal("Pick pooled !ok")
	}
	if p.parentOf[sessA.Provider] != "zhipu" {
		t.Error("Pick did not return a zhipu virtual")
	}
	for i := 0; i < 5; i++ {
		pick, ok := r.Pick(RouteTarget{Provider: "zhipu", Model: "glm"}, "session-A")
		if !ok || pick.Provider != sessA.Provider {
			t.Errorf("Pick session-A iter %d = %q, want stable %q (session-sticky)", i, pick.Provider, sessA.Provider)
		}
	}
	// A different session key lands on a (likely) different virtual — distinct
	// sessions spread across the pool. With 2 accounts and a good hash, both
	// appear across a handful of distinct keys.
	seen := map[string]bool{sessA.Provider: true}
	for _, key := range []string{"s1", "s2", "s3", "s4", "s5", "s6"} {
		pick, ok := r.Pick(RouteTarget{Provider: "zhipu", Model: "glm"}, key)
		if !ok {
			t.Fatalf("Pick %s !ok", key)
		}
		seen[pick.Provider] = true
	}
	if len(seen) != 2 {
		t.Errorf("distinct session keys covered %d virtuals, want 2 (%v)", len(seen), seen)
	}

	// Non-pooled provider: Expand passes through; Pick on a built non-pooled name
	// returns it, on a not-built name returns !ok.
	p2 := newTestProxy(t, &Config{
		Listen:    "127.0.0.1:1",
		Providers: map[string]Provider{"single": {OpenAIBaseURL: "https://x", Provider: testProviderID}},
	})
	r2 := newResolver(p2, p2.providers, p2.poolIndex)
	if e := r2.Expand(RouteTarget{Provider: "single", Model: "m"}); len(e) != 1 || e[0].Provider != "single" {
		t.Errorf("Expand non-pooled = %+v, want [{single}]", e)
	}
	// "single" is a file-backed static provider (built even when not logged in) →
	// Pick returns it. A name with NO built impl (not in cfg.Providers at all) → !ok.
	if pick, ok := r2.Pick(RouteTarget{Provider: "single"}, ""); !ok || pick.Provider != "single" {
		t.Errorf("Pick built non-pooled = %+v ok=%v, want {single}/ok", pick, ok)
	}
	if _, ok := r2.Pick(RouteTarget{Provider: "does-not-exist"}, ""); ok {
		t.Error("Pick on a name with no built impl should be !ok")
	}

	// Health-aware failover: circuit-open the sticky account → Pick fails over to
	// the healthy sibling instead of returning the dead one.
	seedRuntimeCircuit(t, p, sessA.Provider, time.Now().Add(time.Hour))
	fallback, ok := r.Pick(RouteTarget{Provider: "zhipu", Model: "glm"}, "session-A")
	if !ok {
		t.Fatal("Pick session-A with sticky account circuit-open should fail over, got !ok")
	}
	if fallback.Provider == sessA.Provider {
		t.Errorf("Pick did not fail over from the circuit-open sticky account %q (still picked it)", sessA.Provider)
	}
	if p.parentOf[fallback.Provider] != "zhipu" {
		t.Error("failover pick is not a zhipu virtual")
	}

	// All accounts circuit-open → Pick !ok (no healthy virtual).
	other := fallback.Provider
	seedRuntimeCircuit(t, p, other, time.Now().Add(time.Hour))
	if _, ok := r.Pick(RouteTarget{Provider: "zhipu", Model: "glm"}, "session-A"); ok {
		t.Error("Pick with ALL accounts circuit-open should be !ok")
	}
}

// TestResolver_PickSkipsModelLockedVirtual (bug 6): the resolver is the health
// gate for Fusion panel/judge legs + Shadow. It used to check PROVIDER health
// only (circuit/rate-limit), so a model the main routing path had already
// locked (recordModelFailure) was still picked here — Fusion/Shadow kept
// requesting a known-bad (provider, model) until the lockout expired. Pick must
// treat a model-locked (virtual, model) as unavailable, like tryTarget does.
func TestResolver_PickSkipsModelLockedVirtual(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	writePoolFile(t, "zhipu", "zhipu", "KEY-A", "KEY-B")
	cfg := &Config{Listen: "127.0.0.1:1", Providers: map[string]Provider{
		"zhipu": {OpenAIBaseURL: "https://z", Provider: "zhipu"},
	}}
	p := newTestProxy(t, cfg)
	r := newResolver(p, p.providers, p.poolIndex)

	// Pre-lock: the model resolves to a healthy virtual.
	if _, ok := r.Pick(RouteTarget{Provider: "zhipu", Model: "glm"}, "session-1"); !ok {
		t.Fatal("pre-lock Pick returned !ok; want a healthy virtual")
	}

	// Lock the model on EVERY virtual (the main routing path would skip them all).
	for _, vid := range p.poolIndex["zhipu"] {
		p.recordModelFailure(vid, "glm", Scheduling{ModelLockout: "1h"})
	}
	if _, ok := r.Pick(RouteTarget{Provider: "zhipu", Model: "glm"}, "session-1"); ok {
		t.Errorf("post-lock Pick returned ok for a fully model-locked pool; resolver must skip locked (provider,model)")
	}

	// A DIFFERENT model on the same pool is unaffected — the lock is model-specific.
	if _, ok := r.Pick(RouteTarget{Provider: "zhipu", Model: "glm-other"}, "session-1"); !ok {
		t.Errorf("unlocked model on same pool should still resolve")
	}
}
