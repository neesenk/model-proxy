package app

import (
	"bytes"
	"encoding/json"
	"io"
	"model-proxy/internal/provider"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- proxy_routing_test.go ----

// TestForward_ProviderRouting_SplitsByModel verifies that the proxy routes
// requests to different providers based on the route's model→provider/model map.
func TestForward_ProviderRouting_SplitsByModel(t *testing.T) {
	var codexHit, gwHit requestHit
	codexUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		codexHit = captureHit(r)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer codexUp.Close()
	gwUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gwHit = captureHit(r)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer gwUp.Close()

	cfg := &Config{

		Providers: map[string]Provider{
			"codex": {OpenAIBaseURL: codexUp.URL, Provider: testProviderID},
			"aqp":   {OpenAIBaseURL: gwUp.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"gpt-5.5": {{Provider: "codex", Model: "gpt-5.5"}},
			"glm-5.2": {{Provider: "aqp", Model: "glm-5.2"}},
		},
	}
	p := newTestProxy(t, cfg)
	// Override both providers' auth with known tokens for deterministic test.
	p.providers["codex"] = &testProv{key: "codex-token"}
	p.providers["aqp"] = &testProv{key: "gw-key"}

	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	// 1) gpt-5.5 → codex provider
	codexHit, gwHit = requestHit{}, requestHit{}
	postOK(t, px.URL+"/v1/responses", `{"model":"gpt-5.5","input":[]}`)
	if codexHit.path == "" {
		t.Error("gpt-5.5: expected to hit codex backend")
	}
	if gwHit.path != "" {
		t.Error("gpt-5.5: should not hit aqp backend")
	}
	if codexHit.auth != "Bearer codex-token" {
		t.Errorf("gpt-5.5 auth=%q want Bearer codex-token", codexHit.auth)
	}
	// P2-1: assert the model field was rewritten to the upstream model name
	if codexHit.model != "gpt-5.5" {
		t.Errorf("gpt-5.5 model rewrite: upstream model=%q want gpt-5.5", codexHit.model)
	}

	// 2) glm-5.2 → aqp provider
	codexHit, gwHit = requestHit{}, requestHit{}
	postOK(t, px.URL+"/v1/responses", `{"model":"glm-5.2","input":[]}`)
	if gwHit.path == "" {
		t.Error("glm-5.2: expected to hit aqp backend")
	}
	if codexHit.path != "" {
		t.Error("glm-5.2: should not hit codex backend")
	}
	if gwHit.auth != "Bearer gw-key" {
		t.Errorf("glm-5.2 auth=%q want Bearer gw-key", gwHit.auth)
	}
	// P2-1: assert model rewrite
	if gwHit.model != "glm-5.2" {
		t.Errorf("glm-5.2 model rewrite: upstream model=%q want glm-5.2", gwHit.model)
	}
}

// TestForward_UnknownModel errors when the model isn't in the route.
func TestForward_UnknownModel(t *testing.T) {
	gwUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer gwUp.Close()
	cfg := &Config{

		Providers: map[string]Provider{
			"aqp": {OpenAIBaseURL: gwUp.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"gpt-5.5": {{Provider: "aqp", Model: "gpt-5.5"}},
		},
	}
	p := newTestProxy(t, cfg)
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()
	resp, err := http.Post(px.URL+"/v1/responses", "application/json", stringReader(`{"model":"unknown"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Errorf("expected 502 for unknown model, got %d", resp.StatusCode)
	}
}

type requestHit struct {
	path  string
	auth  string
	model string
}

func captureHit(r *http.Request) requestHit {
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	var v struct {
		Model string `json:"model"`
	}
	json.Unmarshal(body, &v)
	return requestHit{path: r.URL.Path, auth: r.Header.Get("Authorization"), model: v.Model}
}

func post(t *testing.T, url, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", stringReader(body))
	if err != nil {
		t.Fatal(err)
	}
	b, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil {
		t.Fatalf("post %s: read response: %v", url, readErr)
	}
	if closeErr != nil {
		t.Fatalf("post %s: close response: %v", url, closeErr)
	}
	return resp.StatusCode, string(b)
}

// postOK posts and requires the client-visible status to be 200 — forward tests
// must assert the client outcome, not only upstream hit counts (testing.md
// "Route side effects").
func postOK(t *testing.T, url, body string) {
	t.Helper()
	code, respBody := post(t, url, body)
	if code != http.StatusOK {
		t.Fatalf("post %s: status=%d body=%s, want 200", url, code, respBody)
	}
}

func stringReader(s string) io.Reader { return &stringReaderImpl{s: s} }

type stringReaderImpl struct {
	s   string
	pos int
}

func (r *stringReaderImpl) Read(p []byte) (int, error) {
	if r.pos >= len(r.s) {
		return 0, io.EOF
	}
	n := copy(p, r.s[r.pos:])
	r.pos += n
	return n, nil
}

// testProv implements provider.Provider for deterministic tests.
type testProv struct {
	key string
}

func (t *testProv) AuthHeaders(req *http.Request) error {
	req.Header.Set("Authorization", "Bearer "+t.key)
	req.Header.Del("x-api-key")
	return nil
}
func (t *testProv) Refresh() error { return nil }
func (t *testProv) RewriteRequest(url string, body []byte, path string) (string, []byte) {
	return url, body
}
func (t *testProv) Logout() error                           { return nil }
func (t *testProv) Usage() error                            { return nil }
func (t *testProv) FetchModels() ([]string, error)          { return nil, nil }
func (t *testProv) Quota() (*provider.QuotaSnapshot, error) { return nil, nil }

// ProbeRequest/ExtraHeaders/FilterModelIDs: testProv uses OpenAI-style defaults
// (matches baseProbe). Inlined rather than embedding baseProbe so the test stub
// stays self-contained and readable.
func (t *testProv) ProbeRequest(modelID string) provider.ProbeRequest {
	return provider.ProbeRequest{
		Method: http.MethodPost,
		Path:   "/chat/completions",
		Body:   []byte(`{"model":"` + modelID + `","messages":[{"role":"user","content":"hi"}],"max_tokens":1}`),
	}
}
func (t *testProv) ExtraHeaders(req *http.Request, path string)          {}
func (t *testProv) FilterModelIDs(ids []string) (kept, dropped []string) { return ids, nil }

var _ provider.Provider = (*testProv)(nil)

// TestRouteExpansionFansOutPool verifies that a route target naming a pooled
// parent is fanned out to its N virtual children at build time: each expanded
// target carries the SAME Model + Priority as the original target, and its
// Provider is a virtual id present in poolIndex (parentOf[vid] == parent). A
// non-pooled provider passes through unchanged.
//
// This is the load-bearing routing test for the credential-pool feature: if
// expansion silently dropped targets, forwarded the parent name, or lost the
// Model/Priority, conversations would hit the wrong upstream or 404.
func TestRouteExpansionFansOutPool(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	writePoolFile(t, "zhipu", "zhipu", "KEY-A", "KEY-B")

	cfg := &Config{
		Listen:    "127.0.0.1:1",
		Providers: map[string]Provider{"zhipu": {OpenAIBaseURL: "https://z", Provider: "zhipu"}},
		Routes:    map[string][]RouteTarget{"glm-5": {{Provider: "zhipu", Model: "glm-5", Priority: 7}}},
	}
	p := newTestProxy(t, cfg)
	got := p.expandedRoutes["glm-5"]
	if len(got) != 2 {
		t.Fatalf("expanded len = %d, want 2 (%v)", len(got), got)
	}
	seen := map[string]bool{}
	for _, tg := range got {
		if tg.Model != "glm-5" {
			t.Fatalf("expanded model = %q, want glm-5", tg.Model)
		}
		if tg.Priority != 7 {
			t.Fatalf("expanded priority = %d, want 7 (must be preserved from parent target)", tg.Priority)
		}
		parent := p.parentOf[tg.Provider]
		if parent != "zhipu" {
			t.Fatalf("expanded target provider %q not a zhipu virtual (parentOf=%q)", tg.Provider, parent)
		}
		if !p.poolIndexHas("zhipu", tg.Provider) {
			t.Fatalf("expanded target %q not listed in poolIndex[zhipu]=%v", tg.Provider, p.poolIndex["zhipu"])
		}
		if seen[tg.Provider] {
			t.Fatalf("virtual %q appears twice in expansion (dedup broken)", tg.Provider)
		}
		seen[tg.Provider] = true
	}
	// The parent name itself must NOT appear as a runnable target after expansion.
	for _, tg := range got {
		if tg.Provider == "zhipu" {
			t.Fatalf("parent name %q leaked into expanded targets: %v", "zhipu", got)
		}
	}

	// Non-pooled provider passes through unchanged (single-account / not-logged-in
	// → loadPool returns 0 accounts → not in poolIndex → passthrough).
	cfg2 := &Config{
		Listen:    "127.0.0.1:1",
		Providers: map[string]Provider{"z": {OpenAIBaseURL: "https://z", Provider: "zhipu"}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "z", Model: "m"}}},
	}
	p2 := newTestProxy(t, cfg2)
	got2 := p2.expandedRoutes["m"]
	if len(got2) != 1 || got2[0].Provider != "z" || got2[0].Model != "m" {
		t.Fatalf("non-pooled target should pass through unchanged: got %v", got2)
	}
}

// scheduleFirst returns the provider the scheduler tries first for an exposed
// model + session, via the live schedule() path (commits sticky). Used by the
// session-sticky tests to assert per-session round-robin assignment.
func scheduleFirst(p *Proxy, exposed, sessionKey string) string {
	routeKeys := make(map[string]bool, len(p.expandedRoutes))
	for k := range p.expandedRoutes {
		routeKeys[k] = true
	}
	ordered := p.schedule(p.cfg, p.parentOf, exposed, sessionKey, p.expandedRoutes[exposed], routeKeys)
	if len(ordered) == 0 {
		return ""
	}
	return ordered[0].Provider
}

// TestSessionStickySpreadsSessions verifies that distinct session ids land on
// distinct pool accounts via per-parent round-robin assignment (the heart of
// session-sticky: different conversations → different accounts for concurrency).
//
// The round-robin band is the parent's virtual ids SORTED by id (stable), and
// spreadCtr starts at 0, so the assignment is deterministic: 1st new session →
// band[0], 2nd → band[1], 3rd → band[2]. Assert the EXACT account each session
// lands on — a count-only check would pass even if round-robin handed two
// sessions the same account and skipped a third (a real green-signal risk).
func TestSessionStickySpreadsSessions(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	writePoolFile(t, "zhipu", "zhipu", "KEY-A", "KEY-B", "KEY-C")
	cfg := &Config{Listen: "127.0.0.1:1",
		Providers: map[string]Provider{"zhipu": {OpenAIBaseURL: "https://z", Provider: "zhipu"}},
		Routes:    map[string][]RouteTarget{"glm-5": {{Provider: "zhipu", Model: "glm-5"}}}}
	p := newTestProxy(t, cfg)
	sortedVids := append([]string(nil), p.poolIndex["zhipu"]...)
	sort.Strings(sortedVids)
	want := map[string]string{
		"s1": sortedVids[0],
		"s2": sortedVids[1],
		"s3": sortedVids[2],
	}
	for _, sid := range []string{"s1", "s2", "s3"} {
		got := scheduleFirst(p, "glm-5", sid)
		if got != want[sid] {
			t.Fatalf("session %s landed on %s, want %s (sorted band %v)", sid, got, want[sid], sortedVids)
		}
	}
}

// TestSessionStickyReusesWithinDwell verifies that the SAME session id reuses
// its parked account within the dwell window (cache-friendly: one conversation
// stays on one account so its prompt cache stays warm).
func TestSessionStickyReusesWithinDwell(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	writePoolFile(t, "zhipu", "zhipu", "KEY-A", "KEY-B")
	cfg := &Config{Listen: "127.0.0.1:1",
		Providers: map[string]Provider{"zhipu": {OpenAIBaseURL: "https://z", Provider: "zhipu"}},
		Routes:    map[string][]RouteTarget{"glm-5": {{Provider: "zhipu", Model: "glm-5"}}}}
	p := newTestProxy(t, cfg)
	first := scheduleFirst(p, "glm-5", "s1")
	if first == "" {
		t.Fatal("first schedule returned no provider")
	}
	for i := 0; i < 3; i++ {
		if got := scheduleFirst(p, "glm-5", "s1"); got != first {
			t.Fatalf("same session drifted: first=%s got=%s", first, got)
		}
	}
}

// TestSessionStickyFallsBackWithoutHeader verifies that a client WITHOUT a
// session header falls back to the model-keyed sticky path (all its requests
// pin to one account) — the pre-session behavior must be preserved.
func TestSessionStickyFallsBackWithoutHeader(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	writePoolFile(t, "zhipu", "zhipu", "KEY-A", "KEY-B", "KEY-C")
	cfg := &Config{Listen: "127.0.0.1:1",
		Providers: map[string]Provider{"zhipu": {OpenAIBaseURL: "https://z", Provider: "zhipu"}},
		Routes:    map[string][]RouteTarget{"glm-5": {{Provider: "zhipu", Model: "glm-5"}}}}
	p := newTestProxy(t, cfg)
	got := map[string]bool{}
	for i := 0; i < 5; i++ {
		got[scheduleFirst(p, "glm-5", "")] = true
	}
	if len(got) != 1 {
		t.Fatalf("no session header should pin to one account, got %v", got)
	}
}

// TestSessionStickySkipsCircuitOpen verifies that a circuit-opened account is
// skipped when assigning new sessions: the round-robin band is built from
// AVAILABLE targets only, so a blocked account is never handed out.
//
// Deterministic: blocked is the id-sorted first virtual, so the available band
// is [sortedVids[1], sortedVids[2]]; spreadCtr starts at 0 → s1 lands on
// sortedVids[1], s2 on sortedVids[2]. Assert exact accounts (not just a count)
// and that the blocked one is never picked.
func TestSessionStickySkipsCircuitOpen(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	writePoolFile(t, "zhipu", "zhipu", "KEY-A", "KEY-B", "KEY-C")
	cfg := &Config{Listen: "127.0.0.1:1",
		Providers: map[string]Provider{"zhipu": {OpenAIBaseURL: "https://z", Provider: "zhipu"}},
		Routes:    map[string][]RouteTarget{"glm-5": {{Provider: "zhipu", Model: "glm-5"}}}}
	p := newTestProxy(t, cfg)
	// Block the id-sorted first virtual for an hour.
	sortedVids := append([]string(nil), p.poolIndex["zhipu"]...)
	sort.Strings(sortedVids)
	blocked := sortedVids[0]
	seedRuntimeCircuit(t, p, blocked, time.Now().Add(time.Hour))
	// Available band = sortedVids minus blocked = [sortedVids[1], sortedVids[2]].
	want := map[string]string{
		"s1": sortedVids[1],
		"s2": sortedVids[2],
	}
	for _, sid := range []string{"s1", "s2"} {
		got := scheduleFirst(p, "glm-5", sid)
		if got == blocked {
			t.Fatalf("session %s landed on blocked account %s", sid, blocked)
		}
		if got != want[sid] {
			t.Fatalf("session %s landed on %s, want %s (blocked=%s, sortedVids=%v)", sid, got, want[sid], blocked, sortedVids)
		}
	}
}

// poolIndexHas reports whether vid is listed under parent in poolIndex.
func (p *Proxy) poolIndexHas(parent, vid string) bool {
	for _, v := range p.poolIndex[parent] {
		if v == vid {
			return true
		}
	}
	return false
}

// TestForward_ExpandedPooledRouteHitsVirtual verifies the end-to-end path:
// forward() must read expandedRoutes (not cfg.Routes), so a route targeting a
// pooled parent actually dispatches to one of its virtuals. This catches a bug
// where buildExpandedRoutes is correct but forward still reads the raw config.
func TestForward_ExpandedPooledRouteHitsVirtual(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	writePoolFile(t, "zhipu", "zhipu", "KEY-A", "KEY-B")

	var hits []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.Header.Get("Authorization"))
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()

	cfg := &Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]Provider{
			"zhipu": {OpenAIBaseURL: up.URL, Provider: "zhipu"},
		},
		Routes: map[string][]RouteTarget{
			"glm-5": {{Provider: "zhipu", Model: "glm-5"}},
		},
	}
	p := newTestProxy(t, cfg)
	// Sanity: the route was expanded to virtuals.
	if len(p.expandedRoutes["glm-5"]) != 2 {
		t.Fatalf("precondition: expanded len = %d, want 2", len(p.expandedRoutes["glm-5"]))
	}
	// Parent name must not be a runnable provider key.
	if _, ok := p.providers["zhipu"]; ok {
		t.Fatal("parent zhipu should not be in providers map (it's pooled)")
	}

	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	// Send a few requests; each must be served by a zhipu virtual (Bearer KEY-A
	// or KEY-B). The parent name "zhipu" would produce no upstream hit at all
	// (not in providers → provImpl nil → no auth → upstream rejects), so seeing
	// either virtual token proves forward used expandedRoutes.
	for i := 0; i < 4; i++ {
		post(t, px.URL+"/v1/responses", `{"model":"glm-5","input":[]}`)
	}
	if len(hits) == 0 {
		t.Fatal("no upstream hits — forward did not route to a virtual")
	}
	for _, h := range hits {
		if h != "Bearer KEY-A" && h != "Bearer KEY-B" {
			t.Fatalf("upstream auth = %q, want Bearer KEY-A or KEY-B (a pooled virtual)", h)
		}
	}
}

// TestExpandTarget_PreservesProtocol (regression #1): pool fan-out must copy
// Protocol so a pooled provider's cross-protocol route still converts and uses
// the backend's URL/path. expandTarget used to rebuild the RouteTarget with only
// Provider/Model/Priority, dropping Protocol — so a pooled OpenAI backend behind
// an Anthropic-exposed route lost its conversion and hit the wrong path.
func TestExpandTarget_PreservesProtocol(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	writePoolFile(t, "zhipu", "zhipu", "KEY-A", "KEY-B")

	cfg := &Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]Provider{
			"zhipu": {OpenAIBaseURL: "https://z", Provider: "zhipu"},
		},
		Routes: map[string][]RouteTarget{
			// Anthropic-exposed name routed to an OpenAI backend: needs conversion.
			"claude": {{Provider: "zhipu", Model: "glm-5", Priority: 2, Protocol: "openai"}},
		},
	}
	p := newTestProxy(t, cfg)

	got := p.expandedRoutes["claude"]
	if len(got) != 2 {
		t.Fatalf("expanded len = %d, want 2 (2-account pool)", len(got))
	}
	for i, tg := range got {
		if tg.Protocol != "openai" {
			t.Errorf("expanded[%d].Protocol = %q, want \"openai\" (dropped in fan-out → no conversion + wrong URL/path)", i, tg.Protocol)
		}
		if tg.Model != "glm-5" {
			t.Errorf("expanded[%d].model = %q, want glm-5", i, tg.Model)
		}
		if tg.Priority != 2 {
			t.Errorf("expanded[%d].Priority = %d, want 2", i, tg.Priority)
		}
		if p.parentOf[tg.Provider] != "zhipu" {
			t.Errorf("expanded[%d].Provider %q is not a zhipu virtual", i, tg.Provider)
		}
	}
}

// ---- proxy_routing_integration_test.go ----

// --- UC10: GET /v1/models returns exposed names ∪ claude_mapping keys ---

func TestUC_ModelsEndpointUnion(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{
			"aqp": {OpenAIBaseURL: "http://x", Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"glm-5.2":         {{Provider: "aqp", Model: "glm-5.2"}},
			"deepseek-v4-pro": {{Provider: "aqp", Model: "deepseek-v4-pro"}},
		},
		ClaudeMapping: map[string]string{
			"claude-opus-4-8":   "glm-5.2",
			"claude-sonnet-4-6": "deepseek-v4-pro",
		},
	}
	p := newTestProxy(t, cfg)
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	resp, err := http.Get(px.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/models: status = %d, want 200", resp.StatusCode)
	}
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("GET /v1/models: decode: %v", err)
	}
	resp.Body.Close()

	// Exact set: routes ∪ claude_mapping — a leak of any other name (over
	//exposure) is as wrong as a missing one.
	want := map[string]bool{
		"glm-5.2": true, "deepseek-v4-pro": true,
		"claude-opus-4-8": true, "claude-sonnet-4-6": true,
	}
	got := map[string]bool{}
	for _, m := range out.Data {
		if !want[m.ID] {
			t.Errorf("GET /v1/models exposed unexpected model %q (over-exposure); got %v", m.ID, out.Data)
		}
		got[m.ID] = true
	}
	for name := range want {
		if !got[name] {
			t.Errorf("GET /v1/models missing %q (routes ∪ claude_mapping); got %v", name, got)
		}
	}
}

// --- UC11: /debug/schedule reports sticky + dwell remaining ---

func TestUC_DebugScheduleReportsSticky(t *testing.T) {
	up, _ := newCaptureUpstream(200, `{}`)
	defer up.Close()
	cfg := &Config{
		Providers: map[string]Provider{
			"a": {OpenAIBaseURL: up.URL, Provider: testProviderID},
			"b": {OpenAIBaseURL: up.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"m1": {
				{Provider: "a", Model: "m1", Priority: 1},
				{Provider: "b", Model: "m1", Priority: 2},
			},
		},
	}
	p := newProxyWithStatic(t, cfg, map[string]string{"a": "a", "b": "b"})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	// Send a request so the route parks sticky on provider "a".
	postOK(t, px.URL+"/v1/responses", `{"model":"m1","input":[]}`)

	resp, err := http.Get(px.URL + "/debug/schedule")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Models map[string]struct {
			First    string  `json:"first"`
			Sticky   string  `json:"sticky"`
			DwellRem float64 `json:"sticky_dwell_remaining_sec"`
			Ordered  []struct {
				Provider  string  `json:"provider"`
				Tier      string  `json:"tier"`
				Available bool    `json:"available"`
				Peak      bool    `json:"peak"`
				Surplus   float64 `json:"surplus"`
			} `json:"ordered"`
		} `json:"models"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()

	m := out.Models["m1"]
	if m.Sticky != "a" {
		t.Errorf("sticky=%q want a (first request parks sticky)", m.Sticky)
	}
	if m.DwellRem <= 0 {
		t.Errorf("dwell_remaining=%v want >0 (within dwell window)", m.DwellRem)
	}
	if len(m.Ordered) != 2 {
		t.Errorf("ordered len=%d want 2", len(m.Ordered))
	}
	if m.Ordered[0].Provider != "a" {
		t.Errorf("ordered[0]=%q want a (sticky first)", m.Ordered[0].Provider)
	}
}

// --- UC12: sticky — two consecutive requests hit the same provider ---

func TestUC_StickySameProvider(t *testing.T) {
	var mu sync.Mutex
	hits := []string{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Write([]byte(`{}`))
	}))
	defer up.Close()
	cfg := &Config{
		Providers: map[string]Provider{
			"a": {OpenAIBaseURL: up.URL, Provider: testProviderID},
			"b": {OpenAIBaseURL: up.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"m1": {
				{Provider: "a", Model: "m1", Priority: 1},
				{Provider: "b", Model: "m1", Priority: 2},
			},
		},
	}
	p := newProxyWithStatic(t, cfg, map[string]string{"a": "a-key", "b": "b-key"})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	for i := 0; i < 3; i++ {
		post(t, px.URL+"/v1/responses", `{"model":"m1","input":[]}`)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hits) != 3 {
		t.Fatalf("hits=%d want 3", len(hits))
	}
	for _, h := range hits {
		if h != "Bearer a-key" {
			t.Errorf("sticky expected all hits on provider a (Bearer a-key), got %q", h)
		}
	}
}

// --- UC13: aqp /messages gets ?beta=true + anthropic-version + x-compass-request-id ---

func TestUC_AqpBetaAndHeaders(t *testing.T) {
	var gotURL, gotAV, gotRID string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURL = r.URL.String()
		gotAV = r.Header.Get("anthropic-version")
		gotRID = r.Header.Get("x-compass-request-id")
		w.Write([]byte(`{}`))
	}))
	defer up.Close()
	cfg := &Config{
		Providers: map[string]Provider{
			"aqp": {OpenAIBaseURL: up.URL, AnthropicBaseURL: up.URL, Provider: "aqp"},
		},
		Routes: map[string][]RouteTarget{
			"glm-5.2": {{Provider: "aqp", Model: "glm-5.2"}},
		},
	}
	p := newTestProxy(t, cfg)
	// Build a REAL AqpProvider (so RewriteRequest adds ?beta) with a fake
	// Authenticator, so no auth file is read.
	p.providers["aqp"] = mustRealProvider(t, "aqp", &provider.Config{
		ProviderID:    "aqp",
		OpenAIBaseURL: up.URL,
		Auth:          fakeAuth{key: "k"},
	})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	postOK(t, px.URL+"/v1/messages", `{"model":"glm-5.2","messages":[]}`)

	if !strings.Contains(gotURL, "beta=true") {
		t.Errorf("upstream URL=%q missing beta=true (aqp /messages needs it)", gotURL)
	}
	if gotAV != "2023-06-01" {
		t.Errorf("anthropic-version=%q want 2023-06-01", gotAV)
	}
	if gotRID == "" {
		t.Error("x-compass-request-id empty (aqp requests must set a UUID)")
	}
}

// --- UC14: codex request gets store:false injected ---

func TestUC_CodexStoreFalseInjected(t *testing.T) {
	var gotBody string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Write([]byte(`{}`))
	}))
	defer up.Close()
	cfg := &Config{
		Providers: map[string]Provider{
			"codex": {OpenAIBaseURL: up.URL, Provider: "codex"},
		},
		Routes: map[string][]RouteTarget{
			"gpt-5.5": {{Provider: "codex", Model: "gpt-5.5"}},
		},
	}
	p := newTestProxy(t, cfg)
	// Build a REAL CodexProvider (so RewriteRequest injects store:false) with a
	// fake Authenticator, so no auth file is read.
	p.providers["codex"] = mustRealProvider(t, "codex", &provider.Config{
		ProviderID:    "codex",
		OpenAIBaseURL: up.URL,
		Auth:          fakeAuth{key: "k"},
	})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	postOK(t, px.URL+"/v1/responses", `{"model":"gpt-5.5","input":[]}`)

	var m struct {
		Store bool `json:"store"`
	}
	if err := json.Unmarshal([]byte(gotBody), &m); err != nil {
		t.Fatalf("body not JSON: %s", gotBody)
	}
	if m.Store != false {
		t.Errorf("store=%v want false (codex backend requires store:false)", m.Store)
	}
}

// --- UC15: deepseek dual-protocol — anthropic→anthropic_base_url, openai→openai_base_url ---

func TestUC_DeepSeekDualProtocolBaseURL(t *testing.T) {
	var anthropicHit, openaiHit string
	anthUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anthropicHit = r.URL.Path
		w.Write([]byte(`{}`))
	}))
	defer anthUp.Close()
	oaiUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		openaiHit = r.URL.Path
		w.Write([]byte(`{}`))
	}))
	defer oaiUp.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"deepseek": {
				OpenAIBaseURL:    oaiUp.URL,
				AnthropicBaseURL: anthUp.URL,
				Provider:         "deepseek",
			},
		},
		Routes: map[string][]RouteTarget{
			"deepseek-v4-pro": {{Provider: "deepseek", Model: "deepseek-v4-pro"}},
		},
	}
	p := newTestProxy(t, cfg)
	// deepseek provider sets both Bearer + x-api-key; use a key file via testProv override.
	p.providers["deepseek"] = &testProv{key: "ds-key"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	postOK(t, px.URL+"/v1/messages", `{"model":"deepseek-v4-pro","messages":[]}`)
	postOK(t, px.URL+"/v1/chat/completions", `{"model":"deepseek-v4-pro","messages":[]}`)

	if anthropicHit == "" {
		t.Error("anthropic request did not hit anthropic_base_url upstream")
	}
	if openaiHit == "" {
		t.Error("openai request did not hit openai_base_url upstream")
	}
	if anthropicHit == openaiHit {
		t.Errorf("both protocols hit the same upstream path %q (should use per-protocol base)", anthropicHit)
	}
}

// --- UC16: unknown path → 502; /health → 200 ---

func TestUC_UnknownPathAndHealth(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{"a": {OpenAIBaseURL: "http://x", Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"m1": {{Provider: "a", Model: "m1"}}},
	}
	p := newTestProxy(t, cfg)
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	// Unknown path → 502.
	resp, _ := http.Post(px.URL+"/v1/whatever", "application/json", stringReader(`{"model":"m1"}`))
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Errorf("unknown path status=%d want 502", resp.StatusCode)
	}

	// /health → 200.
	resp, _ = http.Get(px.URL + "/health")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("/health status=%d want 200", resp.StatusCode)
	}

	// /health/status → 200.
	resp, _ = http.Get(px.URL + "/health/status")
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("/health/status status=%d want 200", resp.StatusCode)
	}
}

// --- UC17: missing model field → 400 ---

func TestUC_MissingModelField400(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{"a": {OpenAIBaseURL: "http://x", Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"m1": {{Provider: "a", Model: "m1"}}},
	}
	p := newTestProxy(t, cfg)
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	resp, _ := http.Post(px.URL+"/v1/responses", "application/json", stringReader(`{"input":[]}`))
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("missing model: status=%d want 400", resp.StatusCode)
	}
}

// --- UC18: unparseable JSON body → 400 ---

func TestUC_UnparseableBody400(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{"a": {OpenAIBaseURL: "http://x", Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"m1": {{Provider: "a", Model: "m1"}}},
	}
	p := newTestProxy(t, cfg)
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	resp, _ := http.Post(px.URL+"/v1/responses", "application/json", stringReader(`not-json`))
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("unparseable body: status=%d want 400", resp.StatusCode)
	}
}

// --- UC extra: header whitelist — client Authorization is NOT forwarded ---

func TestUC_ClientAuthNotForwarded(t *testing.T) {
	var gotAuth, gotCookie string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCookie = r.Header.Get("Cookie")
		w.Write([]byte(`{}`))
	}))
	defer up.Close()
	cfg := &Config{
		Providers: map[string]Provider{"a": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"m1": {{Provider: "a", Model: "m1"}}},
	}
	p := newProxyWithStatic(t, cfg, map[string]string{"a": "proxy-key"})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	req, _ := http.NewRequest("POST", px.URL+"/v1/responses", stringReader(`{"model":"m1","input":[]}`))
	req.Header.Set("Authorization", "Bearer CLIENT-SECRET") // must NOT reach upstream
	req.Header.Set("Cookie", "SSO_C=secret")                // must NOT reach upstream
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if gotAuth != "Bearer proxy-key" {
		t.Errorf("upstream Authorization=%q want Bearer proxy-key (client secret must not leak)", gotAuth)
	}
	if gotCookie != "" {
		t.Errorf("upstream Cookie=%q want empty (client cookie must not leak through the header whitelist)", gotCookie)
	}
}

// ensure bytes import is used (some build configs need it)
var _ = bytes.MinRead

// ---- request_routing_adapter_test.go ----

func TestForceProvider(t *testing.T) {
	request := &http.Request{
		Header: http.Header{"X-Mp-Force-Provider": []string{"header-provider"}},
		URL: &url.URL{
			RawQuery: "force_provider=query-provider",
		},
	}
	if got := forcedProviderFromRequest(request); got != "header-provider" {
		t.Errorf("header precedence = %q, want header-provider", got)
	}
	request.Header.Del("x-mp-force-provider")
	if got := forcedProviderFromRequest(request); got != "query-provider" {
		t.Errorf("query fallback = %q, want query-provider", got)
	}
	if got := forcedProviderFromRequest(nil); got != "" {
		t.Errorf("nil request = %q, want empty", got)
	}
	if got := forcedProviderFromRequest(&http.Request{}); got != "" {
		t.Errorf("nil URL = %q, want empty", got)
	}
}
