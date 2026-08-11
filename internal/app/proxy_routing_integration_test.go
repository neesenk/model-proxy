package app

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"model-proxy/internal/provider"
)

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
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()

	got := map[string]bool{}
	for _, m := range out.Data {
		got[m.ID] = true
	}
	for _, want := range []string{"glm-5.2", "deepseek-v4-pro", "claude-opus-4-8", "claude-sonnet-4-6"} {
		if !got[want] {
			t.Errorf("GET /v1/models missing %q (routes ∪ claude_mapping); got %v", want, got)
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
	post(t, px.URL+"/v1/responses", `{"model":"m1","input":[]}`)

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

	post(t, px.URL+"/v1/messages", `{"model":"glm-5.2","messages":[]}`)

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

	post(t, px.URL+"/v1/responses", `{"model":"gpt-5.5","input":[]}`)

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

	post(t, px.URL+"/v1/messages", `{"model":"deepseek-v4-pro","messages":[]}`)
	post(t, px.URL+"/v1/chat/completions", `{"model":"deepseek-v4-pro","messages":[]}`)

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
	var gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
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
}

// ensure bytes import is used (some build configs need it)
var _ = bytes.MinRead
