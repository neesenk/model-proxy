package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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

	cfg := &Config{
		Providers: map[string]Provider{
			"zhipu":    {OpenAIBaseURL: zhipuUp.URL, Provider: "static"},
			"deepseek": {OpenAIBaseURL: deepUp.URL, Provider: "static"},
		},
		Routes: map[string][]RouteTarget{
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
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	post(t, px.URL+"/v1/responses", `{"model":"glm","input":[]}`)
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

	cfg := &Config{
		Providers: map[string]Provider{
			"zhipu":    {OpenAIBaseURL: zhipuUp.URL, Provider: "static"},
			"deepseek": {OpenAIBaseURL: deepUp.URL, Provider: "static"},
		},
		Routes: map[string][]RouteTarget{
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
	px := httptest.NewServer(http.HandlerFunc(p.handler))
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
	cfg := &Config{
		Providers: map[string]Provider{"zhipu": {OpenAIBaseURL: "https://x", Provider: "static"}},
		Routes:    map[string][]RouteTarget{"glm": {{Provider: "zhipu", Model: "glm"}}},
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

// TestPin_TTLExpiry: an expired pin is ignored (decideOrder falls back to the
// normal order), while an active pin holds.
func TestPin_TTLExpiry(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{
			"zhipu":    {OpenAIBaseURL: "https://x", Provider: "static"},
			"deepseek": {OpenAIBaseURL: "https://y", Provider: "static"},
		},
		Routes: map[string][]RouteTarget{
			"glm": {
				{Provider: "zhipu", Model: "glm", Priority: 1},
				{Provider: "deepseek", Model: "glm", Priority: 2},
			},
		},
	}
	p := newTestProxy(t, cfg)
	routeKeys := map[string]bool{"glm": true}
	// Active pin → only deepseek.
	setPinForTest(p, "glm", "deepseek", time.Hour)
	ordered, _ := p.decideOrder(cfg, p.parentOf, "glm", "", cfg.Routes["glm"], time.Now(), false, routeKeys)
	if len(ordered) != 1 || ordered[0].Provider != "deepseek" {
		t.Errorf("active pin order=%+v want [deepseek] only", ordered)
	}
	// Expire the pin → back to the full ranking (zhipu first by priority).
	p.healthMu.Lock()
	pe := p.pins["glm"]
	pe.expiresAt = time.Now().Add(-time.Minute)
	p.pins["glm"] = pe
	p.healthMu.Unlock()
	ordered2, _ := p.decideOrder(cfg, p.parentOf, "glm", "", cfg.Routes["glm"], time.Now(), false, routeKeys)
	if len(ordered2) != 2 || ordered2[0].Provider != "zhipu" {
		t.Errorf("expired pin order=%+v want full ranking [zhipu, deepseek]", ordered2)
	}
}

// TestScheduleStatus_ShowsPin: /debug/schedule labels a pinned route with its
// pin provider + expiry, and the pin's narrowing of `ordered` is visible.
func TestScheduleStatus_ShowsPin(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{
			"zhipu":    {OpenAIBaseURL: "https://x", Provider: "static"},
			"deepseek": {OpenAIBaseURL: "https://y", Provider: "static"},
		},
		Routes: map[string][]RouteTarget{
			"glm": {
				{Provider: "zhipu", Model: "glm", Priority: 1},
				{Provider: "deepseek", Model: "glm", Priority: 2},
			},
		},
	}
	p := newTestProxy(t, cfg)
	setPinForTest(p, "glm", "deepseek", time.Hour)
	var st statusSchedule
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
	// Pin narrowed ordered to deepseek only.
	if len(ri.Ordered) != 1 || ri.Ordered[0].Provider != "deepseek" {
		t.Errorf("ordered=%+v want [deepseek] (pin narrows it)", ri.Ordered)
	}
}

// TestHandlePinAPI: POST /api/pin installs, GET /api/pin lists, DELETE removes;
// POST to a bad provider is 400 with a clear message.
func TestHandlePinAPI(t *testing.T) {
	w := newWebServer(newTestProxy(t, &Config{
		Providers: map[string]Provider{"zhipu": {OpenAIBaseURL: "https://x", Provider: "static"}},
		Routes:    map[string][]RouteTarget{"glm": {{Provider: "zhipu", Model: "glm"}}},
	}), "test-config.yaml")
	mux := http.NewServeMux()
	w.register(mux)

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
	json.Unmarshal(rec2.Body.Bytes(), &set)
	if set.Provider != "zhipu" || set.Expires == "" {
		t.Errorf("pin response=%+v want provider zhipu + an expiry", set)
	}

	// GET list contains the pin.
	rec3 := httptest.NewRecorder()
	mux.ServeHTTP(rec3, httptest.NewRequest("GET", "/api/pin", nil))
	if !strings.Contains(rec3.Body.String(), `"glm"`) || !strings.Contains(rec3.Body.String(), `"zhipu"`) {
		t.Errorf("pin list missing the pin: %s", rec3.Body.String())
	}

	// DELETE removes it.
	rec4 := httptest.NewRecorder()
	mux.ServeHTTP(rec4, httptest.NewRequest("DELETE", "/api/pin?route=glm", nil))
	if rec4.Code != 200 {
		t.Fatalf("unpin status=%d want 200", rec4.Code)
	}
	rec5 := httptest.NewRecorder()
	mux.ServeHTTP(rec5, httptest.NewRequest("GET", "/api/pin", nil))
	if strings.Contains(rec5.Body.String(), `"glm"`) {
		t.Errorf("pin still present after delete: %s", rec5.Body.String())
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

	cfg := &Config{
		Providers: map[string]Provider{
			"a": {OpenAIBaseURL: aUp.URL, Provider: "static"},
			"b": {OpenAIBaseURL: bUp.URL, Provider: "static"},
		},
		Routes: map[string][]RouteTarget{"glm": {
			{Provider: "a", Model: "glm", Priority: 1},
			{Provider: "b", Model: "glm", Priority: 2},
		}},
		Cache: CacheConfig{Enabled: true, TTL: "1h"},
	}
	p := newTestProxy(t, cfg)
	p.providers["a"] = &testProv{key: "a"}
	p.providers["b"] = &testProv{key: "b"}
	px := httptest.NewServer(http.HandlerFunc(p.handler))
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

	cfg := &Config{
		Providers: map[string]Provider{
			"a": {OpenAIBaseURL: aUp.URL, Provider: "static"},
			"b": {OpenAIBaseURL: bUp.URL, Provider: "static"},
		},
		Routes: map[string][]RouteTarget{
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
	sched := Scheduling{} // threshold()=3, cooldown()=10m
	for i := 0; i < 3; i++ {
		p.recordFailure("b", sched)
	}
	p.healthMu.Lock()
	hb := p.health["b"]
	open := hb != nil && hb.circuitOpenUntil.After(time.Now())
	p.healthMu.Unlock()
	if !open {
		t.Fatal("precondition: b should be circuit-open")
	}

	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()
	post(t, px.URL+"/v1/responses", `{"model":"glm","input":[]}`)

	if !bHit {
		t.Error("pinned + circuit-open provider b was NOT hit — pin must force through the circuit")
	}
	if aHit {
		t.Error("provider a was hit — pin must NOT fail over when the pinned provider is circuit-open")
	}
}
