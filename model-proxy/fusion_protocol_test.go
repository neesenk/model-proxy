package main

import (
	"encoding/json"
	"io"
	"model-proxy/internal/observe/counters"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

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
	toks := proxy.tokens.Snapshot()
	if u := toks[counters.TokenKey{Provider: "pb", Model: "mb"}]; u.Input != 21 || u.Output != 9 {
		t.Errorf("openai member usage = %+v, want {21 9}", u)
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
	if m := proxy.metrics.Snapshot()[counters.PMKey{Provider: "pa", Model: "ma"}]; m.Failures != 0 || m.Failovers != 1 {
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
	if m := proxy.metrics.Snapshot()[counters.PMKey{Provider: "pa", Model: "ma"}]; m.Failovers != 0 || m.Requests != 1 {
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
