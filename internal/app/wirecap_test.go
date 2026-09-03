package app

// wirecap_test.go — application integration for wire capability probing:
// probe execution against mock upstreams, end-to-end protocol selection
// through proxy.Handler, runtime 404 correction, and persistence round-trips.
// Pure verdict, policy, and Store behavior belongs to internal/runtime/wirecap.
// Probing is synchronous in tests (probeAllWireCaps called directly —
// production dispatches it asynchronously via startWireCapProbe, which
// newProxyWithStatePath leaves disabled).

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"model-proxy/internal/probe"
)

// ---------------------------------------------------------------------------
// probe execution
// ---------------------------------------------------------------------------

// TestWireCap_ProbeProviders: eligible providers are probed (chat + responses
// legs on the openai base, model from provider.Models[0]); codex
// (ProtocolHint-covered) is skipped; a fresh verdict is not re-probed.
// Anthropic is NOT probed — it's config-declared via anthropic_base_url.
func TestWireCap_ProbeProviders(t *testing.T) {
	type hit struct{ path, body string }
	var hits []hit
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		hits = append(hits, hit{r.URL.Path, string(b)})
		if r.URL.Path == "/responses" {
			w.Header().Set("content-type", "application/json")
			w.Write([]byte(`{"id":"r1","status":"completed","output":[]}`))
			return
		}
		w.WriteHeader(http.StatusNotFound) // /chat/completions does not exist
	}))
	defer up.Close()
	var codexHits int
	codexUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		codexHits++
		w.Write([]byte(`{}`))
	}))
	defer codexUp.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"p":   {OpenAIBaseURL: up.URL, Provider: testProviderID, Models: []string{"m-probe"}},
			"cdx": {OpenAIBaseURL: codexUp.URL, Provider: "codex"},
		},
		Routes: map[string][]RouteTarget{},
	}
	p := newTestProxy(t, cfg)
	p.providers["p"] = &testProv{key: "k"}
	p.providers["cdx"] = &testProv{key: "k"}

	p.probeAllWireCaps()

	caps, ok := p.wireVerdict("p")
	if !ok {
		t.Fatal("provider p not probed")
	}
	if caps.Responses != triYes || caps.Chat != triNo {
		t.Errorf("verdict = responses:%s chat:%s, want yes/no", caps.Responses, caps.Chat)
	}
	if caps.BaseURL != up.URL {
		t.Errorf("verdict base_url = %q", caps.BaseURL)
	}
	// Both legs probed with provider.Models[0].
	if len(hits) != 2 {
		t.Fatalf("upstream hits = %d, want 2: %+v", len(hits), hits)
	}
	for _, h := range hits {
		if !strings.Contains(h.body, `"m-probe"`) {
			t.Errorf("probe body missing model m-probe: %s", h.body)
		}
		if h.path != "/responses" && h.path != "/chat/completions" {
			t.Errorf("unexpected probe path %q", h.path)
		}
	}
	// codex skipped (hint-covered).
	if codexHits != 0 {
		t.Errorf("codex provider probed (%d hits), want 0", codexHits)
	}
	if _, ok := p.wireVerdict("cdx"); ok {
		t.Error("codex must not get a wire verdict")
	}

	// Fresh verdict → second pass is a no-op.
	p.probeAllWireCaps()
	if len(hits) != 2 {
		t.Errorf("re-probe hit upstream %d times, want 2 total (fresh verdict skipped)", len(hits))
	}
}

// TestWireCap_ProbeNeverFabricatesAnthropicOnOpenAIBase: the anthropic
// protocol is never probed on the openai base — even for a provider WITH
// anthropic_base_url (anthropic support is config-declared; the model-level
// pass probes it on the anthropic base instead). Both openai legs are probed
// and the freshness check skips re-probes.
func TestWireCap_ProbeNeverFabricatesAnthropicOnOpenAIBase(t *testing.T) {
	var paths []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		paths = append(paths, r.URL.Path)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"r1","status":"completed","output":[]}`))
	}))
	defer up.Close()
	cfg := &Config{
		Providers: map[string]Provider{
			"p": {OpenAIBaseURL: up.URL, AnthropicBaseURL: "http://anthropic-unused", Provider: testProviderID, Models: []string{"m"}},
		},
		Routes: map[string][]RouteTarget{},
	}
	p := newTestProxy(t, cfg)
	p.providers["p"] = &testProv{key: "k"}

	p.probeAllWireCaps()

	if len(paths) != 2 {
		t.Fatalf("probe paths = %v, want exactly [/chat/completions /responses]", paths)
	}
	for _, path := range paths {
		if path == "/v1/messages" {
			t.Fatalf("anthropic fabricated on the openai base: %v", paths)
		}
	}
	caps, ok := p.wireVerdict("p")
	if !ok {
		t.Fatal("provider p not probed")
	}
	if caps.Responses != triYes || caps.Chat != triYes {
		t.Errorf("verdict = responses:%s chat:%s, want yes/yes", caps.Responses, caps.Chat)
	}
	// Fresh → second pass is a no-op.
	p.probeAllWireCaps()
	if len(paths) != 2 {
		t.Errorf("re-probe hit upstream %d times, want 2 total (fresh verdict skipped)", len(paths))
	}
}

// TestWireCap_StaleNegativeVerdictIsReprobed: a "no" verdict expires after
// wireCapNegativeTTL (a transient 404 must not downgrade a provider forever);
// a fresh "no" is still trusted and skips re-probing.
func TestWireCap_StaleNegativeVerdictIsReprobed(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		hits.Add(1)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"r1","status":"completed","output":[]}`))
	}))
	defer up.Close()
	cfg := &Config{
		Providers: map[string]Provider{
			"p": {OpenAIBaseURL: up.URL, Provider: testProviderID, Models: []string{"m"}},
		},
		Routes: map[string][]RouteTarget{},
	}
	p := newTestProxy(t, cfg)
	p.providers["p"] = &testProv{key: "k"}

	p.setWireCaps("p", wireCaps{BaseURL: up.URL, Responses: triNo, Chat: triNo,
		ProbedAt: time.Now().Add(-2 * wireCapNegativeTTL)})
	p.probeAllWireCaps()
	if hits.Load() == 0 {
		t.Fatal("stale negative verdict was not re-probed")
	}
	caps, _ := p.wireVerdict("p")
	if caps.Responses != triYes {
		t.Fatalf("verdict after re-probe = %s, want yes", caps.Responses)
	}

	hits.Store(0)
	p.setWireCaps("p", wireCaps{BaseURL: up.URL, Responses: triNo, Chat: triYes, ProbedAt: time.Now()})
	p.probeAllWireCaps()
	if hits.Load() != 0 {
		t.Fatalf("fresh negative verdict re-probed (%d hits), want 0", hits.Load())
	}
}

// TestWireCap_ProbeModelSelection: without provider.Models, the probe model
// comes from the first route target (probe.PickModel, shared with wire record).
func TestWireCap_ProbeModelSelection(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Write([]byte(`{}`))
	}))
	defer up.Close()
	cfg := &Config{
		Providers: map[string]Provider{"p": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"m1": {{Provider: "p", Model: "m1"}}},
	}
	if got := probe.PickModel(cfg, nil, "p"); got != "m1" {
		t.Errorf("PickModel = %q, want m1 (route target)", got)
	}
	cfg.Providers["p"] = Provider{OpenAIBaseURL: up.URL, Provider: testProviderID, Models: []string{"m0"}}
	if got := probe.PickModel(cfg, nil, "p"); got != "m0" {
		t.Errorf("PickModel = %q, want m0 (provider.Models[0] wins)", got)
	}
}

// ---------------------------------------------------------------------------
// end-to-end protocol selection
// ---------------------------------------------------------------------------

// TestWireCap_Forward_AnthropicToResponses: an anthropic client behind a route
// with NO protocol: declaration gets converted to responses once the verdict
// says the endpoint supports it (upstream sees an input-list body).
func TestWireCap_Forward_AnthropicToResponses(t *testing.T) {
	var gotBody, gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotPath = string(b), r.URL.Path
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer up.Close()
	cfg := &Config{
		Providers: map[string]Provider{"oai": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"claude-x": {{Provider: "oai", Model: "gpt-x"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["oai"] = &testProv{key: "k"}
	p.setWireCaps("oai", wireCaps{BaseURL: up.URL, Responses: triYes, Chat: triNo, ProbedAt: time.Now()})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	resp, err := http.Post(px.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"claude-x","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read response: %v", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("client status = %d, want 200: %s", resp.StatusCode, body)
	}

	if gotPath != "/responses" {
		t.Errorf("upstream path = %q, want /responses", gotPath)
	}
	if !strings.Contains(gotBody, `"input"`) || strings.Contains(gotBody, `"messages"`) {
		t.Errorf("upstream got non-responses body: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"model":"gpt-x"`) {
		t.Errorf("upstream model not rewritten: %s", gotBody)
	}
	if !strings.Contains(string(body), `"type":"message"`) {
		t.Errorf("client did not get an anthropic response: %s", body)
	}
}

// TestWireCap_Forward_ResponsesToChatWhenNo: a responses client behind a
// provider whose verdict says responses=no gets converted to chat.
func TestWireCap_Forward_ResponsesToChatWhenNo(t *testing.T) {
	var gotBody, gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotPath = string(b), r.URL.Path
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"c1","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer up.Close()
	cfg := &Config{
		Providers: map[string]Provider{"oai": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"gpt-x": {{Provider: "oai", Model: "gpt-x"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["oai"] = &testProv{key: "k"}
	p.setWireCaps("oai", wireCaps{BaseURL: up.URL, Responses: triNo, Chat: triYes, ProbedAt: time.Now()})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	resp, err := http.Post(px.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"gpt-x","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read response: %v", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("client status = %d, want 200: %s", resp.StatusCode, body)
	}

	if gotPath != "/chat/completions" {
		t.Errorf("upstream path = %q, want /chat/completions", gotPath)
	}
	if !strings.Contains(gotBody, `"messages"`) {
		t.Errorf("upstream got non-chat body: %s", gotBody)
	}
	if !strings.Contains(gotBody, `"model":"gpt-x"`) {
		t.Errorf("upstream model not rewritten: %s", gotBody)
	}
	// Converted back to a responses-shaped client answer.
	if !strings.Contains(string(body), `"object":"response"`) {
		t.Errorf("client did not get a responses object: %s", body)
	}
}

// TestWireCap_Forward_AnthropicConvertsToChatWithoutAnthropicBase: without an
// anthropic_base_url the anthropic protocol is unsupported BY DEFINITION — the
// proxy converts to chat (responses verdict no) instead of byte-passthrough'ing
// an anthropic body onto the openai base (the old gateway-passthrough branch
// is gone).
func TestWireCap_Forward_AnthropicConvertsToChatWithoutAnthropicBase(t *testing.T) {
	var gotBody, gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotPath = string(b), r.URL.Path
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"c1","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer up.Close()
	cfg := &Config{
		Providers: map[string]Provider{"oai": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"claude-x": {{Provider: "oai", Model: "claude-x"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["oai"] = &testProv{key: "k"}
	p.setWireCaps("oai", wireCaps{BaseURL: up.URL, Responses: triNo, Chat: triYes, ProbedAt: time.Now()})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	resp, err := http.Post(px.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"claude-x","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read response: %v", readErr)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("client status = %d, want 200: %s", resp.StatusCode, body)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("upstream path = %q, want /chat/completions (converted, not passthrough)", gotPath)
	}
	if !strings.Contains(gotBody, `"messages"`) {
		t.Errorf("upstream got non-chat body: %s", gotBody)
	}
	if !strings.Contains(string(body), `"type":"message"`) {
		t.Errorf("client did not get an anthropic response: %s", body)
	}
}

// ---------------------------------------------------------------------------
// runtime 404 correction
// ---------------------------------------------------------------------------

// TestWireCap_Forward_404Correction: verdict said responses=yes but /responses
// 404s → the verdict flips to no (no model lock — our protocol miss, not a
// missing model) and the NEXT request goes out as chat.
func TestWireCap_Forward_404Correction(t *testing.T) {
	var bodies []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, r.URL.Path+" "+string(b))
		if r.URL.Path == "/responses" {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"error":{"message":"no such route"}}`))
			return
		}
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"c1","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer up.Close()
	cfg := &Config{
		Providers: map[string]Provider{"oai": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"claude-x": {{Provider: "oai", Model: "gpt-x"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["oai"] = &testProv{key: "k"}
	p.setWireCaps("oai", wireCaps{BaseURL: up.URL, Responses: triYes, Chat: triUnknown, ProbedAt: time.Now()})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	// Request 1: verdict-driven → /responses → 404 (committed to the client;
	// only one target).
	resp, err := http.Post(px.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"claude-x","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("request 1 client status = %d, want 404 (committed upstream verdict miss)", resp.StatusCode)
	}
	if len(bodies) != 1 || !strings.HasPrefix(bodies[0], "/responses ") {
		t.Fatalf("request 1 upstream bodies = %v, want one /responses call", bodies)
	}
	// Verdict flipped; NO model lock recorded.
	caps, _ := p.wireVerdict("oai")
	if caps.Responses != triNo {
		t.Errorf("post-404 verdict responses = %s, want no", caps.Responses)
	}
	locks := 0
	for _, entries := range p.runtimeState.Dashboard(time.Now()).ModelLocks {
		locks += len(entries)
	}
	if locks != 0 {
		t.Errorf("model locked %d time(s), want 0 (verdict miss is not a model failure)", locks)
	}

	// Request 2: verdict now no → converted to chat, upstream answers 200.
	resp2, err := http.Post(px.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"claude-x","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body2, readErr2 := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if readErr2 != nil {
		t.Fatalf("read response 2: %v", readErr2)
	}
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("request 2 client status = %d, want 200: %s", resp2.StatusCode, body2)
	}
	if len(bodies) != 2 || !strings.HasPrefix(bodies[1], "/chat/completions ") {
		t.Fatalf("request 2 upstream bodies = %v, want a /chat/completions call", bodies)
	}
	if !strings.Contains(string(body2), `"type":"message"`) {
		t.Errorf("client did not get an anthropic response: %s", body2)
	}
}

// TestWireCap_MissVerdictUsesRequestSnapshotParent (A2 regression): the
// wire-verdict 404 correction must resolve the pool parent from the REQUEST
// snapshot's ParentOf projection, not from live p.parentOf. Pre-fix
// noteWireResponsesMiss re-read the reload-owned map, so a pre-reload
// in-flight request whose virtual's parent changed across reload recorded
// the verdict under the NEW generation's parent name (single-snapshot red
// line violation).
func TestWireCap_MissVerdictUsesRequestSnapshotParent(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{
			"parent-old": {OpenAIBaseURL: "http://example.invalid", Provider: testProviderID},
			"parent-new": {OpenAIBaseURL: "http://example.invalid", Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{},
	}
	p := newTestProxy(t, cfg)

	// Generation 1: virtual "v" belongs to parent-old; the request captures
	// its snapshot here.
	p.mu.Lock()
	p.parentOf = map[string]string{"v": "parent-old"}
	p.mu.Unlock()
	snap := p.SnapshotRuntime()

	// Reload swaps the generation-owned map: "v" now belongs to parent-new.
	p.mu.Lock()
	p.parentOf = map[string]string{"v": "parent-new"}
	p.mu.Unlock()

	// The in-flight (generation-1) request's 404 correction resolves the
	// parent through its own snapshot — this is the gate targetExecutor binds
	// at assembly from RuntimeSnapshot.ParentOf.
	gate := proxyHealthGate{proxy: p, parentOf: snap.ParentOf}
	gate.NoteWireResponsesMiss("v", "")

	caps, ok := p.wireVerdict("parent-old")
	if !ok || caps.Responses != triNo {
		t.Errorf("verdict under request-generation parent = %+v (ok=%v), want responses=no", caps, ok)
	}
	if _, ok := p.wireVerdict("parent-new"); ok {
		t.Error("verdict recorded under the NEW generation's parent — request snapshot was bypassed")
	}
}

// ---------------------------------------------------------------------------
// persistence
// ---------------------------------------------------------------------------

// TestWireCap_PersistRoundTrip: verdicts land in quota_state.json under a
// top-level wire_caps key, restore on boot when base_url matches, drop when
// it doesn't, and survive reload.
func TestWireCap_PersistRoundTrip(t *testing.T) {
	useStaticProviderPools(t, "p")
	dir := t.TempDir()
	statePath := dir + "/quota_state.json"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) }))
	defer up.Close()
	mkCfg := func(baseURL string) *Config {
		return &Config{
			Providers: map[string]Provider{"p": {OpenAIBaseURL: baseURL, Provider: testProviderID}},
			Routes:    map[string][]RouteTarget{},
		}
	}

	p1 := newTestProxyAt(t, mkCfg(up.URL), statePath)
	p1.setWireCaps("p", wireCaps{BaseURL: up.URL, Responses: triYes, Chat: triNo, ProbedAt: time.Now()})
	if err := p1.quota.Persist(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if content := string(raw); !strings.Contains(content, `"wire_caps"`) || !strings.Contains(content, `"responses": "yes"`) {
		t.Errorf("state file missing wire_caps verdict:\n%s", content)
	}

	// Boot a second proxy on the same file: verdict restored.
	p2 := newTestProxyAt(t, mkCfg(up.URL), statePath)
	caps, ok := p2.wireVerdict("p")
	if !ok || caps.Responses != triYes || caps.Chat != triNo {
		t.Errorf("restored verdict = %+v (ok=%v), want yes/no", caps, ok)
	}

	// base_url changed → verdict invalidated.
	p3 := newTestProxyAt(t, mkCfg("http://other-endpoint.invalid"), statePath)
	if _, ok := p3.wireVerdict("p"); ok {
		t.Error("verdict restored despite base_url mismatch — must be invalidated")
	}

	// reload keeps the in-memory verdict (wireCaps is not cleared health state).
	cfgFile := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgFile, []byte("providers:\n  p: {provider_id: static, openai_base_url: "+up.URL+"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p2.Reload(cfgFile); err != nil {
		t.Fatal(err)
	}
	if _, ok := p2.wireVerdict("p"); !ok {
		t.Error("verdict lost across reload — wireCaps must survive (unlike health)")
	}
}

// wirecap probe timeout → unknown verdict (the timeout branch; 5xx is already
// table-covered in TestWireCap_Classify). wireCapProbeTimeout is a var so the
// test can shrink it instead of sleeping 10s.
func TestWireCap_ProbeTimeoutUnknown(t *testing.T) {
	old := wireCapProbeTimeout
	wireCapProbeTimeout = 50 * time.Millisecond
	defer func() { wireCapProbeTimeout = old }()

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond) // slower than the probe timeout
		w.Write([]byte(`{}`))
	}))
	defer up.Close()
	cfg := &Config{
		Providers: map[string]Provider{"p": {OpenAIBaseURL: up.URL, Provider: testProviderID, Models: []string{"m1"}}},
		Routes:    map[string][]RouteTarget{},
	}
	p := newTestProxy(t, cfg)
	p.providers["p"] = &testProv{key: "k"}
	p.probeAllWireCaps()
	caps, ok := p.wireVerdict("p")
	if !ok {
		t.Fatal("provider not probed")
	}
	if caps.Responses != triUnknown || caps.Chat != triUnknown {
		t.Errorf("timeout verdict = responses:%s chat:%s, want unknown/unknown (no negative conclusion cached)", caps.Responses, caps.Chat)
	}
}
