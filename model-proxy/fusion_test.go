package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	observeevents "model-proxy/internal/observe/events"
	"model-proxy/internal/observe/requestlog"
)

// fusion_test.go covers the fusion orchestration engine (panel → synthesis):
// fan-out + synthesis, partial failure / quorum fallback, the tool round,
// quorum+grace timing, cross-protocol panels, circuit-breaker wiring, usage
// parsing, and runtime failure handling.

// fakeUpstream records every request body + path and answers with a
// configurable responder.
type fakeUpstream struct {
	srv    *httptest.Server
	mu     sync.Mutex
	bodies []string
	paths  []string
}

func newFakeUpstream(t *testing.T, respond http.HandlerFunc) *fakeUpstream {
	t.Helper()
	f := &fakeUpstream{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		f.mu.Lock()
		f.bodies = append(f.bodies, string(body))
		f.paths = append(f.paths, r.URL.Path)
		f.mu.Unlock()
		respond(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeUpstream) hits() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

func (f *fakeUpstream) lastBody() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		return ""
	}
	return f.bodies[len(f.bodies)-1]
}

func (f *fakeUpstream) lastPath() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.paths) == 0 {
		return ""
	}
	return f.paths[len(f.paths)-1]
}

// --- responders ---

func anthropicDraftResponder(text string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprintf(w, `{"id":"msg_d","type":"message","role":"assistant","content":[{"type":"text","text":%q}],"usage":{"input_tokens":11,"output_tokens":7}}`, text)
	}
}

func openaiDraftResponder(text string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprintf(w, `{"id":"chatcmpl_d","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":21,"completion_tokens":9}}`, text)
	}
}

// responsesDraftResponder answers a native responses-API leg with the
// responses output[] shape (no choices[]/content[] at the top level).
func responsesDraftResponder(text string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		fmt.Fprintf(w, `{"id":"resp_d","status":"completed","output":[{"type":"reasoning","summary":[]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":%q}]}],"usage":{"input_tokens":13,"output_tokens":5}}`, text)
	}
}

func statusResponder(code int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(code) }
}

func delayedResponder(d time.Duration, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { time.Sleep(d); next(w, r) }
}

// anthropicSSEResponder streams a plain-text anthropic SSE answer.
func anthropicSSEResponder(text string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_s\",\"model\":\"synth\",\"usage\":{\"input_tokens\":50}}}\n\n")
		io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", text)
		io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":9}}\n\n")
		io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}
}

// anthropicToolUseSSEResponder streams an anthropic SSE answer that IS a tool call.
func anthropicToolUseSSEResponder() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_s\",\"usage\":{\"input_tokens\":50}}}\n\n")
		io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"get_weather\",\"input\":{}}}\n\n")
		io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"city\\\":\\\"Paris\\\"}\"}}\n\n")
		io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":12}}\n\n")
		io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}
}

// newFusionRig builds a Proxy whose only route "hard" is a single fusion target
// referencing recipe "recipe". Every provider named in the recipe must have a
// fake upstream in ups; a member/synthesizer with protocol:openai or
// protocol:responses gets an openai_base_url, everything else an
// anthropic_base_url (client is anthropic).
func newFusionRig(t *testing.T, recipe FusionConfig, ups map[string]*fakeUpstream) (*Proxy, *httptest.Server) {
	t.Helper()
	names := map[string]string{} // provider name → effective protocol
	for _, m := range recipe.Panel {
		names[m.Provider] = m.Protocol
	}
	names[recipe.Synthesizer.Provider] = recipe.Synthesizer.Protocol
	if recipe.Judge != nil {
		names[recipe.Judge.Provider] = recipe.Judge.Protocol
	}
	providers := map[string]Provider{}
	for name, proto := range names {
		up := ups[name]
		if up == nil {
			t.Fatalf("no fake upstream for provider %q", name)
		}
		p := Provider{Provider: testProviderID}
		if proto == "openai" || proto == "responses" {
			p.OpenAIBaseURL = up.srv.URL
		} else {
			p.AnthropicBaseURL = up.srv.URL
		}
		providers[name] = p
	}
	cfg := &Config{
		Listen:    "127.0.0.1:1",
		Providers: providers,
		Routes:    map[string][]RouteTarget{"hard": {{Provider: "fusion", Model: "recipe", Priority: 1}}},
		Fusion:    map[string]FusionConfig{"recipe": recipe},
	}
	proxy := newTestProxy(t, cfg)
	t.Cleanup(proxy.Close) // stop the quota tracker; don't leak a poller past the test
	for name := range names {
		proxy.providers[name] = &testProv{key: name}
	}
	px := httptest.NewServer(http.HandlerFunc(proxy.handler))
	t.Cleanup(px.Close)
	return proxy, px
}

func postAnthropic(t *testing.T, px *httptest.Server, body string) string {
	t.Helper()
	resp, err := http.Post(px.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func recentLiveEvents(p *Proxy) []observeevents.Event {
	return p.events.Snapshot()
}

const fusionClientBody = `{"model":"hard","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"solve X"}]}`

// TestFusion_PooledParentMembers (regression for the unified resolver, #10): a
// Fusion recipe whose panel member AND synthesizer name POOLED parents must still
// run. Pre-fix the parent name had no runtime instance (only "name#<id>" virtuals
// exist), so the member was dropped as "not available" and the synthesizer had no
// impl — fusion broke the moment a second account was added. After the fix both
// resolve to a virtual via the resolver and run.
func TestFusion_PooledParentMembers(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	// Two pooled parents (same provider_id "zhipu", distinct names → distinct pool
	// files + distinct upstreams) so the panel member and synthesizer are separate.
	writePoolFile(t, "zhipu-draft", "zhipu", "ZA", "ZB")
	writePoolFile(t, "zhipu-synth", "zhipu", "GA", "GB")

	draftUp := newFakeUpstream(t, anthropicDraftResponder("draft-ok"))
	synthUp := newFakeUpstream(t, anthropicSSEResponder("pooled synthesis ok"))

	cfg := &Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]Provider{
			"zhipu-draft": {AnthropicBaseURL: draftUp.srv.URL, Provider: "zhipu"},
			"zhipu-synth": {AnthropicBaseURL: synthUp.srv.URL, Provider: "zhipu"},
		},
		Routes: map[string][]RouteTarget{"hard": {{Provider: "fusion", Model: "recipe"}}},
		Fusion: map[string]FusionConfig{"recipe": {
			Panel:       []RouteTarget{{Provider: "zhipu-draft", Model: "zdraft"}},
			Synthesizer: RouteTarget{Provider: "zhipu-synth", Model: "gsynth"},
		}},
	}
	p := newTestProxy(t, cfg)
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	out := postAnthropic(t, px, fusionClientBody)
	if !strings.Contains(out, "pooled synthesis ok") {
		t.Fatalf("client missing synthesis — pooled Fusion members not resolved to virtuals: %s", out)
	}
	if draftUp.hits() == 0 {
		t.Error("pooled panel member (zhipu-draft) never hit — resolver did not resolve it to a virtual")
	}
	if synthUp.hits() == 0 {
		t.Error("pooled synthesizer (zhipu-synth) never hit — resolver did not resolve it to a virtual")
	}
}

// TestFusion_FanOutSynthesis (plan #1): all panel members succeed → the
// synthesis body carries every candidate + the original question, the client
// receives the synthesizer's stream, and every member gets its own
// metrics/tokens/live/request-log record.
func TestFusion_FanOutSynthesis(t *testing.T) {
	pa := newFakeUpstream(t, anthropicDraftResponder("draft-A"))
	pb := newFakeUpstream(t, anthropicDraftResponder("draft-B"))
	pc := newFakeUpstream(t, anthropicDraftResponder("draft-C"))
	ps := newFakeUpstream(t, anthropicSSEResponder("final answer"))
	recipe := FusionConfig{
		Panel: []RouteTarget{
			{Provider: "pa", Model: "ma"},
			{Provider: "pb", Model: "mb"},
			{Provider: "pc", Model: "mc"},
		},
		Synthesizer: RouteTarget{Provider: "ps", Model: "ms"},
	}
	proxy, px := newFusionRig(t, recipe, map[string]*fakeUpstream{"pa": pa, "pb": pb, "pc": pc, "ps": ps})
	// Wire a real request logger so panel-leg records are observable.
	dir := t.TempDir()
	l := requestlog.New(requestlog.Options{
		Directory: dir, MaxFileSize: 1 << 30, MaxBodyBytes: 1 << 20,
	})
	go l.Run()
	t.Cleanup(l.Shutdown)
	proxy.reqLog = l

	out := postAnthropic(t, px, fusionClientBody)
	if !strings.Contains(out, "final answer") {
		t.Fatalf("client body missing synthesizer answer: %s", out)
	}
	// Every panel member hit exactly once with a non-streaming, model-rewritten
	// draft request.
	for name, up := range map[string]*fakeUpstream{"pa": pa, "pb": pb, "pc": pc} {
		if up.hits() != 1 {
			t.Fatalf("member %s hits = %d, want 1", name, up.hits())
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(up.lastBody()), &body); err != nil {
			t.Fatalf("member %s draft body not JSON: %v", name, err)
		}
		if body["stream"] != false {
			t.Errorf("member %s draft stream = %v, want false", name, body["stream"])
		}
		wantModel := map[string]string{"pa": "ma", "pb": "mb", "pc": "mc"}[name]
		if body["model"] != wantModel {
			t.Errorf("member %s draft model = %v, want %s", name, body["model"], wantModel)
		}
		if _, ok := body["tools"]; ok {
			t.Errorf("member %s draft body carries tools", name)
		}
	}
	// Synthesis body: system carries the instruction + all three candidates; the
	// conversation (messages) is untouched and still carries the question.
	var synth struct {
		Model    string `json:"model"`
		System   string `json:"system"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(ps.lastBody()), &synth); err != nil {
		t.Fatalf("synthesis body not JSON: %v", err)
	}
	if synth.Model != "ms" {
		t.Errorf("synthesis model = %q, want ms", synth.Model)
	}
	if !strings.Contains(synth.System, "结果汇总模型") {
		t.Errorf("synthesis system missing fixed instruction: %q", synth.System)
	}
	for _, d := range []string{"draft-A", "draft-B", "draft-C"} {
		if !strings.Contains(synth.System, d) {
			t.Errorf("synthesis system missing candidate %q: %q", d, synth.System)
		}
	}
	if n := strings.Count(synth.System, "<CANDIDATE "); n != 3 {
		t.Errorf("synthesis system has %d candidate blocks, want 3", n)
	}
	if len(synth.Messages) != 1 || synth.Messages[0].Content != "solve X" {
		t.Errorf("synthesis messages = %+v, want the original question only", synth.Messages)
	}
	// Metrics + tokens: one committed request per member and the synthesizer;
	// draft-leg usage (anthropic shape) and synthesizer usage (SSE scan) both
	// land in the token counter.
	metrics := proxy.metrics.snapshot()
	for _, k := range []pmKey{{Provider: "pa", Model: "ma"}, {Provider: "pb", Model: "mb"}, {Provider: "pc", Model: "mc"}, {Provider: "ps", Model: "ms"}} {
		if metrics[k].Requests != 1 {
			t.Errorf("metrics %v requests = %d, want 1", k, metrics[k].Requests)
		}
	}
	toks := proxy.tokens.snapshot()
	if u := toks[tokenKey{Provider: "pa", Model: "ma"}]; u.Input != 11 || u.Output != 7 {
		t.Errorf("member pa usage = %+v, want {11 7}", u)
	}
	if u := toks[tokenKey{Provider: "ps", Model: "ms"}]; u.Input != 50 || u.Output != 9 {
		t.Errorf("synthesizer usage = %+v, want {50 9}", u)
	}
	// Live events: panel legs are marked fusion-panel:<model> (start + end).
	var startOK, endOK bool
	for _, e := range recentLiveEvents(proxy) {
		if e.Provider == "fusion-panel:ma" && e.Type == "start" {
			startOK = true
		}
		if e.Provider == "fusion-panel:ma" && e.Type == "end" && e.Status == 200 {
			endOK = true
		}
	}
	if !startOK || !endOK {
		t.Errorf("fusion-panel live events missing (start=%v end=%v)", startOK, endOK)
	}
	// Request log: 3 panel records (fusion-panel-<i>-<parent>) + the
	// synthesizer record (body contains the candidate sections).
	l.Shutdown()
	records, err := requestlog.QueryRecords(dir, requestlog.Filter{})
	if err != nil {
		t.Fatalf("query Fusion request records: %v", err)
	}
	var panelRecs, synthRecs []requestlog.Record
	for _, r := range records {
		if strings.HasPrefix(r.RequestID, "fusion-panel-") {
			panelRecs = append(panelRecs, r)
		} else if r.Provider == "ps" {
			synthRecs = append(synthRecs, r)
		}
	}
	if len(panelRecs) != 3 {
		t.Fatalf("panel request-log records = %d, want 3 (%+v)", len(panelRecs), records)
	}
	seenProv := map[string]bool{}
	for _, r := range panelRecs {
		seenProv[r.Provider] = true
		if r.Status != 200 {
			t.Errorf("panel record %s status = %d, want 200", r.RequestID, r.Status)
		}
	}
	for _, p := range []string{"pa", "pb", "pc"} {
		if !seenProv[p] {
			t.Errorf("no panel record for provider %s", p)
		}
	}
	if len(synthRecs) != 1 {
		t.Fatalf("synthesizer records = %d, want 1", len(synthRecs))
	}
	// NB: the logged body is JSON text — '<' may appear as its < escape,
	// so match on the marker text without the angle bracket.
	if !strings.Contains(synthRecs[0].RequestBody, "CANDIDATE 1") {
		t.Errorf("synthesizer record body missing candidates: %q", synthRecs[0].RequestBody)
	}
}

// TestFusion_PartialFailure (plan #2): with min_panel=2, one failed leg still
// synthesizes from the surviving drafts; two failed legs make the quorum
// unreachable → direct answer from the synthesizer with the ORIGINAL body.
func TestFusion_PartialFailure(t *testing.T) {
	t.Run("one failure still synthesizes", func(t *testing.T) {
		pa := newFakeUpstream(t, anthropicDraftResponder("draft-A"))
		pb := newFakeUpstream(t, statusResponder(500))
		pc := newFakeUpstream(t, anthropicDraftResponder("draft-C"))
		ps := newFakeUpstream(t, anthropicSSEResponder("final answer"))
		recipe := FusionConfig{
			Panel: []RouteTarget{
				{Provider: "pa", Model: "ma"},
				{Provider: "pb", Model: "mb"},
				{Provider: "pc", Model: "mc"},
			},
			Synthesizer: RouteTarget{Provider: "ps", Model: "ms"},
		}
		_, px := newFusionRig(t, recipe, map[string]*fakeUpstream{"pa": pa, "pb": pb, "pc": pc, "ps": ps})
		out := postAnthropic(t, px, fusionClientBody)
		if !strings.Contains(out, "final answer") {
			t.Fatalf("client body missing answer: %s", out)
		}
		var synth struct {
			System string `json:"system"`
		}
		if err := json.Unmarshal([]byte(ps.lastBody()), &synth); err != nil {
			t.Fatalf("synthesis body not JSON: %v", err)
		}
		if n := strings.Count(synth.System, "<CANDIDATE "); n != 2 {
			t.Errorf("synthesis has %d candidates, want 2", n)
		}
		for _, d := range []string{"draft-A", "draft-C"} {
			if !strings.Contains(synth.System, d) {
				t.Errorf("synthesis missing surviving candidate %q", d)
			}
		}
	})
	t.Run("quorum unreachable falls back to direct", func(t *testing.T) {
		pa := newFakeUpstream(t, anthropicDraftResponder("draft-A"))
		pb := newFakeUpstream(t, statusResponder(500))
		pc := newFakeUpstream(t, statusResponder(500))
		ps := newFakeUpstream(t, anthropicSSEResponder("direct answer"))
		recipe := FusionConfig{
			Panel: []RouteTarget{
				{Provider: "pa", Model: "ma"},
				{Provider: "pb", Model: "mb"},
				{Provider: "pc", Model: "mc"},
			},
			Synthesizer: RouteTarget{Provider: "ps", Model: "ms"},
		}
		_, px := newFusionRig(t, recipe, map[string]*fakeUpstream{"pa": pa, "pb": pb, "pc": pc, "ps": ps})
		out := postAnthropic(t, px, fusionClientBody)
		if !strings.Contains(out, "direct answer") {
			t.Fatalf("client body missing direct answer: %s", out)
		}
		// Fallback sends the ORIGINAL body: no candidate section anywhere.
		if strings.Contains(ps.lastBody(), "CANDIDATE") {
			t.Errorf("fallback synthesis body carries candidates: %s", ps.lastBody())
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(ps.lastBody()), &body); err != nil {
			t.Fatalf("fallback body not JSON: %v", err)
		}
		if body["model"] != "ms" {
			t.Errorf("fallback model = %v, want ms", body["model"])
		}
	})
}

// TestFusion_ToolRound (plan #3): a request with tools is still orchestrated —
// draft legs strip tools/tool_choice, the synthesizer keeps them, and its
// tool_use answer passes through to the client intact.
func TestFusion_ToolRound(t *testing.T) {
	pa := newFakeUpstream(t, anthropicDraftResponder("draft-A"))
	pb := newFakeUpstream(t, anthropicDraftResponder("draft-B"))
	ps := newFakeUpstream(t, anthropicToolUseSSEResponder())
	recipe := FusionConfig{
		Panel:       []RouteTarget{{Provider: "pa", Model: "ma"}, {Provider: "pb", Model: "mb"}},
		Synthesizer: RouteTarget{Provider: "ps", Model: "ms"},
	}
	_, px := newFusionRig(t, recipe, map[string]*fakeUpstream{"pa": pa, "pb": pb, "ps": ps})
	body := `{"model":"hard","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"weather in Paris?"}],` +
		`"tools":[{"name":"get_weather","description":"w","input_schema":{"type":"object"}}],"tool_choice":{"type":"auto"}}`
	out := postAnthropic(t, px, body)
	if !strings.Contains(out, `"type":"tool_use"`) || !strings.Contains(out, "get_weather") {
		t.Fatalf("client body missing tool_use passthrough: %s", out)
	}
	if !strings.Contains(out, "partial_json") {
		t.Errorf("client body missing tool input delta: %s", out)
	}
	// Draft legs: tools and tool_choice are gone.
	for name, up := range map[string]*fakeUpstream{"pa": pa, "pb": pb} {
		var b map[string]any
		if err := json.Unmarshal([]byte(up.lastBody()), &b); err != nil {
			t.Fatalf("member %s body not JSON: %v", name, err)
		}
		if _, ok := b["tools"]; ok {
			t.Errorf("member %s draft body still has tools", name)
		}
		if _, ok := b["tool_choice"]; ok {
			t.Errorf("member %s draft body still has tool_choice", name)
		}
	}
	// Synthesizer leg: tools preserved.
	var sb map[string]any
	if err := json.Unmarshal([]byte(ps.lastBody()), &sb); err != nil {
		t.Fatalf("synthesis body not JSON: %v", err)
	}
	tools, ok := sb["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Errorf("synthesis body tools = %v, want 1 tool", sb["tools"])
	}
}

// TestFusion_QuorumGrace (plan #3b): after the quorum is met the engine waits a
// grace window for nearly-done stragglers — one inside the window joins the
// synthesis; one outside is cancelled and excluded.
func TestFusion_QuorumGrace(t *testing.T) {
	old := fusionGracePeriod
	fusionGracePeriod = 200 * time.Millisecond
	defer func() { fusionGracePeriod = old }()

	build := func(t *testing.T, stragglerDelay time.Duration) (*Proxy, *fakeUpstream, *httptest.Server) {
		pa := newFakeUpstream(t, anthropicDraftResponder("draft-A"))
		pb := newFakeUpstream(t, anthropicDraftResponder("draft-B"))
		pc := newFakeUpstream(t, delayedResponder(stragglerDelay, anthropicDraftResponder("draft-C")))
		ps := newFakeUpstream(t, anthropicSSEResponder("final answer"))
		recipe := FusionConfig{
			Panel: []RouteTarget{
				{Provider: "pa", Model: "ma"},
				{Provider: "pb", Model: "mb"},
				{Provider: "pc", Model: "mc"},
			},
			Synthesizer: RouteTarget{Provider: "ps", Model: "ms"},
		}
		proxy, px := newFusionRig(t, recipe, map[string]*fakeUpstream{"pa": pa, "pb": pb, "pc": pc, "ps": ps})
		return proxy, ps, px
	}
	synthSystem := func(t *testing.T, ps *fakeUpstream) string {
		var synth struct {
			System string `json:"system"`
		}
		if err := json.Unmarshal([]byte(ps.lastBody()), &synth); err != nil {
			t.Fatalf("synthesis body not JSON: %v", err)
		}
		return synth.System
	}

	t.Run("straggler within grace joins", func(t *testing.T) {
		_, ps, px := build(t, 60*time.Millisecond)
		postAnthropic(t, px, fusionClientBody)
		if sys := synthSystem(t, ps); !strings.Contains(sys, "draft-C") {
			t.Errorf("straggler inside grace missing from synthesis: %q", sys)
		}
	})
	t.Run("straggler beyond grace dropped", func(t *testing.T) {
		proxy, ps, px := build(t, 600*time.Millisecond)
		postAnthropic(t, px, fusionClientBody)
		sys := synthSystem(t, ps)
		if strings.Contains(sys, "draft-C") {
			t.Errorf("straggler beyond grace leaked into synthesis: %q", sys)
		}
		if strings.Contains(sys, "draft-A") != true || !strings.Contains(sys, "draft-B") {
			t.Errorf("fast candidates missing from synthesis: %q", sys)
		}
		// The cancelled straggler was CUT, not failed — the circuit must not
		// count it as a provider failure.
		proxy.healthMu.Lock()
		failures := 0
		if h := proxy.health["pc"]; h != nil {
			failures = h.consecutiveFailures
		}
		proxy.healthMu.Unlock()
		if failures != 0 {
			t.Errorf("cancelled straggler consecutiveFailures = %d, want 0", failures)
		}
	})
}

// TestFusion_CrossProtocolPanel (plan #4): an anthropic client request fans out
// to an openai-protocol member — the member receives a converted openai body
// (system folded into messages), and its openai-shaped draft + usage still feed
// the synthesis and the counters.
func TestFusion_CrossProtocolPanel(t *testing.T) {
	pa := newFakeUpstream(t, anthropicDraftResponder("draft-A"))
	pb := newFakeUpstream(t, openaiDraftResponder("draft-B"))
	ps := newFakeUpstream(t, anthropicSSEResponder("final answer"))
	recipe := FusionConfig{
		Panel: []RouteTarget{
			{Provider: "pa", Model: "ma"},
			{Provider: "pb", Model: "mb", Protocol: "openai"},
		},
		Synthesizer: RouteTarget{Provider: "ps", Model: "ms"},
	}
	proxy, px := newFusionRig(t, recipe, map[string]*fakeUpstream{"pa": pa, "pb": pb, "ps": ps})
	body := `{"model":"hard","max_tokens":100,"stream":true,"system":"be brief","messages":[{"role":"user","content":"solve X"}]}`
	out := postAnthropic(t, px, body)
	if !strings.Contains(out, "final answer") {
		t.Fatalf("client body missing answer: %s", out)
	}
	// The openai member got a converted chat/completions request.
	if pb.lastPath() != "/chat/completions" {
		t.Errorf("openai member path = %q, want /chat/completions", pb.lastPath())
	}
	var ob map[string]any
	if err := json.Unmarshal([]byte(pb.lastBody()), &ob); err != nil {
		t.Fatalf("openai member body not JSON: %v", err)
	}
	if ob["model"] != "mb" {
		t.Errorf("openai member model = %v, want mb", ob["model"])
	}
	if ob["stream"] != false {
		t.Errorf("openai member stream = %v, want false", ob["stream"])
	}
	msgs, ok := ob["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatalf("openai member messages missing: %v", ob["messages"])
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "be brief" {
		t.Errorf("openai member first message = %v, want folded system prompt", first)
	}
	// Both drafts (anthropic + openai member) reached the synthesis.
	var synth struct {
		System string `json:"system"`
	}
	if err := json.Unmarshal([]byte(ps.lastBody()), &synth); err != nil {
		t.Fatalf("synthesis body not JSON: %v", err)
	}
	for _, d := range []string{"draft-A", "draft-B"} {
		if !strings.Contains(synth.System, d) {
			t.Errorf("synthesis missing %q: %q", d, synth.System)
		}
	}
	// Openai-shaped usage was parsed into the counter.
	toks := proxy.tokens.snapshot()
	if u := toks[tokenKey{Provider: "pb", Model: "mb"}]; u.Input != 21 || u.Output != 9 {
		t.Errorf("openai member usage = %+v, want {21 9}", u)
	}
}

// TestFusion_CircuitRecordFailure (plan #5): a member's 5xx feeds the standard
// circuit breaker (recordFailure), while surviving members still synthesize.
func TestFusion_CircuitRecordFailure(t *testing.T) {
	pa := newFakeUpstream(t, anthropicDraftResponder("draft-A"))
	pb := newFakeUpstream(t, statusResponder(500))
	pc := newFakeUpstream(t, anthropicDraftResponder("draft-C"))
	ps := newFakeUpstream(t, anthropicSSEResponder("final answer"))
	recipe := FusionConfig{
		Panel: []RouteTarget{
			{Provider: "pa", Model: "ma"},
			{Provider: "pb", Model: "mb"},
			{Provider: "pc", Model: "mc"},
		},
		Synthesizer: RouteTarget{Provider: "ps", Model: "ms"},
	}
	proxy, px := newFusionRig(t, recipe, map[string]*fakeUpstream{"pa": pa, "pb": pb, "pc": pc, "ps": ps})
	out := postAnthropic(t, px, fusionClientBody)
	if !strings.Contains(out, "final answer") {
		t.Fatalf("client body missing answer: %s", out)
	}
	proxy.healthMu.Lock()
	h := proxy.health["pb"]
	failures := 0
	if h != nil {
		failures = h.consecutiveFailures
	}
	proxy.healthMu.Unlock()
	if failures != 1 {
		t.Errorf("failed member consecutiveFailures = %d, want 1", failures)
	}
	if got := proxy.metrics.snapshot()[pmKey{Provider: "pb", Model: "mb"}].Failures; got != 1 {
		t.Errorf("failed member metrics failures = %d, want 1", got)
	}
}

func TestFusionLegSharesTargetPolicies(t *testing.T) {
	t.Run("unsupported parameter is learned and retried", func(t *testing.T) {
		var mu sync.Mutex
		hits := 0
		pa := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hits++
			n := hits
			mu.Unlock()
			if n == 1 {
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, `{"error":{"message":"Unsupported parameter: temperature"}}`)
				return
			}
			anthropicDraftResponder("draft-A")(w, r)
		})
		pb := newFakeUpstream(t, anthropicDraftResponder("draft-B"))
		ps := newFakeUpstream(t, anthropicSSEResponder("final"))
		recipe := FusionConfig{
			Panel:       []RouteTarget{{Provider: "pa", Model: "ma"}, {Provider: "pb", Model: "mb"}},
			Synthesizer: RouteTarget{Provider: "ps", Model: "ms"},
		}
		proxy, px := newFusionRig(t, recipe, map[string]*fakeUpstream{"pa": pa, "pb": pb, "ps": ps})
		body := `{"model":"hard","max_tokens":100,"temperature":0.2,"stream":true,"messages":[{"role":"user","content":"solve X"}]}`
		if out := postAnthropic(t, px, body); !strings.Contains(out, "final") {
			t.Fatalf("client body missing synthesis: %s", out)
		}
		if pa.hits() != 2 {
			t.Fatalf("unsupported-parameter panel hits = %d, want 2", pa.hits())
		}
		var retry map[string]any
		if err := json.Unmarshal([]byte(pa.lastBody()), &retry); err != nil {
			t.Fatal(err)
		}
		if _, exists := retry["temperature"]; exists {
			t.Errorf("retry still carries learned temperature: %s", pa.lastBody())
		}
		proxy.healthMu.Lock()
		learned := proxy.paramBlock[modelLockKey{provider: "pa", model: "ma"}]["temperature"]
		proxy.healthMu.Unlock()
		if !learned {
			t.Error("fusion leg did not persist unsupported parameter")
		}
	})

	for _, tc := range []struct {
		name      string
		responder http.HandlerFunc
	}{
		{"model denied", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusForbidden)
			io.WriteString(w, `{"error":{"message":"You do not have access to model ma"}}`)
		}},
		{"empty success", statusResponder(http.StatusOK)},
	} {
		t.Run(tc.name+" locks only the model", func(t *testing.T) {
			pa := newFakeUpstream(t, tc.responder)
			pb := newFakeUpstream(t, anthropicDraftResponder("draft-B"))
			pc := newFakeUpstream(t, anthropicDraftResponder("draft-C"))
			ps := newFakeUpstream(t, anthropicSSEResponder("final"))
			recipe := FusionConfig{
				Panel: []RouteTarget{
					{Provider: "pa", Model: "ma"},
					{Provider: "pb", Model: "mb"},
					{Provider: "pc", Model: "mc"},
				},
				Synthesizer: RouteTarget{Provider: "ps", Model: "ms"},
				MinPanel:    2,
			}
			proxy, px := newFusionRig(t, recipe, map[string]*fakeUpstream{
				"pa": pa, "pb": pb, "pc": pc, "ps": ps,
			})
			if out := postAnthropic(t, px, fusionClientBody); !strings.Contains(out, "final") {
				t.Fatalf("client body missing synthesis: %s", out)
			}
			if !proxy.modelLocked("pa", "ma", time.Now()) {
				t.Error("fusion leg failure did not lock (pa, ma)")
			}
			if proxy.modelLocked("pa", "other", time.Now()) {
				t.Error("fusion leg failure poisoned another model")
			}
		})
	}
}

// TestParseUsageJSON (plan #6): non-streaming usage parsing covers the
// anthropic shape (incl. cache fields) and the openai shape.
func TestParseUsageJSON(t *testing.T) {
	anth := parseUsageJSON([]byte(`{"usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":3,"cache_read_input_tokens":2}}`))
	if anth.Input != 10 || anth.Output != 5 || anth.CacheCreation != 3 || anth.CacheRead != 2 {
		t.Errorf("anthropic usage = %+v, want {10 5 3 2}", anth)
	}
	oai := parseUsageJSON([]byte(`{"usage":{"prompt_tokens":20,"completion_tokens":7}}`))
	if oai.Input != 20 || oai.Output != 7 {
		t.Errorf("openai usage = %+v, want {20 7}", oai)
	}
	if u := parseUsageJSON([]byte(`not json`)); u != (tokenUsage{}) {
		t.Errorf("garbage usage = %+v, want zero", u)
	}
}

// TestFusion_SynthesizerPoolExhaustedFailsClosed (P0-1): when the synthesizer's
// pooled parent has NO healthy account, the synthesizer must fail CLOSED —
// fusion returns false, the route's next target serves, and the synthesizer's
// upstream sees ZERO requests (previously the unresolved pooled-parent name
// fell through with a nil impl and shipped an UNAUTHENTICATED request).
func TestFusion_SynthesizerPoolExhaustedFailsClosed(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	writePoolFile(t, "zhipu-draft", "zhipu", "ZA", "ZB")
	writePoolFile(t, "zhipu-synth", "zhipu", "GA", "GB")

	draftUp := newFakeUpstream(t, anthropicDraftResponder("draft-ok"))
	synthUp := newFakeUpstream(t, anthropicSSEResponder("should never be sent"))
	directUp := newFakeUpstream(t, anthropicSSEResponder("direct fallback ok"))

	cfg := &Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]Provider{
			"zhipu-draft": {AnthropicBaseURL: draftUp.srv.URL, Provider: "zhipu"},
			"zhipu-synth": {AnthropicBaseURL: synthUp.srv.URL, Provider: "zhipu"},
			"direct":      {AnthropicBaseURL: directUp.srv.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{"hard": {
			{Provider: "fusion", Model: "recipe", Priority: 1},
			{Provider: "direct", Model: "dfull", Priority: 2},
		}},
		Fusion: map[string]FusionConfig{"recipe": {
			Panel:       []RouteTarget{{Provider: "zhipu-draft", Model: "zdraft"}},
			Synthesizer: RouteTarget{Provider: "zhipu-synth", Model: "gsynth"},
		}},
	}
	p := newTestProxy(t, cfg)
	// Rate-limit BOTH synthesizer accounts → the resolver finds no healthy virtual.
	for _, vid := range p.poolIndex["zhipu-synth"] {
		p.recordRateLimit(vid, time.Now().Add(time.Hour), rlTransient)
	}
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	out := postAnthropic(t, px, fusionClientBody)
	if !strings.Contains(out, "direct fallback ok") {
		t.Fatalf("expected route failover to the direct target, got: %s", out)
	}
	if synthUp.hits() != 0 {
		t.Errorf("synthesizer upstream hit %d times — a bare unauthenticated request leaked", synthUp.hits())
	}
	if draftUp.hits() == 0 {
		t.Error("panel member should still have been tried")
	}
}

// TestFusionLeg_WireVerdict404Correction (review fix): a panel leg converted
// to /responses by the wire verdict that comes back 404 must flip the verdict
// (noteWireResponsesMiss) and must NOT lock the model — the verdict was wrong,
// not the model. The NEXT request's leg then goes out as chat. Pre-fix the leg
// recorded a model failure and the verdict never flipped.
func TestFusionLeg_WireVerdict404Correction(t *testing.T) {
	pa := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/responses" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		openaiDraftResponder("draft-A")(w, r)
	})
	pb := newFakeUpstream(t, anthropicDraftResponder("draft-B"))
	ps := newFakeUpstream(t, anthropicSSEResponder("final"))
	cfg := &Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]Provider{
			// pa has ONLY an openai base: with no declared protocol and a
			// responses=yes verdict the leg is verdict-driven to /responses.
			"pa": {OpenAIBaseURL: pa.srv.URL, Provider: testProviderID},
			"pb": {AnthropicBaseURL: pb.srv.URL, Provider: testProviderID},
			"ps": {AnthropicBaseURL: ps.srv.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{"hard": {{Provider: "fusion", Model: "recipe"}}},
		Fusion: map[string]FusionConfig{"recipe": {
			Panel:       []RouteTarget{{Provider: "pa", Model: "ma"}, {Provider: "pb", Model: "mb"}},
			Synthesizer: RouteTarget{Provider: "ps", Model: "ms"},
			MinPanel:    1,
		}},
	}
	proxy := newTestProxy(t, cfg)
	for _, name := range []string{"pa", "pb", "ps"} {
		proxy.providers[name] = &testProv{key: name}
	}
	proxy.setWireCaps("pa", wireCaps{BaseURL: pa.srv.URL, Responses: triYes, Anthropic: triUnknown, ProbedAt: time.Now()})
	px := httptest.NewServer(http.HandlerFunc(proxy.handler))
	defer px.Close()

	// Request 1: the pa leg is verdict-driven to /responses and 404s.
	if out := postAnthropic(t, px, fusionClientBody); !strings.Contains(out, "final") {
		t.Fatalf("client body missing synthesis: %s", out)
	}
	if pa.lastPath() != "/responses" {
		t.Fatalf("pa leg path = %q, want verdict-driven /responses", pa.lastPath())
	}
	caps, _ := proxy.wireVerdict("pa")
	if caps.Responses != triNo {
		t.Errorf("post-404 verdict responses = %s, want no (flipped)", caps.Responses)
	}
	if proxy.modelLocked("pa", "ma", time.Now()) {
		t.Error("(pa, ma) model-locked after a verdict-miss 404 — the verdict was wrong, not the model")
	}
	if m := proxy.metrics.snapshot()[pmKey{Provider: "pa", Model: "ma"}]; m.Failures != 0 || m.Failovers != 1 {
		t.Errorf("pa metrics = failures %d failovers %d, want 0/1 (leg abandoned like tryTarget's failover)", m.Failures, m.Failovers)
	}

	// Request 2: the flipped verdict sends the pa leg to chat — its draft now
	// feeds the synthesis, and the model is STILL not locked.
	if out := postAnthropic(t, px, fusionClientBody); !strings.Contains(out, "final") {
		t.Fatalf("request 2 client body missing synthesis: %s", out)
	}
	if pa.lastPath() != "/chat/completions" {
		t.Errorf("request 2 pa leg path = %q, want /chat/completions after verdict flip", pa.lastPath())
	}
	var synth struct {
		System string `json:"system"`
	}
	if err := json.Unmarshal([]byte(ps.lastBody()), &synth); err != nil {
		t.Fatalf("synthesis body not JSON: %v", err)
	}
	if !strings.Contains(synth.System, "draft-A") {
		t.Errorf("request 2 synthesis missing pa's chat draft: %q", synth.System)
	}
	if proxy.modelLocked("pa", "ma", time.Now()) {
		t.Error("(pa, ma) model-locked after the recovered chat leg")
	}
}

// TestFusionLeg_NativeResponsesBackendDraft (review fix E4): a panel leg whose
// backend speaks the native responses protocol (target declares
// protocol:responses) gets a responses-shaped JSON back. extractCandidateText
// must parse THAT shape — pre-fix it only understood chat/anthropic, extracted
// "", and the leg was misjudged as an empty draft and model-locked forever.
func TestFusionLeg_NativeResponsesBackendDraft(t *testing.T) {
	pa := newFakeUpstream(t, responsesDraftResponder("draft-R"))
	pb := newFakeUpstream(t, anthropicDraftResponder("draft-B"))
	ps := newFakeUpstream(t, anthropicSSEResponder("final"))
	recipe := FusionConfig{
		Panel: []RouteTarget{
			{Provider: "pa", Model: "ma", Protocol: "responses"},
			{Provider: "pb", Model: "mb"},
		},
		Synthesizer: RouteTarget{Provider: "ps", Model: "ms"},
		MinPanel:    1,
	}
	proxy, px := newFusionRig(t, recipe, map[string]*fakeUpstream{"pa": pa, "pb": pb, "ps": ps})

	if out := postAnthropic(t, px, fusionClientBody); !strings.Contains(out, "final") {
		t.Fatalf("client body missing synthesis: %s", out)
	}
	if pa.lastPath() != "/responses" {
		t.Fatalf("pa leg path = %q, want native /responses", pa.lastPath())
	}
	// The responses-shaped draft fed the synthesis (not discarded as empty).
	var synth struct {
		System string `json:"system"`
	}
	if err := json.Unmarshal([]byte(ps.lastBody()), &synth); err != nil {
		t.Fatalf("synthesis body not JSON: %v", err)
	}
	if !strings.Contains(synth.System, "draft-R") {
		t.Errorf("synthesis missing pa's responses draft: %q", synth.System)
	}
	if proxy.modelLocked("pa", "ma", time.Now()) {
		t.Error("(pa, ma) model-locked — a good responses draft was misjudged as empty")
	}
	if m := proxy.metrics.snapshot()[pmKey{Provider: "pa", Model: "ma"}]; m.Failovers != 0 || m.Requests != 1 {
		t.Errorf("pa metrics = requests %d failovers %d, want 1/0 (successful leg)", m.Requests, m.Failovers)
	}
}

// TestExtractCandidateText pins the per-protocol draft extraction: anthropic
// content[] and chat choices[] parse under their own backend protocol, and the
// responses output[] shape parses only when the leg's backend is responses.
func TestExtractCandidateText(t *testing.T) {
	cases := []struct {
		name         string
		backendProto string
		body         string
		want         string
	}{
		{"anthropic", "anthropic", `{"content":[{"type":"text","text":"A"}]}`, "A"},
		{"chat", "openai", `{"choices":[{"message":{"content":"B"}}]}`, "B"},
		{"responses", "responses", `{"output":[{"type":"reasoning"},{"type":"message","content":[{"type":"output_text","text":"C"}]}]}`, "C"},
		{"responses multi-part", "responses", `{"output":[{"type":"message","content":[{"type":"output_text","text":"C1"},{"type":"output_text","text":"C2"}]}]}`, "C1C2"},
		{"responses shape under chat proto stays empty", "openai", `{"output":[{"type":"message","content":[{"type":"output_text","text":"C"}]}]}`, ""},
		{"garbage", "responses", `not json`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractCandidateText([]byte(c.body), c.backendProto); got != c.want {
				t.Errorf("extractCandidateText(%s) = %q, want %q", c.backendProto, got, c.want)
			}
		})
	}
}

// TestFusionLeg_FailureMetricsAlignTryTarget (review fix): abandoned fusion
// legs record the same metrics shape as tryTarget — a connection error and a
// 5xx count Failures+Failovers, a 401-after-refresh counts Failovers only.
func TestFusionLeg_FailureMetricsAlignTryTarget(t *testing.T) {
	build := func(t *testing.T, pa *fakeUpstream) (*Proxy, *httptest.Server) {
		pb := newFakeUpstream(t, anthropicDraftResponder("draft-B"))
		ps := newFakeUpstream(t, anthropicSSEResponder("final"))
		recipe := FusionConfig{
			Panel:       []RouteTarget{{Provider: "pa", Model: "ma"}, {Provider: "pb", Model: "mb"}},
			Synthesizer: RouteTarget{Provider: "ps", Model: "ms"},
			MinPanel:    1,
		}
		return newFusionRig(t, recipe, map[string]*fakeUpstream{"pa": pa, "pb": pb, "ps": ps})
	}

	t.Run("connection error counts failures and failovers", func(t *testing.T) {
		dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		dead.Close() // connection refused — no cleanup needed
		proxy, px := build(t, &fakeUpstream{srv: dead})
		if out := postAnthropic(t, px, fusionClientBody); !strings.Contains(out, "final") {
			t.Fatalf("client body missing synthesis: %s", out)
		}
		m := proxy.metrics.snapshot()[pmKey{Provider: "pa", Model: "ma"}]
		if m.Failures != 1 || m.Failovers != 1 {
			t.Errorf("conn-error leg metrics = failures %d failovers %d, want 1/1", m.Failures, m.Failovers)
		}
	})

	t.Run("401 after refresh counts failovers only", func(t *testing.T) {
		proxy, px := build(t, newFakeUpstream(t, statusResponder(http.StatusUnauthorized)))
		if out := postAnthropic(t, px, fusionClientBody); !strings.Contains(out, "final") {
			t.Fatalf("client body missing synthesis: %s", out)
		}
		m := proxy.metrics.snapshot()[pmKey{Provider: "pa", Model: "ma"}]
		if m.Failures != 0 || m.Failovers != 1 {
			t.Errorf("401 leg metrics = failures %d failovers %d, want 0/1", m.Failures, m.Failovers)
		}
		// …but the circuit still sees the failure (same as tryTarget).
		proxy.healthMu.Lock()
		h := proxy.health["pa"]
		failures := 0
		if h != nil {
			failures = h.consecutiveFailures
		}
		proxy.healthMu.Unlock()
		if failures != 1 {
			t.Errorf("401 leg consecutiveFailures = %d, want 1", failures)
		}
	})

	t.Run("5xx counts failures and failovers", func(t *testing.T) {
		proxy, px := build(t, newFakeUpstream(t, statusResponder(http.StatusInternalServerError)))
		if out := postAnthropic(t, px, fusionClientBody); !strings.Contains(out, "final") {
			t.Fatalf("client body missing synthesis: %s", out)
		}
		m := proxy.metrics.snapshot()[pmKey{Provider: "pa", Model: "ma"}]
		if m.Failures != 1 || m.Failovers != 1 {
			t.Errorf("5xx leg metrics = failures %d failovers %d, want 1/1", m.Failures, m.Failovers)
		}
	})
}

// TestFusion_ResponsesChainRestored (review fix): a responses-protocol client
// on a fusion route with stateless (chat) backends gets the same
// previous_response_id handling as forward — turn 1's final synthesizer answer
// is recorded, and turn 2's legs + synthesizer receive the EXPANDED history
// (no previous_response_id leaked upstream, no broken chain).
func TestFusion_ResponsesChainRestored(t *testing.T) {
	pa := newFakeUpstream(t, openaiDraftResponder("draft-A"))
	pb := newFakeUpstream(t, openaiDraftResponder("draft-B"))
	ps := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"id":"chat_s1","choices":[{"index":0,"message":{"role":"assistant","content":"a1"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`)
	})
	recipe := FusionConfig{
		Panel: []RouteTarget{
			{Provider: "pa", Model: "ma", Protocol: "openai"},
			{Provider: "pb", Model: "mb", Protocol: "openai"},
		},
		Synthesizer: RouteTarget{Provider: "ps", Model: "ms", Protocol: "openai"},
	}
	_, px := newFusionRig(t, recipe, map[string]*fakeUpstream{"pa": pa, "pb": pb, "ps": ps})

	post := func(body string) []byte {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/responses", strings.NewReader(body))
		req.Header.Set("content-type", "application/json")
		req.Header.Set("x-claude-code-session-id", "sess")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		out, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != 200 {
			t.Fatalf("status=%d body=%s", resp.StatusCode, out)
		}
		return out
	}

	// Turn 1: the synthesizer's converted responses answer carries id chat_s1,
	// which the executor records (history = input + answer output).
	first := post(`{"model":"hard","input":"q1"}`)
	var firstResp map[string]any
	if err := json.Unmarshal(first, &firstResp); err != nil || firstResp["id"] != "chat_s1" {
		t.Fatalf("turn 1 response = %s", first)
	}

	// Turn 2: chained on chat_s1. Every upstream body must carry the restored
	// history (q1, a1, q2) and must NOT leak previous_response_id.
	second := post(`{"model":"hard","previous_response_id":"chat_s1","input":"q2"}`)
	if !strings.Contains(string(second), "a1") {
		t.Fatalf("turn 2 response = %s", second)
	}
	assertRestored := func(name, body string, wantCandidates bool) {
		t.Helper()
		if strings.Contains(body, "previous_response_id") {
			t.Errorf("%s body leaks previous_response_id: %s", name, body)
		}
		var req map[string]any
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			t.Fatalf("%s body not JSON: %v", name, err)
		}
		msgs, _ := req["messages"].([]any)
		if len(msgs) < 3 {
			t.Fatalf("%s chat messages = %d, want ≥3 (q1, a1, q2): %s", name, len(msgs), body)
		}
		if asMap(msgs[0])["content"] != "q1" {
			t.Errorf("%s first message = %v, want user q1", name, msgs[0])
		}
		if asMap(msgs[1])["role"] != "assistant" || asMap(msgs[1])["content"] != "a1" {
			t.Errorf("%s second message = %v, want assistant a1 (restored from the chain)", name, msgs[1])
		}
		if wantCandidates && !strings.Contains(body, "CANDIDATE 1") {
			t.Errorf("%s body missing injected candidate section: %s", name, body)
		}
	}
	assertRestored("panel pa", pa.lastBody(), false)
	assertRestored("panel pb", pb.lastBody(), false)
	assertRestored("synthesizer", ps.lastBody(), true)
}
