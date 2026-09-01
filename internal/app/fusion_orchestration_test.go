package app

import (
	"encoding/json"
	"io"
	"model-proxy/internal/observe/counters"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"model-proxy/internal/observe/requestlog"
)

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
