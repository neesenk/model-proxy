package app

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	observeevents "model-proxy/internal/observe/events"
)

// TestForward_EmitsLiveEvents: a served request publishes a start event (on
// entry) and an end event (on commit) to the hub, carrying agent/route/provider/
// status/latency.
func TestForward_EmitsLiveEvents(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer up.Close()

	cfg := &Config{
		Providers: map[string]Provider{"z": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"glm": {{Provider: "z", Model: "glm"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["z"] = &testProv{key: "k"}
	ch, _, cancel := p.events.Subscribe()
	defer cancel()

	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()
	req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/responses", strings.NewReader(`{"model":"glm","input":[]}`))
	req.Header.Set("user-agent", "claude-cli/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Collect events with a short grace for the async publish.
	var got []observeevents.Event
	deadline := time.After(time.Second)
	for len(got) < 2 {
		select {
		case e := <-ch:
			got = append(got, e)
		case <-deadline:
			t.Fatalf("received %d events, want 2 (start+end): %+v", len(got), got)
		}
	}
	if got[0].Type != "start" {
		t.Errorf("first event type=%q want start", got[0].Type)
	}
	if got[0].Agent != "claude-code" || got[0].Exposed != "glm" {
		t.Errorf("start event = %+v want agent claude-code / exposed glm", got[0])
	}
	var endEv observeevents.Event
	for _, e := range got {
		if e.Type == "end" {
			endEv = e
		}
	}
	if endEv.Type != "end" {
		t.Fatal("no end event received")
	}
	if endEv.Provider != "z" || endEv.UpstreamModel != "glm" || endEv.Status != 200 {
		t.Errorf("end event = %+v want provider z / glm / 200", endEv)
	}
	if endEv.LatencyMs < 0 {
		t.Errorf("end latency=%d negative", endEv.LatencyMs)
	}
}

// TestForward_LiveEndEventCarriesStreamUsage proves that the terminal live
// event is published only after the client-facing SSE stream has passed through
// targetexec's usage capture. The usage frame is the normal OpenAI Chat
// completion shape, rather than a synthetic event DTO.
func TestForward_LiveEndEventCarriesStreamUsage(t *testing.T) {
	const upstreamStream = "data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-x\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"chatcmpl_1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-x\",\"choices\":[],\"usage\":{\"prompt_tokens\":17,\"completion_tokens\":9,\"total_tokens\":26}}\n\n" +
		"data: [DONE]\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		io.WriteString(w, upstreamStream)
	}))
	defer up.Close()

	p := newTestProxy(t, &Config{
		Providers: map[string]Provider{"z": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"glm": {{Provider: "z", Model: "gpt-x", Protocol: "openai"}}},
	})
	p.providers["z"] = &testProv{key: "k"}
	ch, _, cancel := p.events.Subscribe()
	defer cancel()
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	req, err := http.NewRequest(http.MethodPost, px.URL+"/v1/chat/completions", strings.NewReader(
		`{"model":"glm","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("user-agent", "claude-cli/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || !strings.Contains(resp.Header.Get("content-type"), "text/event-stream") || string(body) != upstreamStream {
		t.Fatalf("client stream status=%d content-type=%q body=%q", resp.StatusCode, resp.Header.Get("content-type"), body)
	}

	var start, end observeevents.Event
	deadline := time.After(time.Second)
	for end.Type == "" {
		select {
		case event := <-ch:
			switch event.Type {
			case "start":
				start = event
			case "end":
				end = event
			}
		case <-deadline:
			t.Fatalf("did not receive terminal live event; start=%+v end=%+v", start, end)
		}
	}
	if start.Agent != "claude-code" || start.Exposed != "glm" {
		t.Errorf("start event = %+v, want agent claude-code / exposed glm", start)
	}
	if start.RequestID == "" || end.RequestID != start.RequestID ||
		end.Agent != "claude-code" || end.Protocol != "openai" ||
		end.Exposed != "glm" || end.Provider != "z" || end.UpstreamModel != "gpt-x" || end.Status != http.StatusOK ||
		end.Input != 17 || end.Output != 9 || end.LatencyMs < 0 {
		t.Errorf("start=%+v terminal=%+v, want matching request id; claude-code/openai/glm/z/gpt-x/200; usage 17/9; non-negative latency", start, end)
	}
}

// TestServeEvents_SSE: the /api/events endpoint streams events as SSE `data:`
// lines; a published event reaches an HTTP subscriber.
func TestServeEvents_SSE(t *testing.T) {
	p := newTestProxy(t, &Config{
		Providers: map[string]Provider{"z": {OpenAIBaseURL: "https://x", Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"glm": {{Provider: "z", Model: "glm"}}},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { observeevents.ServeEvents(p.events, w, r) }))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("content-type"); !strings.Contains(ct, "text/event-stream") {
		t.Errorf("content-type=%q want text/event-stream", ct)
	}

	// Publish an event; it must arrive as an SSE data line.
	p.events.Publish(observeevents.Event{
		Type: "end", RequestID: "sse-test", Agent: "codex",
		Exposed: "glm", Provider: "z", Status: http.StatusOK,
	})
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var got observeevents.Event
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &got); err != nil {
			t.Fatalf("decode SSE event %q: %v", line, err)
		}
		if got.RequestID == "sse-test" {
			break
		}
	}
	if got.RequestID != "sse-test" {
		t.Fatalf("did not receive the published SSE event before deadline; scan err=%v context err=%v", sc.Err(), ctx.Err())
	}
	want := observeevents.Event{
		Type: "end", RequestID: "sse-test", Agent: "codex", Exposed: "glm",
		Provider: "z", Status: http.StatusOK,
	}
	if got.Type != want.Type || got.Agent != want.Agent || got.Exposed != want.Exposed ||
		got.Provider != want.Provider || got.Status != want.Status {
		t.Errorf("SSE event = %+v, want core fields %+v", got, want)
	}
}

// TestLiveEvents_EarlyFailures: the two early-return paths that previously
// emitted NO event — a missing-model 400 and an unknown-model 502 — now emit a
// terminal "end" event, so an agent retry-looping on a malformed/removed model
// is visible (the core "catch a retry loop" use case).
func TestLiveEvents_EarlyFailures(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{"z": {OpenAIBaseURL: "http://127.0.0.1:1", Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"glm": {{Provider: "z", Model: "glm"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["z"] = &testProv{key: "k"}
	ch, _, cancel := p.events.Subscribe()
	defer cancel()
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	expectEnd := func(body string, wantStatus int) {
		resp, err := http.Post(px.URL+"/v1/responses", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		for {
			select {
			case e := <-ch:
				if e.Type == "end" && e.Status == wantStatus {
					return
				}
			case <-time.After(time.Second):
				t.Fatalf("no end event (status=%d) for body %q", wantStatus, body)
			}
		}
	}
	// Missing model → 400 → end event.
	expectEnd(`{"input":[]}`, http.StatusBadRequest)
	// Unknown model → 502 → end event.
	expectEnd(`{"model":"ghost","input":[]}`, http.StatusBadGateway)
}

// TestLiveEvents_CacheHitAndAllFailed (F6c): the two live-view blind spots — a
// cache hit (returns before the normal start event) and an all-targets-failed
// 502 — each emit an explicit end event. Without this, a retry-looping agent
// served from cache or always 502-ing is invisible to the live monitor.
func TestLiveEvents_CacheHitAndAllFailed(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()

	mk := func(routes map[string][]RouteTarget, cache bool) *Proxy {
		cfg := &Config{
			Providers: map[string]Provider{"z": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
			Routes:    routes,
		}
		if cache {
			cfg.Cache = CacheConfig{Enabled: true, TTL: "1h"}
		}
		p := newTestProxy(t, cfg)
		p.providers["z"] = &testProv{key: "k"}
		return p
	}

	// Cache hit → end event with Provider "(cache)".
	pc := mk(map[string][]RouteTarget{"glm": {{Provider: "z", Model: "glm"}}}, true)
	ch, _, cancel := pc.events.Subscribe()
	defer cancel()
	pxc := httptest.NewServer(http.HandlerFunc(pc.Handler))
	defer pxc.Close()
	post(t, pxc.URL+"/v1/responses", `{"model":"glm","input":[]}`) // prime
	post(t, pxc.URL+"/v1/responses", `{"model":"glm","input":[]}`) // cache hit
	gotCache := false
	deadline := time.After(time.Second)
	for !gotCache {
		select {
		case e := <-ch:
			if e.Type == "end" && e.Provider == "(cache)" {
				gotCache = true
			}
		case <-deadline:
			t.Fatal("cache hit did not emit a live end event with provider (cache)")
		}
	}

	// All-failed 502 → end event with Status 502.
	cfg := &Config{
		Providers: map[string]Provider{"dead": {OpenAIBaseURL: "http://127.0.0.1:1", Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"glm": {{Provider: "dead", Model: "glm"}}},
	}
	pf2 := newTestProxy(t, cfg)
	pf2.providers["dead"] = &testProv{key: "k"}
	ch2, _, cancel2 := pf2.events.Subscribe()
	defer cancel2()
	pxf := httptest.NewServer(http.HandlerFunc(pf2.Handler))
	defer pxf.Close()
	resp, err := http.Post(pxf.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"glm","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("all-failed status=%d want 502", resp.StatusCode)
	}
	resp.Body.Close()
	got502 := false
	deadline2 := time.After(time.Second)
	for !got502 {
		select {
		case e := <-ch2:
			if e.Type == "end" && e.Status == http.StatusBadGateway {
				got502 = true
			}
		case <-deadline2:
			t.Fatal("all-failed 502 did not emit a live end event")
		}
	}
}
