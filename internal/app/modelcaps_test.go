package app

// modelcaps_test.go — application integration for MODEL-level protocol
// capability probing: startup probe pass, fingerprint-gated cache reuse,
// ProtocolHint synthesis, model-driven forward protocol selection, model-level
// 404 correction, and model_caps.json persistence round-trips.
// Pure store/policy/file behavior belongs to internal/runtime/wirecap.

import (
	"context"
	"io"
	configdomain "model-proxy/internal/config"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"model-proxy/internal/providerbuild"
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
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"p": {
				OpenAIBaseURL:    openai.srv.URL,
				AnthropicBaseURL: anthropic.srv.URL,
				Provider:         testProviderID,
				Models:           []string{"m1", "m2"},
			},
		},
		Routes: map[string][]configdomain.RouteTarget{},
	}
	p := newTestProxy(t, cfg)
	p.providers["p"] = &testProv{key: "k"}

	p.probeAllModelCaps(context.Background())

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
	p.probeAllModelCaps(context.Background())
	if total := openai.totalHits(); total != 0 {
		t.Errorf("re-probe hit upstream %d times, want 0 (fingerprint unchanged)", total)
	}
}

// TestModelCaps_FingerprintChangeReprobes: a protocol-relevant config change
// (different base URL) invalidates the cached matrix and re-probes.
func TestModelCaps_FingerprintChangeReprobes(t *testing.T) {
	openai := newMatrixUpstream(t, nil)
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"p": {OpenAIBaseURL: openai.srv.URL, Provider: testProviderID, Models: []string{"m1"}},
		},
		Routes: map[string][]configdomain.RouteTarget{},
	}
	p := newTestProxy(t, cfg)
	p.providers["p"] = &testProv{key: "k"}

	// Pre-seed a verdict under a DIFFERENT fingerprint (stale config).
	p.modelCaps.Put("p", "stale-fp", "m1",
		runtimewire.ModelProtocols{Chat: triNo, Anthropic: triNo, Responses: triNo}, time.Now())
	p.probeAllModelCaps(context.Background())
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
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"p": {OpenAIBaseURL: up.URL, Provider: testProviderID, Models: []string{"m1"}},
		},
		Routes: map[string][]configdomain.RouteTarget{},
	}
	p := newTestProxy(t, cfg)
	p.providers["p"] = &testProv{key: "k"}

	p.probeAllModelCaps(context.Background())
	if mp, _ := p.modelCaps.Get("p", "m1"); mp.Chat != triUnknown {
		t.Fatalf("chat leg = %s, want unknown after 5xx", mp.Chat)
	}
	flaky500 = false
	p.probeAllModelCaps(context.Background())
	if chatHits != 2 {
		t.Errorf("chat leg hits = %d, want 2 (unknown leg re-probed)", chatHits)
	}
	if mp, _ := p.modelCaps.Get("p", "m1"); mp.Chat != triYes {
		t.Errorf("chat leg after re-probe = %s, want yes", mp.Chat)
	}
}

// TestModelCaps_StaleModelsPruned: models dropped from config since the last
// pass (fingerprint unchanged — it does not cover the model list) are pruned
// from the store on the next pass, so /api/models stops serving them; the
// still-configured concluded models are NOT re-probed.
func TestModelCaps_StaleModelsPruned(t *testing.T) {
	openai := newMatrixUpstream(t, nil)
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"p": {OpenAIBaseURL: openai.srv.URL, Provider: testProviderID, Models: []string{"m1"}},
		},
		Routes: map[string][]configdomain.RouteTarget{},
	}
	p := newTestProxy(t, cfg)
	p.providers["p"] = &testProv{key: "k"}

	// Pre-seed concluded verdicts under the CURRENT fingerprint: m1 (still
	// configured) + stale (removed from models: since the last pass).
	fp := providerbuild.ProtocolConfigFingerprint(cfg.Providers["p"])
	p.modelCaps.Put("p", fp, "m1",
		runtimewire.ModelProtocols{Chat: triYes, Anthropic: triNo, Responses: triNo}, time.Now())
	p.modelCaps.Put("p", fp, "stale",
		runtimewire.ModelProtocols{Chat: triYes, Anthropic: triNo, Responses: triNo}, time.Now())

	p.probeAllModelCaps(context.Background())

	if _, ok := p.modelCaps.Get("p", "stale"); ok {
		t.Error("model dropped from config survived the probe pass — must be pruned")
	}
	if _, ok := p.modelCaps.Get("p", "m1"); !ok {
		t.Error("configured model lost in pruning")
	}
	if total := openai.totalHits(); total != 0 {
		t.Errorf("concluded models re-probed (%d hits), want 0 (fingerprint unchanged)", total)
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
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"cdx": {OpenAIBaseURL: up.URL, Provider: "codex", Models: []string{"gpt-x"}},
		},
		Routes: map[string][]configdomain.RouteTarget{},
	}
	p := newTestProxy(t, cfg)
	p.providers["cdx"] = &testProv{key: "k"}

	p.probeAllModelCaps(context.Background())

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
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{"oai": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]configdomain.RouteTarget{"claude-x": {{Provider: "oai", Model: "gpt-x"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["oai"] = &testProv{key: "k"}
	p.modelCaps.Put("oai", "fp", "gpt-x",
		runtimewire.ModelProtocols{Chat: triYes, Anthropic: triNo, Responses: triYes}, time.Now())
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	code, body := post(t, px.URL+"/v1/messages", `{"model":"claude-x","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	if code != http.StatusOK {
		t.Fatalf("client status = %d: %s", code, body)
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
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{"oai": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]configdomain.RouteTarget{"claude-x": {{Provider: "oai", Model: "old-m"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["oai"] = &testProv{key: "k"}
	p.setWireCaps("oai", wireCaps{BaseURL: up.URL, Responses: triYes, Chat: triYes, ProbedAt: time.Now()})
	p.modelCaps.Put("oai", "fp", "old-m",
		runtimewire.ModelProtocols{Chat: triYes, Anthropic: triNo, Responses: triNo}, time.Now())
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	code, body := post(t, px.URL+"/v1/messages", `{"model":"claude-x","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	if code != http.StatusOK {
		t.Fatalf("client status = %d: %s", code, body)
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
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{"oai": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]configdomain.RouteTarget{"claude-x": {{Provider: "oai", Model: "gpt-x"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["oai"] = &testProv{key: "k"}
	p.modelCaps.Put("oai", "fp", "gpt-x",
		runtimewire.ModelProtocols{Chat: triYes, Anthropic: triNo, Responses: triYes}, time.Now())
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	if code, _ := post(t, px.URL+"/v1/messages", `{"model":"claude-x","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`); code != http.StatusNotFound {
		t.Fatalf("request 1 status = %d, want 404 (committed verdict miss)", code)
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

	if code, _ := post(t, px.URL+"/v1/messages", `{"model":"claude-x","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`); code != http.StatusOK {
		t.Fatalf("request 2 status = %d, want 200 (chat fallback)", code)
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
	mkCfg := func(baseURL string) *configdomain.Config {
		return &configdomain.Config{
			Providers: map[string]configdomain.Provider{"p": {OpenAIBaseURL: baseURL, Provider: testProviderID, Models: []string{"m1"}}},
			Routes:    map[string][]configdomain.RouteTarget{},
		}
	}

	p1 := newTestProxyAt(t, mkCfg(up.URL), statePath)
	p1.probeAllModelCaps(context.Background())
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
		cfg := &configdomain.Config{
			Providers: map[string]configdomain.Provider{"p": {OpenAIBaseURL: up.URL, AnthropicBaseURL: up.URL, Provider: testProviderID}},
			Routes:    map[string][]configdomain.RouteTarget{"m": {{Provider: "p", Model: "m"}}},
		}
		p := newTestProxy(t, cfg)
		p.providers["p"] = &testProv{key: "k"}
		p.modelCaps.Put("p", "fp", "m", mp, time.Now())
		px := httptest.NewServer(http.HandlerFunc(p.Handler))
		t.Cleanup(px.Close)
		return p, px
	}
	// fire posts one client request and returns the client-visible terminal
	// state; named fire (not post) so it does not shadow the package-level
	// post() helper it delegates to. The E2E contract row is "让错误浮现"/leg
	// hit — both the wire path AND the client outcome must be asserted.
	fire := func(t *testing.T, px *httptest.Server) (int, string) {
		return post(t, px.URL+"/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"ping"}]}`)
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
		code, body := fire(t, px)
		if len(paths) != 1 || paths[0] != "/v1/messages" {
			t.Errorf("upstream paths = %v, want one /v1/messages (converted off the dead chat leg)", paths)
		}
		if code != 200 {
			t.Errorf("client status = %d body=%s, want 200 (converted leg must serve the client)", code, body)
		}
		if !strings.Contains(body, "pong") {
			t.Errorf("client body = %s, want the converted response text", body)
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
		code, body := fire(t, px)
		if len(paths) != 1 || paths[0] != "/v1/messages" {
			t.Errorf("upstream paths = %v, want one /v1/messages (unknown leg tried before the dead one)", paths)
		}
		if code != 200 {
			t.Errorf("client status = %d body=%s, want 200 (unknown leg must serve the client)", code, body)
		}
		if !strings.Contains(body, "pong") {
			t.Errorf("client body = %s, want the converted response text", body)
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
		code, body := fire(t, px)
		if len(paths) != 1 || paths[0] != "/chat/completions" {
			t.Errorf("upstream paths = %v, want one /chat/completions (error surfaced, no leg-hopping)", paths)
		}
		if code != http.StatusBadRequest {
			t.Errorf("client status = %d body=%s, want 400 (upstream error must surface, never a 502)", code, body)
		}
		if !strings.Contains(body, "40403") && !strings.Contains(body, "Model not supported") {
			t.Errorf("client body = %s, want the upstream rejection detail to remain visible", body)
		}
	})
}

// TestModelCaps_TransientFailureDoesNotFlapConcludedVerdict: a re-probe pass
// (here: a sibling model's unknown leg keeps the provider eligible) must not
// downgrade a model's previously CONCLUDED legs when the re-probe hits a
// transient 429 (the zcode/zhipu "? unknown" flapping): the partially
// concluded model keeps its yes legs through the storm and concludes fully
// once the throttle lifts. Fully concluded models are not re-probed at all
// (see TestModelCaps_ConcludedModelsNotReprobedAsCollateral).
func TestModelCaps_TransientFailureDoesNotFlapConcludedVerdict(t *testing.T) {
	// phase 0: chat/anthropic 200, responses 500 (partially conclusive)
	// phase 1: everything 429 (the storm)
	// phase 2: everything 200 (recovery)
	var phase atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		switch phase.Load() {
		case 0:
			if r.URL.Path == "/responses" {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Write([]byte(`{}`))
		case 1:
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":{"code":"1302","message":"Concurrency limit reached"}}`))
		default:
			w.Write([]byte(`{}`))
		}
	}))
	defer up.Close()
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"p": {OpenAIBaseURL: up.URL, AnthropicBaseURL: up.URL, Provider: testProviderID, Models: []string{"m1"}},
		},
		Routes: map[string][]configdomain.RouteTarget{},
	}
	p := newTestProxy(t, cfg)
	p.providers["p"] = &testProv{key: "k"}

	// Pass 1: m1 concludes chat/anthropic yes, responses unknown (5xx).
	p.probeAllModelCaps(context.Background())
	if mp, _ := p.modelCaps.Get("p", "m1"); mp.Chat != triYes || mp.Anthropic != triYes || mp.Responses != triUnknown {
		t.Fatalf("pass 1 m1 = %+v, want yes/yes/unknown", mp)
	}

	// A new model joins the config (fingerprint unchanged) while the upstream
	// starts rate-limiting: the pass re-probes m1 (unconcluded responses leg).
	prov := cfg.Providers["p"]
	prov.Models = []string{"m1", "m2"}
	cfg.Providers["p"] = prov
	phase.Store(1)
	p.probeAllModelCaps(context.Background())

	// m1 keeps its concluded legs despite the 429s (merge-on-unknown); the
	// unconcluded responses leg stays unknown; m2 lands all-unknown.
	if mp, _ := p.modelCaps.Get("p", "m1"); mp.Chat != triYes || mp.Anthropic != triYes || mp.Responses != triUnknown {
		t.Errorf("m1 after throttled pass = %+v, want prior yes/yes retained + responses unknown (no flap)", mp)
	}
	if mp, _ := p.modelCaps.Get("p", "m2"); mp.Chat != triUnknown || mp.Responses != triUnknown {
		t.Errorf("m2 after throttled pass = %+v, want unknown (transient, no prior verdict)", mp)
	}

	// Recovery: the throttle lifts and the next pass concludes everything.
	phase.Store(2)
	p.probeAllModelCaps(context.Background())
	if mp, _ := p.modelCaps.Get("p", "m2"); mp.Chat != triYes {
		t.Errorf("m2 after recovery = %+v, want chat yes", mp)
	}
	if mp, _ := p.modelCaps.Get("p", "m1"); mp != (runtimewire.ModelProtocols{Chat: triYes, Anthropic: triYes, Responses: triYes}) {
		t.Errorf("m1 after recovery = %+v, want yes/yes/yes", mp)
	}
}

// TestModelCaps_ConcludedModelsNotReprobedAsCollateral: when one model's
// unconcluded leg keeps a provider eligible for a pass, the provider's
// FULLY concluded models are not re-probed — the collateral re-probe used to
// re-burst the whole provider against rate-limited upstreams (zcode/zhipu:
// adding one model fired 33 requests) and downgrade fine verdicts.
func TestModelCaps_ConcludedModelsNotReprobedAsCollateral(t *testing.T) {
	var chatHits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if r.URL.Path == "/chat/completions" {
			chatHits.Add(1)
		}
		w.Write([]byte(`{}`))
	}))
	defer up.Close()
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"p": {OpenAIBaseURL: up.URL, AnthropicBaseURL: up.URL, Provider: testProviderID, Models: []string{"m1", "m2"}},
		},
		Routes: map[string][]configdomain.RouteTarget{},
	}
	p := newTestProxy(t, cfg)
	p.providers["p"] = &testProv{key: "k"}

	// Both models conclude.
	p.probeAllModelCaps(context.Background())
	first := chatHits.Load()
	if first != 2 {
		t.Fatalf("pass 1 chat hits = %d, want 2 (one per model)", first)
	}

	// A third model joins; the eligibility re-opens the provider. The
	// concluded m1/m2 must not be re-probed: only m3's legs fire.
	prov := cfg.Providers["p"]
	prov.Models = []string{"m1", "m2", "m3"}
	cfg.Providers["p"] = prov
	p.probeAllModelCaps(context.Background())
	if got := chatHits.Load(); got != 3 {
		t.Errorf("chat hits after adding m3 = %d, want 3 (m1/m2 skipped as concluded, only m3 probed)", got)
	}
	for _, m := range []string{"m1", "m2", "m3"} {
		if mp, ok := p.modelCaps.Get("p", m); !ok || !mp.Concluded() {
			t.Errorf("%s = %+v (ok=%v), want concluded", m, mp, ok)
		}
	}
}

// TestModelCaps_ReloadRereadsFileFromDisk: reload re-reads model_caps.json
// instead of restoring the daemon's own in-memory snapshot, so CLI-written
// verdicts (`models refresh` persists the file, then SIGHUPs) reach the
// running daemon — previously the daemon stayed split-brained until restart
// and its next async persist clobbered the file with stale verdicts.
func TestModelCaps_ReloadRereadsFileFromDisk(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Write([]byte(`{}`))
	}))
	defer up.Close()
	statePath := filepath.Join(t.TempDir(), "quota_state.json")
	capsPath := runtimewire.ModelCapsPath(statePath)
	cfgFile := filepath.Join(filepath.Dir(statePath), "config.yaml")
	if err := os.WriteFile(cfgFile, []byte("providers:\n  p: {provider_id: static, openai_base_url: "+up.URL+"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"p": {OpenAIBaseURL: up.URL, Provider: "static", Models: []string{"m1"}},
		},
		Routes: map[string][]configdomain.RouteTarget{},
	}
	p := newTestProxyAt(t, cfg, statePath)

	// In-memory state: a 429-storm pass left the leg unknown.
	fp := providerbuild.ProtocolConfigFingerprint(cfg.Providers["p"])
	p.modelCaps.Put("p", fp, "m1", runtimewire.ModelProtocols{Chat: runtimewire.Unknown, Anthropic: runtimewire.No, Responses: runtimewire.No}, time.Now())

	// The CLI (or another process) writes fresh verdicts to the file.
	if err := runtimewire.SaveModelCapsFile(capsPath, map[string]runtimewire.ProviderModelCaps{
		"p": {Fingerprint: fp, ProbedAt: time.Now(), Models: map[string]runtimewire.ModelProtocols{
			"m1": {Chat: runtimewire.Yes, Anthropic: runtimewire.Yes, Responses: runtimewire.No},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	if err := p.Reload(cfgFile); err != nil {
		t.Fatal(err)
	}
	if mp, ok := p.modelCaps.Get("p", "m1"); !ok || mp.Chat != runtimewire.Yes || mp.Anthropic != runtimewire.Yes {
		t.Errorf("after reload m1 = %+v (ok=%v), want the FILE verdicts (yes/yes) — reload must re-read model_caps.json", mp, ok)
	}
}

// TestModelCaps_ProtocolHintOverwritesUnconcludedLegacyEntry: a hint-covered
// provider (codex → responses) synthesizes its authoritative verdict over a
// legacy entry whose legs never concluded — without the overwrite the entry
// lingers as "? unknown" forever (the hint pass skipped any existing entry).
func TestModelCaps_ProtocolHintOverwritesUnconcludedLegacyEntry(t *testing.T) {
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"codex": {OpenAIBaseURL: "https://codex.invalid", Provider: "codex", Models: []string{"gpt-6-sol"}},
		},
		Routes: map[string][]configdomain.RouteTarget{},
	}
	p := newTestProxy(t, cfg)

	// Legacy entry from a pass before the provider was hint-covered: all
	// unknown, fingerprint matching current config.
	fp := providerbuild.ProtocolConfigFingerprint(cfg.Providers["codex"])
	p.modelCaps.Put("codex", fp, "gpt-6-sol", runtimewire.ModelProtocols{Chat: runtimewire.Unknown, Anthropic: runtimewire.Unknown, Responses: runtimewire.Unknown}, time.Now())

	p.probeAllModelCaps(context.Background())
	mp, ok := p.modelCaps.Get("codex", "gpt-6-sol")
	if !ok || mp != (runtimewire.ModelProtocols{Chat: runtimewire.No, Anthropic: runtimewire.No, Responses: runtimewire.Yes}) {
		t.Errorf("hint synthesis over legacy unknown = %+v (ok=%v), want no/no/yes", mp, ok)
	}
}

// TestModelCaps_DisabledModelsNotProbed: operator-disabled models never hit
// the upstream — not probed, not eligible (a provider whose every model is
// disabled is skipped entirely), and their stored verdicts stay FROZEN in the
// store (not pruned: the operator blocked the model, not the store's memory
// of it). One disabled model on a not-logged-in provider used to fire 36
// auth-error legs per pass.
func TestModelCaps_DisabledModelsNotProbed(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		hits.Add(1)
		w.Write([]byte(`{}`))
	}))
	defer up.Close()
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"p": {OpenAIBaseURL: up.URL, AnthropicBaseURL: up.URL, Provider: testProviderID, Models: []string{"m1", "m2"}},
			// All-disabled provider: must not send a single request.
			"q": {OpenAIBaseURL: up.URL, AnthropicBaseURL: up.URL, Provider: testProviderID, Models: []string{"q1"}},
		},
		Routes: map[string][]configdomain.RouteTarget{},
	}
	p := newTestProxy(t, cfg)
	p.providers["p"] = &testProv{key: "k"}
	p.providers["q"] = &testProv{key: "k"}
	p.runtimeState.RestoreDisabledModels(map[string][]string{
		"p": {"m2"},
		"q": {"q1"},
	})

	// m2 carries a frozen verdict from before the disable.
	fpP := providerbuild.ProtocolConfigFingerprint(cfg.Providers["p"])
	p.modelCaps.Put("p", fpP, "m2", runtimewire.ModelProtocols{Chat: runtimewire.Yes, Anthropic: runtimewire.No, Responses: runtimewire.No}, time.Now())

	p.probeAllModelCaps(context.Background())

	// Only m1's three legs fired (m2 and q1 are disabled — no requests).
	if got := hits.Load(); got != 3 {
		t.Errorf("upstream hits = %d, want 3 (m1's legs only; disabled m2/q1 not probed)", got)
	}
	if mp, ok := p.modelCaps.Get("p", "m1"); !ok || mp.Chat != triYes {
		t.Errorf("m1 = %+v (ok=%v), want probed chat yes", mp, ok)
	}
	if mp, ok := p.modelCaps.Get("p", "m2"); !ok || mp.Chat != triYes || mp.Anthropic != triNo {
		t.Errorf("m2 = %+v (ok=%v), want frozen verdict preserved verbatim", mp, ok)
	}
}

// TestModelCaps_AsyncPersistDoesNotClobberExternalWrite: the CLI `models
// refresh` writes model_caps.json directly and then SIGHUPs the daemon. If
// one of the daemon's EARLIER-triggered async persists runs in the window
// between the CLI's write and the reload's disk re-read, it must NOT
// overwrite the file with the daemon's older in-memory snapshot (the
// split-brain the reload re-read was added to fix, re-entering through the
// persist side). The persist must notice the file is newer than the state
// the in-memory store was derived from and ADOPT the external state instead
// of writing (see TestModelCaps_AsyncPersistAdoptsExternalWriteWithoutReload
// for the no-reload latch the adoption also fixes).
func TestModelCaps_AsyncPersistDoesNotClobberExternalWrite(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Write([]byte(`{}`))
	}))
	defer up.Close()
	statePath := filepath.Join(t.TempDir(), "quota_state.json")
	capsPath := runtimewire.ModelCapsPath(statePath)
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"p": {OpenAIBaseURL: up.URL, Provider: "static", Models: []string{"m1"}},
		},
		Routes: map[string][]configdomain.RouteTarget{},
	}
	p := newTestProxyAt(t, cfg, statePath)
	fp := providerbuild.ProtocolConfigFingerprint(cfg.Providers["p"])

	// The daemon's in-memory snapshot is OLDER and inconclusive (a throttled
	// pass left the chat leg unknown).
	p.modelCaps.Put("p", fp, "m1", runtimewire.ModelProtocols{Chat: runtimewire.Unknown, Anthropic: runtimewire.No, Responses: runtimewire.No}, time.Now())

	// The CLI (an external writer) persists fresh concluded verdicts.
	if err := runtimewire.SaveModelCapsFile(capsPath, map[string]runtimewire.ProviderModelCaps{
		"p": {Fingerprint: fp, ProbedAt: time.Now(), Models: map[string]runtimewire.ModelProtocols{
			"m1": {Chat: runtimewire.Yes, Anthropic: runtimewire.Yes, Responses: runtimewire.No},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	// An earlier-triggered async persist executes now — before any reload
	// re-read could adopt the CLI's verdicts. (Synchronous core: the async
	// wrapper's scheduling is quota-tracker plumbing, not under test.)
	p.persistModelCapsNow()

	loaded, err := runtimewire.LoadModelCapsFile(capsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded["p"].Models["m1"]; got.Chat != runtimewire.Yes || got.Anthropic != runtimewire.Yes {
		t.Errorf("file after async persist = chat:%s anthropic:%s, want the CLI's yes/yes preserved — the daemon's stale snapshot clobbered the external write",
			got.Chat, got.Anthropic)
	}
}

// TestModelCaps_AsyncPersistWritesWhenNoExternalWrite: the skip guard must
// not suppress the daemon's own persists — with no external writer the
// in-memory snapshot lands on disk as before.
func TestModelCaps_AsyncPersistWritesWhenNoExternalWrite(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Write([]byte(`{}`))
	}))
	defer up.Close()
	statePath := filepath.Join(t.TempDir(), "quota_state.json")
	capsPath := runtimewire.ModelCapsPath(statePath)
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"p": {OpenAIBaseURL: up.URL, Provider: "static", Models: []string{"m1"}},
		},
		Routes: map[string][]configdomain.RouteTarget{},
	}
	p := newTestProxyAt(t, cfg, statePath)
	fp := providerbuild.ProtocolConfigFingerprint(cfg.Providers["p"])
	p.modelCaps.Put("p", fp, "m1", runtimewire.ModelProtocols{Chat: runtimewire.Yes, Anthropic: runtimewire.No, Responses: runtimewire.No}, time.Now())

	p.persistModelCapsNow()

	loaded, err := runtimewire.LoadModelCapsFile(capsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded["p"].Models["m1"]; got.Chat != runtimewire.Yes {
		t.Errorf("file after async persist = %+v, want the daemon's own snapshot persisted (chat:yes)", got)
	}
}

// TestModelCaps_PersistProceedsAfterReloadBaseline: a reload re-reads the
// file and re-baselines it; the daemon's own persists AFTER the reload must
// still write (the skip guard compares against the re-read state, not
// against the reload act itself) — e.g. a runtime 404 correction right after
// a CLI-triggered reload must reach disk.
func TestModelCaps_PersistProceedsAfterReloadBaseline(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Write([]byte(`{}`))
	}))
	defer up.Close()
	statePath := filepath.Join(t.TempDir(), "quota_state.json")
	capsPath := runtimewire.ModelCapsPath(statePath)
	cfgFile := filepath.Join(filepath.Dir(statePath), "config.yaml")
	if err := os.WriteFile(cfgFile, []byte("providers:\n  p: {provider_id: static, openai_base_url: "+up.URL+"}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"p": {OpenAIBaseURL: up.URL, Provider: "static", Models: []string{"m1"}},
		},
		Routes: map[string][]configdomain.RouteTarget{},
	}
	p := newTestProxyAt(t, cfg, statePath)
	fp := providerbuild.ProtocolConfigFingerprint(cfg.Providers["p"])

	// The CLI writes fresh verdicts and the daemon reloads (adopting them).
	if err := runtimewire.SaveModelCapsFile(capsPath, map[string]runtimewire.ProviderModelCaps{
		"p": {Fingerprint: fp, ProbedAt: time.Now(), Models: map[string]runtimewire.ModelProtocols{
			"m1": {Chat: runtimewire.Yes, Anthropic: runtimewire.Yes, Responses: runtimewire.Yes},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.Reload(cfgFile); err != nil {
		t.Fatal(err)
	}

	// A runtime correction (e.g. the model-level 404 correction) lands in the
	// in-memory store and must reach disk via the normal async persist.
	p.modelCaps.Put("p", fp, "m1", runtimewire.ModelProtocols{Chat: runtimewire.Yes, Anthropic: runtimewire.Yes, Responses: runtimewire.No}, time.Now())
	p.persistModelCapsNow()

	loaded, err := runtimewire.LoadModelCapsFile(capsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded["p"].Models["m1"]; got.Responses != runtimewire.No {
		t.Errorf("file after post-reload persist = %+v, want responses:no (the correction persisted) — the skip guard must not latch", got)
	}
}

// TestModelCaps_AsyncPersistAdoptsExternalWriteWithoutReload (latch
// regression): when an external writer (CLI `models refresh`) publishes a
// newer model_caps.json and NO reload follows (the CLI only SIGHUPs on a
// model-set change / successful verdict persist, and the signal can miss),
// the mtime skip-guard used to LATCH — the file stayed newer than the
// baseline forever, so every later daemon persist was suppressed and daemon
// verdicts never reached disk again. The guard must instead ADOPT the
// external state (re-read + Restore + re-baseline) and let the daemon's own
// later persists proceed without losing either side's verdicts.
func TestModelCaps_AsyncPersistAdoptsExternalWriteWithoutReload(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Write([]byte(`{}`))
	}))
	defer up.Close()
	statePath := filepath.Join(t.TempDir(), "quota_state.json")
	capsPath := runtimewire.ModelCapsPath(statePath)
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"p": {OpenAIBaseURL: up.URL, Provider: "static", Models: []string{"m1", "m2", "m3"}},
		},
		Routes: map[string][]configdomain.RouteTarget{},
	}
	p := newTestProxyAt(t, cfg, statePath)
	fp := providerbuild.ProtocolConfigFingerprint(cfg.Providers["p"])

	// The daemon's in-memory snapshot is OLDER and inconclusive.
	p.modelCaps.Put("p", fp, "m1", runtimewire.ModelProtocols{Chat: runtimewire.Unknown, Anthropic: runtimewire.No, Responses: runtimewire.No}, time.Now())

	// The CLI (external writer) persists fresh concluded verdicts — newer
	// than the daemon's baseline — and no reload ever re-reads the file.
	if err := runtimewire.SaveModelCapsFile(capsPath, map[string]runtimewire.ProviderModelCaps{
		"p": {Fingerprint: fp, ProbedAt: time.Now(), Models: map[string]runtimewire.ModelProtocols{
			"m1": {Chat: runtimewire.Yes, Anthropic: runtimewire.Yes, Responses: runtimewire.No},
			"m2": {Chat: runtimewire.Yes, Anthropic: runtimewire.No, Responses: runtimewire.No},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	// The daemon's persist must NOT write over the external file — it ADOPTS
	// it: the in-memory store now serves the CLI's verdicts...
	p.persistModelCapsNow()
	if mp, ok := p.modelCaps.Get("p", "m1"); !ok || mp.Chat != runtimewire.Yes || mp.Anthropic != runtimewire.Yes {
		t.Errorf("after adopt m1 = %+v (ok=%v), want the external yes/yes adopted in-memory", mp, ok)
	}
	// ...and the file itself is untouched.
	loaded, err := runtimewire.LoadModelCapsFile(capsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded["p"].Models["m1"]; got.Chat != runtimewire.Yes || got.Anthropic != runtimewire.Yes {
		t.Errorf("file after adopt = %+v, want the CLI's yes/yes preserved", got)
	}

	// The guard did NOT latch: a later daemon persist (e.g. a probe pass or
	// 404 correction) writes again, keeping BOTH sides' verdicts.
	p.modelCaps.Put("p", fp, "m3", runtimewire.ModelProtocols{Chat: runtimewire.Yes, Anthropic: runtimewire.No, Responses: runtimewire.Yes}, time.Now())
	p.persistModelCapsNow()
	loaded, err = runtimewire.LoadModelCapsFile(capsPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded["p"].Models["m3"]; got.Chat != runtimewire.Yes || got.Responses != runtimewire.Yes {
		t.Errorf("file after post-adopt persist m3 = %+v, want the daemon's new verdict persisted (guard unlatched)", got)
	}
	if got := loaded["p"].Models["m2"]; got.Chat != runtimewire.Yes {
		t.Errorf("file after post-adopt persist m2 = %+v, want the CLI's verdict carried along (not lost)", got)
	}
}

// TestModelCaps_PoolVirtualDisabledNotProbed: the probe-side disabled filter
// expands the same keys TargetDisabled matches — a model disabled under a
// POOL-VIRTUAL key ("zhipu#acct1") is out of rotation there, and the pass
// probes once per parent with a shared parent-keyed verdict, so it must not
// be probed either (previously only the config-level parent key was checked:
// the disable blocked routing but the model was still probed).
func TestModelCaps_PoolVirtualDisabledNotProbed(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		hits.Add(1)
		w.Write([]byte(`{}`))
	}))
	defer up.Close()
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"zhipu": {OpenAIBaseURL: up.URL, AnthropicBaseURL: up.URL, Provider: testProviderID, Models: []string{"m1", "m2"}},
		},
		Routes: map[string][]configdomain.RouteTarget{},
	}
	p := newTestProxy(t, cfg)
	p.poolIndex["zhipu"] = []string{"zhipu#acct1"}
	p.providers["zhipu#acct1"] = &testProv{key: "k"}
	p.runtimeState.RestoreDisabledModels(map[string][]string{
		"zhipu#acct1": {"m2"}, // virtual-level disable: blocks routing, must also skip probing
	})

	p.probeAllModelCaps(context.Background())

	if got := hits.Load(); got != 3 {
		t.Errorf("upstream hits = %d, want 3 (m1's legs only; virtual-disabled m2 not probed)", got)
	}
	if mp, ok := p.modelCaps.Get("zhipu", "m1"); !ok || mp.Chat != triYes {
		t.Errorf("m1 = %+v (ok=%v), want probed chat yes", mp, ok)
	}
	if _, ok := p.modelCaps.Get("zhipu", "m2"); ok {
		t.Error("m2 probed despite the pool-virtual disable")
	}
}
