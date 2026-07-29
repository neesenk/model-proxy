package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"model-proxy/provider"
)

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

	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	// 1) gpt-5.5 → codex provider
	codexHit, gwHit = requestHit{}, requestHit{}
	post(t, px.URL+"/v1/responses", `{"model":"gpt-5.5","input":[]}`)
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
	post(t, px.URL+"/v1/responses", `{"model":"glm-5.2","input":[]}`)
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
	px := httptest.NewServer(http.HandlerFunc(p.handler))
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
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
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

	px := httptest.NewServer(http.HandlerFunc(p.handler))
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
			t.Errorf("expanded[%d].Model = %q, want glm-5", i, tg.Model)
		}
		if tg.Priority != 2 {
			t.Errorf("expanded[%d].Priority = %d, want 2", i, tg.Priority)
		}
		if p.parentOf[tg.Provider] != "zhipu" {
			t.Errorf("expanded[%d].Provider %q is not a zhipu virtual", i, tg.Provider)
		}
	}
}
