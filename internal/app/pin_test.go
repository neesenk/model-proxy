package app

import (
	"encoding/json"
	"io"
	"model-proxy/internal/appapi"
	configdomain "model-proxy/internal/config"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// setPinForTest installs a pin bypassing TTL clock semantics the test controls
// (helper so tests can set a pin + optionally a custom expiry in one place).
func setPinForTest(p *Proxy, route, provider string, ttl time.Duration) bool {
	_, ok := p.setPin(route, provider, ttl)
	return ok
}

// TestPin_ForcesProvider: a route with two targets (zhipu p1, deepseek p2) is
// pinned to the LOWER-priority deepseek; forward then hits deepseek ONLY, never
// zhipu, despite zhipu ranking first normally.
func TestPin_ForcesProvider(t *testing.T) {
	var zhipuHit, deepHit bool
	zhipuUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zhipuHit = true
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer zhipuUp.Close()
	deepUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deepHit = true
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer deepUp.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"zhipu":    {OpenAIBaseURL: zhipuUp.URL, Provider: testProviderID},
			"deepseek": {OpenAIBaseURL: deepUp.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"glm": {
				{Provider: "zhipu", Model: "glm", Priority: 1},
				{Provider: "deepseek", Model: "glm", Priority: 2},
			},
		},
	}
	p := newTestProxy(t, cfg)
	p.providers["zhipu"] = &testProv{key: "z"}
	p.providers["deepseek"] = &testProv{key: "d"}
	if !setPinForTest(p, "glm", "deepseek", 0) {
		t.Fatal("setPin deepseek failed")
	}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	postOK(t, px.URL+"/v1/responses", `{"model":"glm","input":[]}`)
	if !deepHit {
		t.Error("pinned provider deepseek was not hit")
	}
	if zhipuHit {
		t.Error("non-pinned zhipu was hit — pin must force the route to deepseek only")
	}
}

// TestPin_NoFailoverWhenPinned: a pin disables failover — the pinned provider
// returning 500 must NOT fall over to the other target.
func TestPin_NoFailoverWhenPinned(t *testing.T) {
	var zhipuHit bool
	zhipuUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zhipuHit = true
		w.Write([]byte(`{}`))
	}))
	defer zhipuUp.Close()
	deepUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte(`boom`))
	}))
	defer deepUp.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"zhipu":    {OpenAIBaseURL: zhipuUp.URL, Provider: testProviderID},
			"deepseek": {OpenAIBaseURL: deepUp.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"glm": {
				{Provider: "zhipu", Model: "glm", Priority: 1},
				{Provider: "deepseek", Model: "glm", Priority: 2},
			},
		},
	}
	p := newTestProxy(t, cfg)
	p.providers["zhipu"] = &testProv{key: "z"}
	p.providers["deepseek"] = &testProv{key: "d"}
	setPinForTest(p, "glm", "deepseek", 0)
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	resp, err := http.Post(px.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"glm","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("pinned+failed status=%d want 502 (no failover off the pin)", resp.StatusCode)
	}
	if zhipuHit {
		t.Error("failover escaped the pin to zhipu — pin must be exclusive")
	}
}

// TestSetPin_Validation: setPin rejects an unknown route and a provider the route
// can't reach (a pin that would silently do nothing).
func TestSetPin_Validation(t *testing.T) {
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{"zhipu": {OpenAIBaseURL: "https://x", Provider: testProviderID}},
		Routes:    map[string][]configdomain.RouteTarget{"glm": {{Provider: "zhipu", Model: "glm"}}},
	}
	p := newTestProxy(t, cfg)
	if setPinForTest(p, "ghost-route", "zhipu", 0) {
		t.Error("setPin unknown route should fail")
	}
	if setPinForTest(p, "glm", "deepseek", 0) {
		t.Error("setPin provider-not-in-route should fail")
	}
	if !setPinForTest(p, "glm", "zhipu", 0) {
		t.Error("setPin valid route+provider should succeed")
	}
}

// TestScheduleStatus_ShowsPin: /debug/schedule labels a pinned route with its
// pin provider + expiry, and OVERLAYS the pin on the default scheduling chain:
// `ordered` keeps the unpinned chain (what unpinning restores) while `first`
// stays the effective pin-applied choice.
func TestScheduleStatus_ShowsPin(t *testing.T) {
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"zhipu":    {OpenAIBaseURL: "https://x", Provider: testProviderID},
			"deepseek": {OpenAIBaseURL: "https://y", Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"glm": {
				{Provider: "zhipu", Model: "glm", Priority: 1},
				{Provider: "deepseek", Model: "glm", Priority: 2},
			},
		},
	}
	p := newTestProxy(t, cfg)
	setPinForTest(p, "glm", "deepseek", time.Hour)
	var st appapi.StatusSchedule
	if err := json.Unmarshal(p.scheduleStatus(), &st); err != nil {
		t.Fatal(err)
	}
	ri, ok := st.Models["glm"]
	if !ok {
		t.Fatal("glm not in schedule status")
	}
	if ri.Pin != "deepseek" {
		t.Errorf("pin=%q want deepseek", ri.Pin)
	}
	if ri.PinExpires == "" {
		t.Error("pin_expires empty; want an 'expires in' label")
	}
	// Pin overlays the DEFAULT chain: ordered keeps both providers in their
	// unpinned order (zhipu p1 first), while first reports the effective
	// pin-applied choice (deepseek).
	if len(ri.Ordered) != 2 || ri.Ordered[0].Provider != "zhipu" || ri.Ordered[1].Provider != "deepseek" {
		t.Errorf("ordered=%+v want [zhipu deepseek] (default chain preserved under pin)", ri.Ordered)
	}
	if ri.First != "deepseek" {
		t.Errorf("first=%q want deepseek (effective pin-applied choice)", ri.First)
	}
}

// TestHandlePinAPI: POST /api/pin installs, GET /api/pin lists, DELETE removes;
// POST to a bad provider is 400 with a clear message.
func TestHandlePinAPI(t *testing.T) {
	w := NewWebServer(newTestProxy(t, &configdomain.Config{
		Providers: map[string]configdomain.Provider{"zhipu": {OpenAIBaseURL: "https://x", Provider: testProviderID}},
		Routes:    map[string][]configdomain.RouteTarget{"glm": {{Provider: "zhipu", Model: "glm"}}},
	}), "test-config.yaml")
	mux := http.NewServeMux()
	w.Register(mux)

	// Bad provider -> 400.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/pin", strings.NewReader(`{"route":"glm","provider":"nope"}`)))
	if rec.Code != 400 {
		t.Errorf("bad-provider pin status=%d want 400", rec.Code)
	}

	// Valid pin -> 200.
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, httptest.NewRequest("POST", "/api/pin", strings.NewReader(`{"route":"glm","provider":"zhipu","ttl_seconds":3600}`)))
	if rec2.Code != 200 {
		t.Fatalf("pin status=%d want 200: %s", rec2.Code, rec2.Body.String())
	}
	var set struct {
		Provider string `json:"provider"`
		Expires  string `json:"expires_at"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &set); err != nil {
		t.Fatalf("parse pin response: %v: %s", err, rec2.Body.String())
	}
	if set.Provider != "zhipu" || set.Expires == "" {
		t.Errorf("pin response=%+v want provider zhipu + an expiry", set)
	}

	// GET list contains exactly the pinned route→provider pair (parsed, not
	// substring — "glm" alone would also match "glm-4.6").
	rec3 := httptest.NewRecorder()
	mux.ServeHTTP(rec3, httptest.NewRequest("GET", "/api/pin", nil))
	var listResp struct {
		Pins []struct {
			Route    string `json:"route"`
			Provider string `json:"provider"`
		} `json:"pins"`
	}
	if err := json.Unmarshal(rec3.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("parse pin list: %v: %s", err, rec3.Body.String())
	}
	if pins := listResp.Pins; len(pins) != 1 || pins[0].Route != "glm" || pins[0].Provider != "zhipu" {
		t.Errorf("pin list = %+v, want exactly [glm→zhipu]", pins)
	}

	// DELETE removes it.
	rec4 := httptest.NewRecorder()
	mux.ServeHTTP(rec4, httptest.NewRequest("DELETE", "/api/pin?route=glm", nil))
	if rec4.Code != 200 {
		t.Fatalf("unpin status=%d want 200", rec4.Code)
	}
	rec5 := httptest.NewRecorder()
	mux.ServeHTTP(rec5, httptest.NewRequest("GET", "/api/pin", nil))
	if body := rec5.Body.String(); strings.Contains(body, `"glm"`) {
		t.Errorf("pin still present after delete: %s", body)
	}
}

// TestPin_BypassesCache is the pin×cache regression (sibling of the replay×cache
// fix): after a request is cached from provider A, pinning B and re-sending the
// SAME request must hit B, not return A's stale cached answer. Before the fix the
// cache lookup ran before the pin's `force` was computed, so the pin was defeated.
func TestPin_BypassesCache(t *testing.T) {
	var aHits, bHits int
	aUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aHits++
		w.Write([]byte(`{"from":"a"}`))
	}))
	defer aUp.Close()
	bUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bHits++
		w.Write([]byte(`{"from":"b"}`))
	}))
	defer bUp.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"a": {OpenAIBaseURL: aUp.URL, Provider: testProviderID},
			"b": {OpenAIBaseURL: bUp.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{"glm": {
			{Provider: "a", Model: "glm", Priority: 1},
			{Provider: "b", Model: "glm", Priority: 2},
		}},
		Cache: configdomain.CacheConfig{Enabled: true, TTL: "1h"},
	}
	p := newTestProxy(t, cfg)
	p.providers["a"] = &testProv{key: "a"}
	p.providers["b"] = &testProv{key: "b"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	body := `{"model":"glm","input":[]}`
	// Prime: a (priority 1) serves + caches.
	resp, err := http.Post(px.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	prime, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(prime), `"from":"a"`) {
		t.Fatalf("prime: expected a, got %s", string(prime))
	}
	// Pin b.
	if !setPinForTest(p, "glm", "b", 0) {
		t.Fatal("setPin b failed")
	}
	// Same request, now pinned to b → must bypass cache and hit b.
	resp2, err := http.Post(px.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	pinned, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if !strings.Contains(string(pinned), `"from":"b"`) {
		t.Errorf("pinned request returned %s, expected b (pin must bypass the a-cached answer)", string(pinned))
	}
	if bHits != 1 {
		t.Errorf("bHits=%d want 1 (pin must reach b, not cache)", bHits)
	}
}

// TestPin_ForcesThroughCircuit is the F4 regression: a pin is EXCLUSIVE — when
// the pinned provider's circuit is open, the request is still forced to it (the
// user pinned it to debug/compare), NOT silently failed over to another target.
// Before the fix, avail() excluded the circuit-open pin → the pin was a no-op →
// the request failed over to the priority-1 provider.
func TestPin_ForcesThroughCircuit(t *testing.T) {
	var aHit, bHit bool
	aUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aHit = true
		w.Write([]byte(`{}`))
	}))
	defer aUp.Close()
	bUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bHit = true
		w.Write([]byte(`{}`))
	}))
	defer bUp.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"a": {OpenAIBaseURL: aUp.URL, Provider: testProviderID},
			"b": {OpenAIBaseURL: bUp.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"glm": {
				{Provider: "a", Model: "glm", Priority: 1},
				{Provider: "b", Model: "glm", Priority: 2},
			},
		},
	}
	p := newTestProxy(t, cfg)
	p.providers["a"] = &testProv{key: "a"}
	p.providers["b"] = &testProv{key: "b"}
	if !setPinForTest(p, "glm", "b", 0) {
		t.Fatal("setPin b failed")
	}
	// Open b's circuit (3 consecutive failures → circuit_threshold default 3).
	sched := configdomain.Scheduling{} // threshold()=3, cooldown()=10m
	for i := 0; i < 3; i++ {
		p.recordFailure("b", sched)
	}
	hb, ok := p.runtimeState.Dashboard(time.Now()).Providers["b"]
	open := ok && hb.CircuitOpenUntil.After(time.Now())
	if !open {
		t.Fatal("precondition: b should be circuit-open")
	}

	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()
	postOK(t, px.URL+"/v1/responses", `{"model":"glm","input":[]}`)

	if !bHit {
		t.Error("pinned + circuit-open provider b was NOT hit — pin must force through the circuit")
	}
	if aHit {
		t.Error("provider a was hit — pin must NOT fail over when the pinned provider is circuit-open")
	}
}

// TestPin_TTLExpiryRestoresScheduling (P0): the pin state machine's fourth
// terminal. While pinned, the route is a hard choice (no failover, forces
// through the circuit); after the TTL lapses, the pin disappears and normal
// scheduling — including circuit failover — resumes.
func TestPin_TTLExpiryRestoresScheduling(t *testing.T) {
	var aHits atomic.Int64
	aUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aHits.Add(1)
		w.Write([]byte(`{"from":"a"}`))
	}))
	defer aUp.Close()
	bUp, bHits := newHitServer(func(int) (int, string, http.Header, time.Duration) {
		return 500, `{"e":"b broken"}`, nil, 0
	})
	defer bUp.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"a": {OpenAIBaseURL: aUp.URL, Provider: testProviderID},
			"b": {OpenAIBaseURL: bUp.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"glm": {
				{Provider: "a", Model: "glm", Priority: 1},
				{Provider: "b", Model: "glm", Priority: 2},
			},
		},
		Scheduling: configdomain.Scheduling{CircuitThreshold: 3, RetryWait: "0"},
	}
	p := newProxyWithStatic(t, cfg, map[string]string{"a": "ka", "b": "kb"})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	// Pin glm → b with a short TTL. While pinned, b is a hard choice: its 500s
	// do NOT fail over to a; the single pinned target failing hard answers
	// with the proxy's all-targets-failed 502 (4xx commits verbatim, 5xx does
	// not), and the failures still trip b's circuit.
	if !setPinForTest(p, "glm", "b", 400*time.Millisecond) {
		t.Fatal("setPin with TTL failed")
	}
	for i := 0; i < 3; i++ {
		if st := postStatus(t, px.URL+"/v1/chat/completions", `{"model":"glm","messages":[]}`); st != 502 {
			t.Fatalf("pinned request %d: status = %d, want 502 (pinned 5xx, no failover)", i+1, st)
		}
	}
	if got := bHits.Load(); got != 3 {
		t.Fatalf("pinned b hits = %d, want 3", got)
	}
	if got := aHits.Load(); got != 0 {
		t.Fatalf("a hit %d time(s) while pinned — pin must not fail over", got)
	}
	if h, ok := p.runtimeState.Dashboard(time.Now()).Providers["b"]; !ok || h.Available {
		t.Fatalf("b circuit should be open after 3 failures while pinned: %+v", h)
	}

	// A fourth request while STILL pinned forces through b's open circuit.
	if st := postStatus(t, px.URL+"/v1/chat/completions", `{"model":"glm","messages":[]}`); st != 502 {
		t.Fatalf("pinned-through-circuit request: status = %d, want 502", st)
	}

	// TTL lapse is observable in the runtime snapshot — poll for it, no sleep.
	waitUntil(t, "pin TTL expiry", func() bool {
		_, active := p.runtimeState.Dashboard(time.Now()).Pins["glm"]
		return !active
	})

	// Pin gone: b's open circuit now routes the request to a (failover restored).
	if st, body := post(t, px.URL+"/v1/chat/completions", `{"model":"glm","messages":[]}`); st != 200 || body != `{"from":"a"}` {
		t.Fatalf("post-expiry request: status=%d body=%s, want 200 from a", st, body)
	}
	if got := aHits.Load(); got != 1 {
		t.Errorf("a hits after expiry = %d, want 1 (scheduling restored)", got)
	}
	if got := bHits.Load(); got != 4 {
		t.Errorf("b hits after expiry = %d, want 4 (unpinned b skipped via circuit)", got)
	}
}
