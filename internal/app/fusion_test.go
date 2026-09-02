package app

import (
	"encoding/json"
	"fmt"
	"io"
	"model-proxy/internal/fusion"
	"model-proxy/internal/observe/counters"
	observeevents "model-proxy/internal/observe/events"
	"model-proxy/internal/observe/requestlog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- fusion_orchestration_test.go ----

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
	// land in the token counter. The synthesizer's commit metrics land in the
	// post-copy Committed effect — wait for them before snapshotting (a
	// Content-Length client can finish reading first).
	metrics := awaitCommitMetrics(t, proxy, counters.PMKey{Provider: "ps", Model: "ms"})
	for _, k := range []counters.PMKey{{Provider: "pa", Model: "ma"}, {Provider: "pb", Model: "mb"}, {Provider: "pc", Model: "mc"}, {Provider: "ps", Model: "ms"}} {
		if metrics[k].Requests != 1 {
			t.Errorf("metrics %v requests = %d, want 1", k, metrics[k].Requests)
		}
	}
	toks := proxy.tokens.Snapshot()
	if u := toks[counters.TokenKey{Provider: "pa", Model: "ma"}]; u.Input != 11 || u.Output != 7 {
		t.Errorf("member pa usage = %+v, want {11 7}", u)
	}
	if u := toks[counters.TokenKey{Provider: "ps", Model: "ms"}]; u.Input != 50 || u.Output != 9 {
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
		failures := 0
		if status, ok := proxy.runtimeState.Dashboard(time.Now()).Providers["pc"]; ok {
			failures = status.ConsecutiveFailures
		}
		if failures != 0 {
			t.Errorf("cancelled straggler consecutiveFailures = %d, want 0", failures)
		}
	})
}

// TestFusion_SynthesizerUpstream5xxIsHardEndpoint pins the fusion failure
// contract ("a failed synthesis leg is a hard endpoint"): the panel reaches
// quorum, but a 5xx synthesizer upstream does NOT retry elsewhere or fall back
// to a draft — the client receives the synthesizer's failure as the terminal
// answer, every panel member was still drafted exactly once, and the run is
// recorded as committed-with-synth-failure rather than vanishing.
func TestFusion_SynthesizerUpstream5xxIsHardEndpoint(t *testing.T) {
	pa := newFakeUpstream(t, anthropicDraftResponder("draft-A"))
	pb := newFakeUpstream(t, anthropicDraftResponder("draft-B"))
	ps := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(500)
		io.WriteString(w, `{"e":"synth down"}`)
	})
	recipe := FusionConfig{
		Panel: []RouteTarget{
			{Provider: "pa", Model: "ma"},
			{Provider: "pb", Model: "mb"},
		},
		Synthesizer: RouteTarget{Provider: "ps", Model: "ms"},
	}
	proxy, px := newFusionRig(t, recipe, map[string]*fakeUpstream{"pa": pa, "pb": pb, "ps": ps})

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(px.URL+"/v1/messages", "application/json", strings.NewReader(fusionClientBody))
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read response: %v", readErr)
	}
	// The synthesizer's 5xx is a hard failure of the fusion route's single
	// remaining target: no draft fallback, no verbatim 500 commit — the proxy
	// answers with its all-targets-failed 502. That IS the hard-endpoint
	// contract; falling back to a draft here would be the bug.
	if resp.StatusCode != 502 {
		t.Fatalf("client status = %d body=%s, want 502 (hard endpoint, no draft fallback)", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "all targets failed") {
		t.Errorf("client body = %s, want the all-targets-failed error", body)
	}

	// Drafts still ran exactly once per member; the synthesizer was tried
	// exactly once (no retry storm around the hard endpoint).
	if pa.hits() != 1 || pb.hits() != 1 {
		t.Errorf("panel hits = pa:%d pb:%d, want 1/1 (quorum drafted before the synth failure)", pa.hits(), pb.hits())
	}
	if ps.hits() != 1 {
		t.Errorf("synthesizer hits = %d, want exactly 1", ps.hits())
	}

	// Terminal state is observable: a 502 end event closes the request.
	saw502 := false
	for _, e := range proxy.events.Snapshot() {
		if e.Type == "end" && e.Status == 502 {
			saw502 = true
		}
	}
	if !saw502 {
		t.Error("no 502 end event closed the failed fusion request")
	}
}

// ---- fusion_test_helpers_test.go ----

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
	px := httptest.NewServer(http.HandlerFunc(proxy.Handler))
	t.Cleanup(px.Close)
	return proxy, px
}

// postAnthropic posts to the proxy's anthropic endpoint and returns the full
// response body. Streaming reads must fail fast (client timeout) and surface
// read errors instead of silently returning a truncated prefix.
func postAnthropic(t *testing.T, px *httptest.Server, body string) string {
	t.Helper()
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(px.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	b, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil {
		t.Fatalf("postAnthropic: read response: %v", readErr)
	}
	if closeErr != nil {
		t.Fatalf("postAnthropic: close response: %v", closeErr)
	}
	return string(b)
}

func recentLiveEvents(p *Proxy) []observeevents.Event {
	return p.events.Snapshot()
}

const fusionClientBody = `{"model":"hard","max_tokens":100,"stream":true,"messages":[{"role":"user","content":"solve X"}]}`

// ---- fusion_obs_test.go ----

// fusion_obs_test.go covers the application integration around
// internal/fusion: /api/fusion, cost gates, judge behavior, instruction
// override, and doctor output. Pure registry/engine tests live with the package.

// --- /api/fusion ---

// apiFusionGet queries /api/fusion on a rig proxy and decodes the response.
func apiFusionGet(t *testing.T, proxy *Proxy, query string) (struct {
	Workflows map[string]fusion.WorkflowStats `json:"workflows"`
	Runs      []fusion.Run                    `json:"runs"`
}, int) {
	t.Helper()
	w := NewWebServer(proxy, "test-config.yaml")
	mux := http.NewServeMux()
	w.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/fusion"+query, nil))
	var out struct {
		Workflows map[string]fusion.WorkflowStats `json:"workflows"`
		Runs      []fusion.Run                    `json:"runs"`
	}
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode /api/fusion: %v (%s)", err, rec.Body.String())
		}
	}
	return out, rec.Code
}

// awaitFusionState polls cond until the proxy's post-commit bookkeeping for
// the just-served request(s) is visible. finishFusion records the run +
// metrics AFTER the response stream completes, and the forwarded upstream
// Content-Length lets the client's ReadAll finish before that bookkeeping
// runs — under scheduler load an immediate read can legitimately miss the
// just-served run (observed flake: "runs = 0, want 1"). Condition-driven, not
// a fixed sleep: locally the first poll already passes.
func awaitFusionState(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestAPIFusion: a successful run is recorded with per-leg detail + the
// synthesis leg's outcome read back from the live-event hub; a quorum failure
// is recorded as a degraded run. The workflow filter isolates recipes.
func TestAPIFusion(t *testing.T) {
	t.Run("success run recorded", func(t *testing.T) {
		pa := newFakeUpstream(t, anthropicDraftResponder("draft-A"))
		pb := newFakeUpstream(t, anthropicDraftResponder("draft-B"))
		ps := newFakeUpstream(t, anthropicSSEResponder("final answer"))
		recipe := FusionConfig{
			Panel:       []RouteTarget{{Provider: "pa", Model: "ma"}, {Provider: "pb", Model: "mb"}},
			Synthesizer: RouteTarget{Provider: "ps", Model: "ms"},
		}
		proxy, px := newFusionRig(t, recipe, map[string]*fakeUpstream{"pa": pa, "pb": pb, "ps": ps})
		postAnthropic(t, px, fusionClientBody)

		awaitFusionState(t, "success run recorded", func() bool {
			o, _ := apiFusionGet(t, proxy, "")
			return len(o.Runs) == 1
		})
		out, code := apiFusionGet(t, proxy, "")
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200", code)
		}
		st := out.Workflows["recipe"]
		if st.Runs != 1 || st.QuorumMet != 1 || len(st.Degraded) != 0 {
			t.Errorf("stats = %+v, want 1 run, quorum met, no degrades", st)
		}
		if st.RunsToday != 1 {
			t.Errorf("runs_today = %d, want 1", st.RunsToday)
		}
		if st.PanelInput != 22 || st.PanelOutput != 14 { // 2 × anthropic draft {11,7}
			t.Errorf("panel tokens = %d/%d, want 22/14", st.PanelInput, st.PanelOutput)
		}
		if st.SynthInput != 50 || st.SynthOutput != 9 { // anthropicSSEResponder usage
			t.Errorf("synth tokens = %d/%d, want 50/9", st.SynthInput, st.SynthOutput)
		}
		if len(out.Runs) != 1 {
			t.Fatalf("runs = %d, want 1", len(out.Runs))
		}
		run := out.Runs[0]
		if run.Workflow != "recipe" || run.Route != "hard" || run.Degraded != "" || run.DraftsUsed != 2 || run.Quorum != 2 {
			t.Errorf("run = %+v", run)
		}
		if !run.SynthCommitted || run.SynthStatus != 200 || run.SynthLatencyMs < 0 {
			t.Errorf("synth read-back = committed %v status %d, want committed 200", run.SynthCommitted, run.SynthStatus)
		}
		if len(run.Legs) != 2 {
			t.Fatalf("legs = %d, want 2", len(run.Legs))
		}
		for _, l := range run.Legs {
			if l.Kind != "panel" || l.Status != 200 || l.Err != "" || l.Input != 11 || l.Output != 7 {
				t.Errorf("leg = %+v, want clean panel 200 with draft usage", l)
			}
		}
		// Metrics: ("fusion", recipe) run counted, no degrade. The Inc runs
		// after engine.Run returns (post-commit) — wait for it instead of
		// racing the handler goroutine.
		m := awaitCommitMetrics(t, proxy, counters.PMKey{Provider: "fusion", Model: "recipe"})[counters.PMKey{Provider: "fusion", Model: "recipe"}]
		if m.Requests != 1 || m.Failovers != 0 {
			t.Errorf("fusion metrics = requests %d failovers %d, want 1/0", m.Requests, m.Failovers)
		}
	})

	t.Run("quorum failure recorded as degraded", func(t *testing.T) {
		// pa is DELAYED so the two failures always land first: the quorum then
		// becomes unreachable and the collector cuts pa (deterministic).
		pa := newFakeUpstream(t, delayedResponder(300*time.Millisecond, anthropicDraftResponder("draft-A")))
		pb := newFakeUpstream(t, statusResponder(500))
		pc := newFakeUpstream(t, statusResponder(500))
		ps := newFakeUpstream(t, anthropicSSEResponder("direct answer"))
		recipe := FusionConfig{
			Panel: []RouteTarget{
				{Provider: "pa", Model: "ma"}, {Provider: "pb", Model: "mb"}, {Provider: "pc", Model: "mc"},
			},
			Synthesizer: RouteTarget{Provider: "ps", Model: "ms"},
		}
		proxy, px := newFusionRig(t, recipe, map[string]*fakeUpstream{"pa": pa, "pb": pb, "pc": pc, "ps": ps})
		postAnthropic(t, px, fusionClientBody)

		awaitFusionState(t, "degraded run recorded", func() bool {
			o, _ := apiFusionGet(t, proxy, "?workflow=recipe")
			return len(o.Runs) == 1
		})
		out, _ := apiFusionGet(t, proxy, "?workflow=recipe")
		st := out.Workflows["recipe"]
		if st.Runs != 1 || st.QuorumMet != 0 || st.Degraded[fusion.DegradedInsufficientProposers] != 1 {
			t.Errorf("stats = %+v, want 1 degraded run (insufficient_proposers)", st)
		}
		if len(out.Runs) != 1 {
			t.Fatalf("runs = %d, want 1", len(out.Runs))
		}
		run := out.Runs[0]
		if run.Degraded != fusion.DegradedInsufficientProposers || run.DraftsUsed != 0 {
			t.Errorf("run degraded = %q drafts %d, want insufficient_proposers/0", run.Degraded, run.DraftsUsed)
		}
		// The failures are visible per leg; the delayed member was cut once the
		// quorum became unreachable.
		byProv := map[string]fusion.LegObservation{}
		for _, l := range run.Legs {
			byProv[l.Provider] = l
		}
		if byProv["pb"].Status != 500 || byProv["pc"].Status != 500 {
			t.Errorf("failed leg statuses = %+v", byProv)
		}
		if !byProv["pa"].Cut {
			t.Errorf("pa leg should be cut after the quorum became unreachable: %+v", byProv["pa"])
		}
		// The run fell back to the synthesizer with the original body — still
		// committed, so the synth read-back is present.
		if !run.SynthCommitted || run.SynthStatus != 200 {
			t.Errorf("degraded synth = committed %v status %d", run.SynthCommitted, run.SynthStatus)
		}
		m := awaitCommitMetrics(t, proxy, counters.PMKey{Provider: "fusion", Model: "recipe"})[counters.PMKey{Provider: "fusion", Model: "recipe"}]
		if m.Requests != 1 || m.Failovers != 1 {
			t.Errorf("fusion metrics = requests %d failovers %d, want 1/1", m.Requests, m.Failovers)
		}
		// Filter to an unknown workflow: empty snapshot.
		out, _ = apiFusionGet(t, proxy, "?workflow=nope")
		if len(out.Workflows) != 0 || len(out.Runs) != 0 {
			t.Errorf("filtered snapshot = %+v, want empty", out)
		}
	})

	t.Run("grace-cut leg recorded", func(t *testing.T) {
		old := fusionGracePeriod
		fusionGracePeriod = 150 * time.Millisecond
		defer func() { fusionGracePeriod = old }()
		pa := newFakeUpstream(t, anthropicDraftResponder("draft-A"))
		pb := newFakeUpstream(t, anthropicDraftResponder("draft-B"))
		pc := newFakeUpstream(t, delayedResponder(600*time.Millisecond, anthropicDraftResponder("draft-C")))
		ps := newFakeUpstream(t, anthropicSSEResponder("final answer"))
		recipe := FusionConfig{
			Panel: []RouteTarget{
				{Provider: "pa", Model: "ma"}, {Provider: "pb", Model: "mb"}, {Provider: "pc", Model: "mc"},
			},
			Synthesizer: RouteTarget{Provider: "ps", Model: "ms"},
		}
		proxy, px := newFusionRig(t, recipe, map[string]*fakeUpstream{"pa": pa, "pb": pb, "pc": pc, "ps": ps})
		postAnthropic(t, px, fusionClientBody)

		awaitFusionState(t, "grace-cut run recorded", func() bool {
			o, _ := apiFusionGet(t, proxy, "")
			return len(o.Runs) == 1
		})
		out, _ := apiFusionGet(t, proxy, "")
		if len(out.Runs) != 1 {
			t.Fatalf("runs = %d, want 1", len(out.Runs))
		}
		var cutSeen bool
		for _, l := range out.Runs[0].Legs {
			if l.Provider == "pc" {
				cutSeen = l.Cut
			}
		}
		if !cutSeen {
			t.Errorf("straggler leg not marked cut: %+v", out.Runs[0].Legs)
		}
	})
}

// --- cost gates ---

// TestFusion_BudgetExceeded: with max_runs_per_day=1 the first request
// orchestrates and the second degrades to a direct synthesizer call (panel
// untouched), recorded as budget_exceeded.
func TestFusion_BudgetExceeded(t *testing.T) {
	pa := newFakeUpstream(t, anthropicDraftResponder("draft-A"))
	pb := newFakeUpstream(t, anthropicDraftResponder("draft-B"))
	ps := newFakeUpstream(t, anthropicSSEResponder("answer"))
	recipe := FusionConfig{
		Panel:         []RouteTarget{{Provider: "pa", Model: "ma"}, {Provider: "pb", Model: "mb"}},
		Synthesizer:   RouteTarget{Provider: "ps", Model: "ms"},
		MaxRunsPerDay: 1,
	}
	proxy, px := newFusionRig(t, recipe, map[string]*fakeUpstream{"pa": pa, "pb": pb, "ps": ps})

	postAnthropic(t, px, fusionClientBody)
	if pa.hits() != 1 || pb.hits() != 1 {
		t.Fatalf("first request should orchestrate (hits pa=%d pb=%d)", pa.hits(), pb.hits())
	}
	postAnthropic(t, px, fusionClientBody)
	if pa.hits() != 1 || pb.hits() != 1 {
		t.Errorf("second request should NOT fan out (hits pa=%d pb=%d)", pa.hits(), pb.hits())
	}
	if strings.Contains(ps.lastBody(), "CANDIDATE") {
		t.Errorf("over-budget body should be the original (no candidates): %s", ps.lastBody())
	}
	awaitFusionState(t, "budget-exceeded run recorded", func() bool {
		o, _ := apiFusionGet(t, proxy, "")
		return o.Workflows["recipe"].Runs == 2
	})
	out, _ := apiFusionGet(t, proxy, "")
	st := out.Workflows["recipe"]
	if st.Runs != 2 || st.QuorumMet != 1 || st.Degraded[fusion.DegradedBudgetExceeded] != 1 || st.RunsToday != 1 {
		t.Errorf("stats = %+v, want runs 2, quorum 1, budget_exceeded 1, runs_today 1", st)
	}
}

// TestFusion_FirstTurnOnly: a first_turn_only recipe orchestrates the first
// turn and answers a multi-turn conversation directly (multi_turn), without
// touching the panel.
func TestFusion_FirstTurnOnly(t *testing.T) {
	pa := newFakeUpstream(t, anthropicDraftResponder("draft-A"))
	pb := newFakeUpstream(t, anthropicDraftResponder("draft-B"))
	ps := newFakeUpstream(t, anthropicSSEResponder("answer"))
	recipe := FusionConfig{
		Panel:         []RouteTarget{{Provider: "pa", Model: "ma"}, {Provider: "pb", Model: "mb"}},
		Synthesizer:   RouteTarget{Provider: "ps", Model: "ms"},
		FirstTurnOnly: true,
	}
	proxy, px := newFusionRig(t, recipe, map[string]*fakeUpstream{"pa": pa, "pb": pb, "ps": ps})

	postAnthropic(t, px, fusionClientBody) // single user message → orchestrated
	if pa.hits() != 1 {
		t.Fatalf("first turn should orchestrate (pa hits %d)", pa.hits())
	}
	multiTurn := `{"model":"hard","max_tokens":100,"stream":true,"messages":[` +
		`{"role":"user","content":"q1"},{"role":"assistant","content":"a1"},{"role":"user","content":"q2"}]}`
	postAnthropic(t, px, multiTurn)
	if pa.hits() != 1 || pb.hits() != 1 {
		t.Errorf("multi-turn request should NOT fan out (hits pa=%d pb=%d)", pa.hits(), pb.hits())
	}
	if strings.Contains(ps.lastBody(), "CANDIDATE") {
		t.Errorf("multi-turn body should be the original (no candidates): %s", ps.lastBody())
	}
	awaitFusionState(t, "multi-turn degraded run recorded", func() bool {
		o, _ := apiFusionGet(t, proxy, "")
		return o.Workflows["recipe"].Runs == 2
	})
	out, _ := apiFusionGet(t, proxy, "")
	st := out.Workflows["recipe"]
	if st.Runs != 2 || st.Degraded[fusion.DegradedMultiTurn] != 1 || st.RunsToday != 1 {
		t.Errorf("stats = %+v, want runs 2, multi_turn 1, runs_today 1 (degraded runs don't consume budget)", st)
	}
}

// --- quality knobs ---

// TestFusion_JudgeReport: a configured judge reviews the candidates (one
// non-streaming call through the leg pipeline) and its report is injected
// into the synthesis body before the candidate sections.
func TestFusion_JudgeReport(t *testing.T) {
	pa := newFakeUpstream(t, anthropicDraftResponder("draft-A"))
	pb := newFakeUpstream(t, anthropicDraftResponder("draft-B"))
	pj := newFakeUpstream(t, anthropicDraftResponder("judge: consensus on X"))
	ps := newFakeUpstream(t, anthropicSSEResponder("final answer"))
	judge := RouteTarget{Provider: "pj", Model: "mj"}
	recipe := FusionConfig{
		Panel:       []RouteTarget{{Provider: "pa", Model: "ma"}, {Provider: "pb", Model: "mb"}},
		Synthesizer: RouteTarget{Provider: "ps", Model: "ms"},
		Judge:       &judge,
	}
	proxy, px := newFusionRig(t, recipe, map[string]*fakeUpstream{"pa": pa, "pb": pb, "pj": pj, "ps": ps})
	out := postAnthropic(t, px, fusionClientBody)
	if !strings.Contains(out, "final answer") {
		t.Fatalf("client body missing answer: %s", out)
	}
	// The judge got a non-streaming, model-rewritten review request carrying
	// the judge instruction + both candidates, tools stripped.
	if pj.hits() != 1 {
		t.Fatalf("judge hits = %d, want 1", pj.hits())
	}
	var jb map[string]any
	if err := json.Unmarshal([]byte(pj.lastBody()), &jb); err != nil {
		t.Fatalf("judge body not JSON: %v", err)
	}
	if jb["model"] != "mj" || jb["stream"] != false {
		t.Errorf("judge body model/stream = %v/%v, want mj/false", jb["model"], jb["stream"])
	}
	jsys, _ := jb["system"].(string)
	if !strings.Contains(jsys, "评审模型") || !strings.Contains(jsys, "draft-A") || !strings.Contains(jsys, "draft-B") {
		t.Errorf("judge system missing instruction/candidates: %q", jsys)
	}
	// The synthesis body carries the judge report BEFORE the candidates.
	var synth struct {
		System string `json:"system"`
	}
	if err := json.Unmarshal([]byte(ps.lastBody()), &synth); err != nil {
		t.Fatalf("synthesis body not JSON: %v", err)
	}
	ji := strings.Index(synth.System, "<JUDGE_ANALYSIS>")
	ci := strings.Index(synth.System, "<CANDIDATE 1>")
	if ji < 0 || !strings.Contains(synth.System, "judge: consensus on X") {
		t.Errorf("synthesis system missing judge report: %q", synth.System)
	}
	if ci < 0 || ji > ci {
		t.Errorf("judge report should precede candidates (judge %d, candidate %d)", ji, ci)
	}
	// Registry: judge leg observed (kind=judge), report used, judge tokens
	// aggregated separately from the panel.
	awaitFusionState(t, "judge run recorded", func() bool {
		o, _ := apiFusionGet(t, proxy, "")
		return len(o.Runs) == 1
	})
	outAPI, _ := apiFusionGet(t, proxy, "")
	run := outAPI.Runs[0]
	if !run.JudgeUsed {
		t.Errorf("run.JudgeUsed = false, want true")
	}
	var judgeLeg *fusion.LegObservation
	for i := range run.Legs {
		if run.Legs[i].Kind == "judge" {
			judgeLeg = &run.Legs[i]
		}
	}
	if judgeLeg == nil || judgeLeg.Status != 200 || judgeLeg.Err != "" {
		t.Errorf("judge leg = %+v, want clean 200", judgeLeg)
	}
	st := outAPI.Workflows["recipe"]
	if st.JudgeInput != 11 || st.JudgeOutput != 7 { // anthropicDraftResponder usage
		t.Errorf("judge tokens = %d/%d, want 11/7", st.JudgeInput, st.JudgeOutput)
	}
	// The judge call went through the standard per-(provider,model) books.
	if m := proxy.metrics.Snapshot()[counters.PMKey{Provider: "pj", Model: "mj"}]; m.Requests != 1 {
		t.Errorf("judge metrics requests = %d, want 1", m.Requests)
	}
}

// TestFusion_JudgeFailure: a failing judge only skips the report — the run
// still synthesizes from the candidates, is not degraded, and records the
// judge leg's error.
func TestFusion_JudgeFailure(t *testing.T) {
	pa := newFakeUpstream(t, anthropicDraftResponder("draft-A"))
	pb := newFakeUpstream(t, anthropicDraftResponder("draft-B"))
	pj := newFakeUpstream(t, statusResponder(500))
	ps := newFakeUpstream(t, anthropicSSEResponder("final answer"))
	judge := RouteTarget{Provider: "pj", Model: "mj"}
	recipe := FusionConfig{
		Panel:       []RouteTarget{{Provider: "pa", Model: "ma"}, {Provider: "pb", Model: "mb"}},
		Synthesizer: RouteTarget{Provider: "ps", Model: "ms"},
		Judge:       &judge,
	}
	proxy, px := newFusionRig(t, recipe, map[string]*fakeUpstream{"pa": pa, "pb": pb, "pj": pj, "ps": ps})
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
	if strings.Contains(synth.System, "JUDGE_ANALYSIS") {
		t.Errorf("failed judge should leave no report section: %q", synth.System)
	}
	if !strings.Contains(synth.System, "draft-A") {
		t.Errorf("synthesis should still carry candidates: %q", synth.System)
	}
	awaitFusionState(t, "judge-failure run recorded", func() bool {
		o, _ := apiFusionGet(t, proxy, "")
		return len(o.Runs) == 1
	})
	outAPI, _ := apiFusionGet(t, proxy, "")
	run := outAPI.Runs[0]
	if run.Degraded != "" || run.JudgeUsed {
		t.Errorf("run = degraded %q judgeUsed %v, want no degrade, no judge report", run.Degraded, run.JudgeUsed)
	}
	var judgeLeg *fusion.LegObservation
	for i := range run.Legs {
		if run.Legs[i].Kind == "judge" {
			judgeLeg = &run.Legs[i]
		}
	}
	if judgeLeg == nil || judgeLeg.Status != 500 || judgeLeg.Err == "" {
		t.Errorf("judge leg = %+v, want recorded 500 failure", judgeLeg)
	}
}

// TestFusion_InstructionOverride: recipe.Instruction replaces the fixed
// synthesis preamble.
func TestFusion_InstructionOverride(t *testing.T) {
	pa := newFakeUpstream(t, anthropicDraftResponder("draft-A"))
	pb := newFakeUpstream(t, anthropicDraftResponder("draft-B"))
	ps := newFakeUpstream(t, anthropicSSEResponder("final answer"))
	recipe := FusionConfig{
		Panel:       []RouteTarget{{Provider: "pa", Model: "ma"}, {Provider: "pb", Model: "mb"}},
		Synthesizer: RouteTarget{Provider: "ps", Model: "ms"},
		Instruction: "自定义汇总指令X",
	}
	_, px := newFusionRig(t, recipe, map[string]*fakeUpstream{"pa": pa, "pb": pb, "ps": ps})
	postAnthropic(t, px, fusionClientBody)
	var synth struct {
		System string `json:"system"`
	}
	if err := json.Unmarshal([]byte(ps.lastBody()), &synth); err != nil {
		t.Fatalf("synthesis body not JSON: %v", err)
	}
	if !strings.Contains(synth.System, "自定义汇总指令X") {
		t.Errorf("synthesis missing custom instruction: %q", synth.System)
	}
	if strings.Contains(synth.System, "结果汇总模型") {
		t.Errorf("default instruction should be replaced: %q", synth.System)
	}
}
