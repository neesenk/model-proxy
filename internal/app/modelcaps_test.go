package app

// modelcaps_test.go — application integration for MODEL-level protocol
// capability probing: startup probe pass, fingerprint-gated cache reuse,
// ProtocolHint synthesis, model-driven forward protocol selection, model-level
// 404 correction, and model_caps.json persistence round-trips.
// Pure store/policy/file behavior belongs to internal/runtime/wirecap.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	runtimewire "model-proxy/internal/runtime/wirecap"
)

// matrixUpstream answers per protocol path and records hits (leg probes are
// concurrent, so the records are mutex-guarded).
type matrixUpstream struct {
	srv   *httptest.Server
	mu    sync.Mutex
	hits  map[string]int
	paths []string
}

func (u *matrixUpstream) record(path string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.hits[path]++
	u.paths = append(u.paths, path)
}

func (u *matrixUpstream) hitCount(path string) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.hits[path]
}

func (u *matrixUpstream) resetHits() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.hits = map[string]int{}
}

func (u *matrixUpstream) totalHits() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	total := 0
	for _, n := range u.hits {
		total += n
	}
	return total
}

func newMatrixUpstream(t *testing.T, statusBy map[string]int) *matrixUpstream {
	t.Helper()
	u := &matrixUpstream{hits: map[string]int{}}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		u.record(r.URL.Path)
		status := statusBy[r.URL.Path]
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		w.Write([]byte(`{}`))
	}))
	t.Cleanup(u.srv.Close)
	return u
}

// TestModelCaps_ProbePass: every configured model is probed on the openai
// base's chat/responses legs; the anthropic leg is probed on the ANTHROPIC
// base when configured and concluded No (unprobed) when not.
func TestModelCaps_ProbePass(t *testing.T) {
	openai := newMatrixUpstream(t, map[string]int{"/responses": http.StatusNotFound})
	anthropic := newMatrixUpstream(t, nil)
	cfg := &Config{
		Providers: map[string]Provider{
			"p": {
				OpenAIBaseURL:    openai.srv.URL,
				AnthropicBaseURL: anthropic.srv.URL,
				Provider:         testProviderID,
				Models:           []string{"m1", "m2"},
			},
		},
		Routes: map[string][]RouteTarget{},
	}
	p := newTestProxy(t, cfg)
	p.providers["p"] = &testProv{key: "k"}

	p.probeAllModelCaps()

	for _, m := range []string{"m1", "m2"} {
		mp, ok := p.modelCaps.Get("p", m)
		if !ok {
			t.Fatalf("model %s not probed", m)
		}
		if mp.Chat != triYes || mp.Responses != triNo || mp.Anthropic != triYes {
			t.Errorf("%s matrix = chat:%s anthropic:%s responses:%s, want yes/yes/no",
				m, mp.Chat, mp.Anthropic, mp.Responses)
		}
	}
	// Anthropic probed ONLY on the anthropic base, never fabricated on openai.
	if openai.hitCount("/v1/messages") != 0 {
		t.Errorf("anthropic fabricated on openai base (hits: %d)", openai.hitCount("/v1/messages"))
	}
	if anthropic.hitCount("/v1/messages") != 2 {
		t.Errorf("anthropic base hits = %d, want 2 (m1+m2)", anthropic.hitCount("/v1/messages"))
	}

	// Fingerprint recorded; a second pass with unchanged config is a no-op.
	fp, ok := p.modelCaps.ProviderFingerprint("p")
	if !ok || fp == "" {
		t.Fatal("provider fingerprint not recorded")
	}
	openai.resetHits()
	p.probeAllModelCaps()
	if total := openai.totalHits(); total != 0 {
		t.Errorf("re-probe hit upstream %d times, want 0 (fingerprint unchanged)", total)
	}
}

// TestModelCaps_FingerprintChangeReprobes: a protocol-relevant config change
// (different base URL) invalidates the cached matrix and re-probes.
func TestModelCaps_FingerprintChangeReprobes(t *testing.T) {
	openai := newMatrixUpstream(t, nil)
	cfg := &Config{
		Providers: map[string]Provider{
			"p": {OpenAIBaseURL: openai.srv.URL, Provider: testProviderID, Models: []string{"m1"}},
		},
		Routes: map[string][]RouteTarget{},
	}
	p := newTestProxy(t, cfg)
	p.providers["p"] = &testProv{key: "k"}

	// Pre-seed a verdict under a DIFFERENT fingerprint (stale config).
	p.modelCaps.Put("p", "stale-fp", "m1",
		runtimewire.ModelProtocols{Chat: triNo, Anthropic: triNo, Responses: triNo}, time.Now())
	p.probeAllModelCaps()
	mp, ok := p.modelCaps.Get("p", "m1")
	if !ok || mp.Chat != triYes {
		t.Errorf("after re-probe = %+v (ok=%v), want chat:yes from the new endpoint", mp, ok)
	}
}

// TestModelCaps_UnknownLegsReprobed: an unconcluded leg (5xx → unknown) is
// re-probed on the next pass even with a matching fingerprint.
func TestModelCaps_UnknownLegsReprobed(t *testing.T) {
	var flaky500 = true
	var chatHits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/chat/completions" {
			chatHits++
			if flaky500 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
		w.Write([]byte(`{}`))
	}))
	defer up.Close()
	cfg := &Config{
		Providers: map[string]Provider{
			"p": {OpenAIBaseURL: up.URL, Provider: testProviderID, Models: []string{"m1"}},
		},
		Routes: map[string][]RouteTarget{},
	}
	p := newTestProxy(t, cfg)
	p.providers["p"] = &testProv{key: "k"}

	p.probeAllModelCaps()
	if mp, _ := p.modelCaps.Get("p", "m1"); mp.Chat != triUnknown {
		t.Fatalf("chat leg = %s, want unknown after 5xx", mp.Chat)
	}
	flaky500 = false
	p.probeAllModelCaps()
	if chatHits != 2 {
		t.Errorf("chat leg hits = %d, want 2 (unknown leg re-probed)", chatHits)
	}
	if mp, _ := p.modelCaps.Get("p", "m1"); mp.Chat != triYes {
		t.Errorf("chat leg after re-probe = %s, want yes", mp.Chat)
	}
}

// TestModelCaps_ProtocolHintSynthesized: a hint-covered provider (codex) is
// never probed — its matrix is synthesized from the hint (responses only).
func TestModelCaps_ProtocolHintSynthesized(t *testing.T) {
	var hits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Write([]byte(`{}`))
	}))
	defer up.Close()
	cfg := &Config{
		Providers: map[string]Provider{
			"cdx": {OpenAIBaseURL: up.URL, Provider: "codex", Models: []string{"gpt-x"}},
		},
		Routes: map[string][]RouteTarget{},
	}
	p := newTestProxy(t, cfg)
	p.providers["cdx"] = &testProv{key: "k"}

	p.probeAllModelCaps()

	if hits != 0 {
		t.Errorf("hint-covered provider probed (%d hits), want 0", hits)
	}
	mp, ok := p.modelCaps.Get("cdx", "gpt-x")
	if !ok || mp.Responses != triYes || mp.Chat != triNo || mp.Anthropic != triNo {
		t.Errorf("synthesized matrix = %+v (ok=%v), want responses-only", mp, ok)
	}
}

// TestModelCaps_Forward_ModelLevelResponsesVerdict: the model-level matrix
// drives protocol selection when the provider-level verdict is absent —
// anthropic client → responses conversion for a responses-capable model.
func TestModelCaps_Forward_ModelLevelResponsesVerdict(t *testing.T) {
	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
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
	p.modelCaps.Put("oai", "fp", "gpt-x",
		runtimewire.ModelProtocols{Chat: triYes, Anthropic: triNo, Responses: triYes}, time.Now())
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	resp, err := http.Post(px.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"claude-x","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("client status = %d: %s", resp.StatusCode, body)
	}
	if gotPath != "/responses" {
		t.Errorf("upstream path = %q, want /responses (model-level verdict)", gotPath)
	}
}

// TestModelCaps_Forward_ModelLevelChatOnly: a chat-only model behind an
// anthropic client is converted to chat even when the provider-level verdict
// says responses=yes (the model-level matrix wins).
func TestModelCaps_Forward_ModelLevelChatOnly(t *testing.T) {
	var gotPath string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"c1","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer up.Close()
	cfg := &Config{
		Providers: map[string]Provider{"oai": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"claude-x": {{Provider: "oai", Model: "old-m"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["oai"] = &testProv{key: "k"}
	p.setWireCaps("oai", wireCaps{BaseURL: up.URL, Responses: triYes, Chat: triYes, ProbedAt: time.Now()})
	p.modelCaps.Put("oai", "fp", "old-m",
		runtimewire.ModelProtocols{Chat: triYes, Anthropic: triNo, Responses: triNo}, time.Now())
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	resp, err := http.Post(px.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"claude-x","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("client status = %d: %s", resp.StatusCode, body)
	}
	if gotPath != "/chat/completions" {
		t.Errorf("upstream path = %q, want /chat/completions (model-level no beats provider-level yes)", gotPath)
	}
}

// TestModelCaps_Forward_404CorrectionModelLevel: a model-verdict-driven
// /responses 404 flips the MODEL-level verdict (provider-level untouched), no
// model lock, and the next request goes out as chat.
func TestModelCaps_Forward_404CorrectionModelLevel(t *testing.T) {
	var paths []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
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
	p.modelCaps.Put("oai", "fp", "gpt-x",
		runtimewire.ModelProtocols{Chat: triYes, Anthropic: triNo, Responses: triYes}, time.Now())
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	resp, err := http.Post(px.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"claude-x","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("request 1 status = %d, want 404 (committed verdict miss)", resp.StatusCode)
	}

	mp, _ := p.modelCaps.Get("oai", "gpt-x")
	if mp.Responses != triNo {
		t.Errorf("model verdict responses = %s, want no after 404 correction", mp.Responses)
	}
	if _, ok := p.wireVerdict("oai"); ok {
		t.Error("provider-level verdict must stay untouched by a model-level correction")
	}
	locks := 0
	for _, entries := range p.runtimeState.Dashboard(time.Now()).ModelLocks {
		locks += len(entries)
	}
	if locks != 0 {
		t.Errorf("model locked %d time(s), want 0 (verdict miss is not a model failure)", locks)
	}

	resp2, err := http.Post(px.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"claude-x","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("request 2 status = %d, want 200 (chat fallback)", resp2.StatusCode)
	}
	if len(paths) != 2 || paths[0] != "/responses" || paths[1] != "/chat/completions" {
		t.Errorf("upstream paths = %v, want [/responses /chat/completions]", paths)
	}
}

// TestModelCaps_PersistRoundTrip: the matrix lands in model_caps.json,
// restores on boot with a matching config fingerprint, and is invalidated by
// a protocol-relevant config change.
func TestModelCaps_PersistRoundTrip(t *testing.T) {
	useStaticProviderPools(t, "p")
	dir := t.TempDir()
	statePath := filepath.Join(dir, "quota_state.json")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{}`)) }))
	defer up.Close()
	mkCfg := func(baseURL string) *Config {
		return &Config{
			Providers: map[string]Provider{"p": {OpenAIBaseURL: baseURL, Provider: testProviderID, Models: []string{"m1"}}},
			Routes:    map[string][]RouteTarget{},
		}
	}

	p1 := newTestProxyAt(t, mkCfg(up.URL), statePath)
	p1.probeAllModelCaps()
	// probeAllModelCaps persists ASYNC; do a synchronous save for the assertion.
	if err := runtimewire.SaveModelCapsFile(p1.modelCapsPath, p1.modelCaps.Snapshot()); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p1.modelCapsPath)
	if err != nil {
		t.Fatal(err)
	}
	if content := string(raw); !strings.Contains(content, `"fingerprint"`) || !strings.Contains(content, `"chat":"yes"`) {
		t.Errorf("model_caps.json missing matrix:\n%s", content)
	}

	// Boot a second proxy on the same file + config: restored without probing.
	p2 := newTestProxyAt(t, mkCfg(up.URL), statePath)
	mp, ok := p2.modelCaps.Get("p", "m1")
	if !ok || mp.Chat != triYes {
		t.Errorf("restored matrix = %+v (ok=%v), want chat:yes", mp, ok)
	}

	// Base URL change → fingerprint mismatch → not restored.
	p3 := newTestProxyAt(t, mkCfg("http://other-endpoint.invalid"), statePath)
	if _, ok := p3.modelCaps.Get("p", "m1"); ok {
		t.Error("matrix restored despite fingerprint mismatch — must be invalidated")
	}
}

// TestModelCaps_Forward_UnknownLegBeatsDeadLeg pins intentional behavior #31
// end to end at the wire level: a chat client whose model-level chat leg is
// probed no, with the anthropic leg probed yes, is CONVERTED to the anthropic
// leg (request hits /v1_messages, not the dead /chat/completions); with an
// UNCONCLUDED anthropic leg the request still tries it (unknown ≠ dead); with
// every leg concluded no the request rides chat so the upstream error surfaces.
func TestModelCaps_Forward_UnknownLegBeatsDeadLegE2E(t *testing.T) {
	makeProxy := func(t *testing.T, up *httptest.Server, mp runtimewire.ModelProtocols) (*Proxy, *httptest.Server) {
		cfg := &Config{
			Providers: map[string]Provider{"p": {OpenAIBaseURL: up.URL, AnthropicBaseURL: up.URL, Provider: testProviderID}},
			Routes:    map[string][]RouteTarget{"m": {{Provider: "p", Model: "m"}}},
		}
		p := newTestProxy(t, cfg)
		p.providers["p"] = &testProv{key: "k"}
		p.modelCaps.Put("p", "fp", "m", mp, time.Now())
		px := httptest.NewServer(http.HandlerFunc(p.Handler))
		t.Cleanup(px.Close)
		return p, px
	}
	post := func(t *testing.T, px *httptest.Server) {
		resp, err := http.Post(px.URL+"/v1/chat/completions", "application/json",
			strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"ping"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	t.Run("anthropic yes beats dead chat leg", func(t *testing.T) {
		var paths []string
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			w.Header().Set("content-type", "application/json")
			w.Write([]byte(`{"id":"m1","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
		}))
		defer up.Close()
		_, px := makeProxy(t, up, runtimewire.ModelProtocols{Chat: triNo, Anthropic: triYes, Responses: triNo})
		post(t, px)
		if len(paths) != 1 || paths[0] != "/v1/messages" {
			t.Errorf("upstream paths = %v, want one /v1/messages (converted off the dead chat leg)", paths)
		}
	})

	t.Run("anthropic unknown beats dead chat leg", func(t *testing.T) {
		var paths []string
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			w.Header().Set("content-type", "application/json")
			w.Write([]byte(`{"id":"m1","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
		}))
		defer up.Close()
		_, px := makeProxy(t, up, runtimewire.ModelProtocols{Chat: triNo, Anthropic: triUnknown, Responses: triNo})
		post(t, px)
		if len(paths) != 1 || paths[0] != "/v1/messages" {
			t.Errorf("upstream paths = %v, want one /v1/messages (unknown leg tried before the dead one)", paths)
		}
	})

	t.Run("all no rides chat so the error surfaces", func(t *testing.T) {
		var paths []string
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			paths = append(paths, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"retcode":40403,"message":"Model not supported by this endpoint"}`))
		}))
		defer up.Close()
		_, px := makeProxy(t, up, runtimewire.ModelProtocols{Chat: triNo, Anthropic: triNo, Responses: triNo})
		post(t, px)
		if len(paths) != 1 || paths[0] != "/chat/completions" {
			t.Errorf("upstream paths = %v, want one /chat/completions (error surfaced, no leg-hopping)", paths)
		}
	})
}
