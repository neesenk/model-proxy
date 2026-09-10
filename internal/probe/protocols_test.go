package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/provider"
)

// matrixImpl answers per-path so tests can steer each protocol leg's verdict.
type matrixImpl struct {
	stubImpl
	mu       sync.Mutex
	statusBy map[string]int
	bodies   map[string][]byte
	seen     map[string][]byte
}

func (m *matrixImpl) handler(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	m.seen[r.URL.Path] = b
	m.mu.Unlock()
	status := m.statusBy[r.URL.Path]
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	if body := m.bodies[r.URL.Path]; body != nil {
		_, _ = w.Write(body)
	}
}

func newMatrixServer(t *testing.T, m *matrixImpl) *httptest.Server {
	t.Helper()
	m.seen = map[string][]byte{}
	srv := httptest.NewServer(http.HandlerFunc(m.handler))
	t.Cleanup(srv.Close)
	return srv
}

func legByName(results []LegResult, leg Leg) LegResult {
	for _, r := range results {
		if r.Leg == leg {
			return r
		}
	}
	return LegResult{Leg: leg}
}

func TestProbeModelProtocolsBasesAndBodies(t *testing.T) {
	m := &matrixImpl{statusBy: map[string]int{}}
	srv := newMatrixServer(t, m)
	prov := configdomain.Provider{OpenAIBaseURL: srv.URL, AnthropicBaseURL: srv.URL}
	results := ProbeModelProtocols(context.Background(), srv.Client(), prov, matrixImpl{}, "m")
	if len(results) != 3 {
		t.Fatalf("got %d leg results, want 3", len(results))
	}
	for _, r := range results {
		if !r.Probed || r.Status != 200 || r.Err != nil {
			t.Errorf("leg %s = %+v, want probed 200", r.Leg, r)
		}
	}
	// Each leg hit its canonical path with its generic body shape.
	for path, key := range map[string]string{
		"/chat/completions": "max_tokens",
		"/v1/messages":      "max_tokens",
		"/responses":        "max_output_tokens",
	} {
		body := string(m.seen[path])
		if !strings.Contains(body, key) {
			t.Errorf("%s body = %s, want %s key", path, body, key)
		}
	}
	// The responses leg must NOT carry anthropic/chat params.
	if strings.Contains(string(m.seen["/responses"]), "messages") {
		t.Errorf("responses body carries messages: %s", m.seen["/responses"])
	}
}

func TestProbeModelProtocolsUnconfiguredBasesNotProbed(t *testing.T) {
	m := &matrixImpl{statusBy: map[string]int{}}
	srv := newMatrixServer(t, m)
	// Only anthropic base configured: chat/responses unprobed, anthropic probed.
	prov := configdomain.Provider{AnthropicBaseURL: srv.URL}
	results := ProbeModelProtocols(context.Background(), srv.Client(), prov, matrixImpl{}, "m")
	if r := legByName(results, LegChat); r.Probed {
		t.Errorf("chat leg probed without openai base: %+v", r)
	}
	if r := legByName(results, LegResponses); r.Probed {
		t.Errorf("responses leg probed without openai base: %+v", r)
	}
	if r := legByName(results, LegAnthropic); !r.Probed || r.Status != 200 {
		t.Errorf("anthropic leg = %+v, want probed 200", r)
	}
	if len(m.seen) != 1 || m.seen["/v1/messages"] == nil {
		t.Errorf("server saw %v, want only /v1/messages", keysOf(m.seen))
	}
}

func TestProbeModelProtocolsPrefersImplDialectBody(t *testing.T) {
	// codex-shaped impl: ProbeRequest path /responses with a dialect body —
	// the responses leg must send THAT body, not the generic one.
	m := &matrixImpl{statusBy: map[string]int{}}
	srv := newMatrixServer(t, m)
	prov := configdomain.Provider{OpenAIBaseURL: srv.URL}
	results := ProbeModelProtocols(context.Background(), srv.Client(), prov, codexShapedImpl{}, "gpt-5.6")
	if r := legByName(results, LegResponses); !r.Probed || r.Status != 200 {
		t.Fatalf("responses leg = %+v", r)
	}
	got := string(m.seen["/responses"])
	if !strings.Contains(got, `"stream":true`) || !strings.Contains(got, `"input":[`) {
		t.Errorf("responses leg body = %s, want codex dialect (input list + stream:true)", got)
	}
}

func TestProbeModelProtocolsRetryPerLeg(t *testing.T) {
	// The anthropic leg rejects max_tokens once, then accepts the renamed
	// max_completion_tokens — the retry applies per leg.
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/messages" {
			calls++
			b, _ := io.ReadAll(r.Body)
			if calls == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"Unsupported parameter: 'max_tokens'. Use 'max_completion_tokens' instead."}}`))
				return
			}
			if !strings.Contains(string(b), "max_completion_tokens") {
				t.Errorf("retry body = %s, want max_completion_tokens", b)
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	prov := configdomain.Provider{OpenAIBaseURL: srv.URL, AnthropicBaseURL: srv.URL}
	results := ProbeModelProtocols(context.Background(), srv.Client(), prov, matrixImpl{}, "gpt-5.5")
	if r := legByName(results, LegAnthropic); r.Status != 200 {
		t.Errorf("anthropic leg = %+v, want 200 after retry", r)
	}
	if calls != 2 {
		t.Errorf("anthropic calls = %d, want 2 (initial + retry)", calls)
	}
}

// TestProbeModelsBatchOrderAndConcurrency: the batch helper preserves input
// order and bounds MODEL-level concurrency — each model fans its own legs out
// concurrently (chat+responses here, no anthropic base), so HTTP-level
// in-flight can reach concurrency × legs but never more.
func TestProbeModelsBatchOrderAndConcurrency(t *testing.T) {
	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		inFlight++
		if inFlight > maxInFlight {
			maxInFlight = inFlight
		}
		mu.Unlock()
		time.Sleep(5 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ids := []string{"m1", "m2", "m3", "m4", "m5"}
	prov := configdomain.Provider{OpenAIBaseURL: srv.URL}
	out := ProbeModels(context.Background(), srv.Client(), prov, matrixImpl{}, ids, 2)
	if len(out) != len(ids) {
		t.Fatalf("results = %d, want %d", len(out), len(ids))
	}
	for i, id := range ids {
		if out[i].ID != id {
			t.Errorf("results[%d].ID = %q, want %q (input order preserved)", i, out[i].ID, id)
		}
		if r := legByName(out[i].Legs, LegChat); r.Status != 200 {
			t.Errorf("results[%d] chat leg = %+v, want 200", i, r)
		}
	}
	// 2 models in flight × 2 probed legs each = 4; a 3rd concurrent model
	// would push it to 6.
	if maxInFlight > 4 {
		t.Errorf("max in-flight = %d, want <= 4 (2 models × 2 legs)", maxInFlight)
	}
}

func keysOf(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// codexShapedImpl mimics CodexProvider's /responses dialect probe.
type codexShapedImpl struct{ stubImpl }

func (codexShapedImpl) ProbeRequest(modelID string) provider.ProbeRequest {
	body, _ := json.Marshal(map[string]any{
		"model":  modelID,
		"input":  []map[string]string{{"role": "user", "content": "hi"}},
		"stream": true,
	})
	return provider.ProbeRequest{Method: http.MethodPost, Path: "/responses", Body: body}
}

func TestPickModel(t *testing.T) {
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"with-models": {Models: []string{"m1", "m2"}},
			"routed":      {},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"r": {{Provider: "routed", Model: "route-m"}},
		},
	}
	derived := map[string][]configdomain.RouteTarget{
		"d": {{Provider: "derived-only", Model: "derived-m"}},
	}
	if got := PickModel(cfg, derived, "with-models"); got != "m1" {
		t.Errorf("with-models = %q, want m1 (config models first)", got)
	}
	if got := PickModel(cfg, derived, "routed"); got != "route-m" {
		t.Errorf("routed = %q, want route-m", got)
	}
	if got := PickModel(cfg, derived, "derived-only"); got != "derived-m" {
		t.Errorf("derived-only = %q, want derived-m", got)
	}
	if got := PickModel(cfg, derived, "missing"); got != "" {
		t.Errorf("missing = %q, want empty", got)
	}
}

func TestDoBodyLimitAndAccept(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Errorf("Accept = %q, want text/event-stream", got)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Repeat("x", 100)))
	}))
	defer srv.Close()
	rep, err := Do(context.Background(), srv.Client(),
		configdomain.Provider{OpenAIBaseURL: srv.URL}, stubImpl{},
		Request{BaseURL: srv.URL, Path: "/chat/completions", Accept: "text/event-stream", BodyLimit: 10})
	if err != nil || rep.Status != 200 {
		t.Fatalf("Do = %+v, %v", rep, err)
	}
	if len(rep.Body) != 10 {
		t.Errorf("body len = %d, want capped at 10", len(rep.Body))
	}
	if rep.Latency <= 0 {
		t.Errorf("Latency = %v, want measured", rep.Latency)
	}
}

func TestAttachProbeToolShapesPerLeg(t *testing.T) {
	cases := []struct {
		leg      Leg
		body     string
		wantPath []string // dotted key path to the tools array
		wantKey  string   // distinguishing key inside the tool object
	}{
		{LegChat, `{"model":"m","messages":[]}`, nil, "function"},
		{LegAnthropic, `{"model":"m","messages":[],"max_tokens":1}`, nil, "input_schema"},
		{LegResponses, `{"model":"m","input":"hi"}`, nil, "parameters"},
	}
	for _, c := range cases {
		out := attachProbeTool([]byte(c.body), c.leg)
		var doc map[string]any
		if err := json.Unmarshal(out, &doc); err != nil {
			t.Fatalf("%s: result not JSON: %v", c.leg, err)
		}
		if doc["model"] != "m" {
			t.Errorf("%s: original fields lost: %s", c.leg, out)
		}
		tools, ok := doc["tools"].([]any)
		if !ok || len(tools) != 1 {
			t.Fatalf("%s: tools = %v, want exactly one declaration", c.leg, doc["tools"])
		}
		tool := tools[0].(map[string]any)
		if c.leg == LegChat {
			if _, ok := tool["function"].(map[string]any); !ok {
				t.Errorf("chat leg: tool missing function envelope: %v", tool)
			}
		} else if _, ok := tool[c.wantKey]; !ok {
			t.Errorf("%s leg: tool missing %q: %v", c.leg, c.wantKey, tool)
		}
	}
	// Unparseable body passes through unchanged.
	raw := []byte("not-json")
	if got := attachProbeTool(raw, LegChat); string(got) != string(raw) {
		t.Errorf("invalid body must pass through, got %s", got)
	}
}

func TestProbeLegAttachesToolsToDialectBody(t *testing.T) {
	// A provider dialect body (path matches the leg) wins, but the tool
	// declaration must still be merged in — the tools-attached measurement
	// applies to dialect legs too.
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		fmt.Fprint(w, `{"choices":[]}`)
	}))
	defer srv.Close()
	impl := dialectImpl{path: "/chat/completions", body: `{"model":"m","dialect":true}`}
	prov := configdomain.Provider{OpenAIBaseURL: srv.URL}
	res := probeLeg(context.Background(), srv.Client(), prov, impl, impl.ProbeRequest("m"), "m", LegChat)
	if !res.Probed || res.Status != 200 {
		t.Fatalf("probeLeg = %+v, want probed 200", res)
	}
	var doc map[string]any
	if err := json.Unmarshal(gotBody, &doc); err != nil {
		t.Fatalf("upstream got non-JSON: %s", gotBody)
	}
	if doc["dialect"] != true {
		t.Errorf("dialect body lost: %s", gotBody)
	}
	if _, ok := doc["tools"].([]any); !ok {
		t.Errorf("tools not attached to dialect body: %s", gotBody)
	}
}

type dialectImpl struct {
	stubImpl
	path, body string
}

func (d dialectImpl) ProbeRequest(modelID string) provider.ProbeRequest {
	return provider.ProbeRequest{Method: http.MethodPost, Path: d.path, Body: []byte(d.body)}
}

// TestProbeProviderOpenAILegsAgentGrade pins the provider-level convergence:
// both openai legs go through the same pipeline as model-level probes —
// function-tool declaration attached, impl dialect merged, max_completion_tokens
// retry applied — so a provider-level yes means callable WITH tools.
func TestProbeProviderOpenAILegsAgentGrade(t *testing.T) {
	var bodiesMu sync.Mutex
	bodies := map[string]string{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodiesMu.Lock()
		bodies[r.URL.Path] = string(b)
		bodiesMu.Unlock()
		if strings.Contains(string(b), `"max_tokens"`) {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"message":"Unsupported parameter: 'max_tokens' ... Use 'max_completion_tokens' instead."}}`))
			return
		}
		w.Write([]byte(`{}`))
	}))
	defer up.Close()

	prov := configdomain.Provider{OpenAIBaseURL: up.URL}
	impl := stubImpl{}
	chat, responses := ProbeProviderOpenAILegs(context.Background(), http.DefaultClient, prov, impl, "m1")
	if chat.Err != nil || responses.Err != nil {
		t.Fatalf("probe errors: chat=%v responses=%v", chat.Err, responses.Err)
	}
	if chat.Status != 200 || responses.Status != 200 {
		t.Fatalf("statuses = chat:%d responses:%d, want 200/200 after max_completion_tokens retry", chat.Status, responses.Status)
	}
	for path, body := range bodies {
		if !strings.Contains(body, `"get_weather"`) {
			t.Errorf("leg %s probe body is not agent-grade (no function tool): %s", path, body)
		}
	}
	if len(bodies) != 2 {
		t.Errorf("probed paths = %v, want chat + responses", bodies)
	}
}
