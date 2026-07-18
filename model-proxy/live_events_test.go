package main

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestEventHub_PublishSubscribeRecent(t *testing.T) {
	h := newEventHub()
	// Publish before anyone subscribes → lands in the recent ring.
	h.publish(liveEvent{Type: "end", Agent: "claude-code", Exposed: "glm", Status: 200})

	ch, recent, cancel := h.subscribe()
	defer cancel()
	if len(recent) != 1 || recent[0].Agent != "claude-code" {
		t.Errorf("recent replay = %+v want 1 claude-code event", recent)
	}
	// New publishes reach the subscriber.
	h.publish(liveEvent{Type: "end", Agent: "codex", Status: 500})
	select {
	case e := <-ch:
		if e.Agent != "codex" || e.Status != 500 {
			t.Errorf("got %+v want codex/500", e)
		}
	case <-time.After(time.Second):
		t.Fatal("did not receive published event")
	}

	// Cancel unsubscribes (channel removed from subs).
	cancel()
	h.mu.Lock()
	n := len(h.subs)
	h.mu.Unlock()
	if n != 0 {
		t.Errorf("after cancel, subs=%d want 0", n)
	}
}

func TestEventHub_DropSlowSubscriber(t *testing.T) {
	h := newEventHub()
	ch, _, cancel := h.subscribe()
	defer cancel()
	// Fill the 32-buffer without reading, then keep publishing: publish must NOT
	// block (slow subscriber is dropped, not stalled).
	for i := 0; i < 32; i++ {
		h.publish(liveEvent{Type: "end"})
	}
	done := make(chan struct{})
	go func() {
		h.publish(liveEvent{Type: "end"}) // would block a non-drop implementation
		h.publish(liveEvent{Type: "end"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("publish blocked on a full subscriber buffer (must drop, not stall)")
	}
	// Drain to avoid leaking the buffered channel.
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

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
		Providers: map[string]Provider{"z": {OpenAIBaseURL: up.URL, Provider: "static"}},
		Routes:    map[string][]RouteTarget{"glm": {{Provider: "z", Model: "glm"}}},
	}
	p := NewProxy(cfg)
	p.providers["z"] = &testProv{key: "k"}
	ch, _, cancel := p.events.subscribe()
	defer cancel()

	px := httptest.NewServer(http.HandlerFunc(p.handler))
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
	var got []liveEvent
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
	var endEv liveEvent
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

// TestServeEvents_SSE: the /api/events endpoint streams events as SSE `data:`
// lines; a published event reaches an HTTP subscriber.
func TestServeEvents_SSE(t *testing.T) {
	p := NewProxy(&Config{
		Providers: map[string]Provider{"z": {OpenAIBaseURL: "https://x", Provider: "static"}},
		Routes:    map[string][]RouteTarget{"glm": {{Provider: "z", Model: "glm"}}},
	})
	srv := httptest.NewServer(http.HandlerFunc(p.serveEvents))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
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
	p.events.publish(liveEvent{Type: "end", Agent: "codex", Exposed: "glm", Provider: "z", Status: 200})
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	found := false
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, "\"type\":\"end\"") && strings.Contains(line, "codex") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("did not receive the published SSE event; scan err=%v", sc.Err())
	}
}

// TestLiveEvents_EarlyFailures: the two early-return paths that previously
// emitted NO event — a missing-model 400 and an unknown-model 502 — now emit a
// terminal "end" event, so an agent retry-looping on a malformed/removed model
// is visible (the core "catch a retry loop" use case).
func TestLiveEvents_EarlyFailures(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{"z": {OpenAIBaseURL: "http://127.0.0.1:1", Provider: "static"}},
		Routes:    map[string][]RouteTarget{"glm": {{Provider: "z", Model: "glm"}}},
	}
	p := NewProxy(cfg)
	p.providers["z"] = &testProv{key: "k"}
	ch, _, cancel := p.events.subscribe()
	defer cancel()
	px := httptest.NewServer(http.HandlerFunc(p.handler))
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
			Providers: map[string]Provider{"z": {OpenAIBaseURL: up.URL, Provider: "static"}},
			Routes:    routes,
		}
		if cache {
			cfg.Cache = CacheConfig{Enabled: true, TTL: "1h"}
		}
		p := NewProxy(cfg)
		p.providers["z"] = &testProv{key: "k"}
		return p
	}

	// Cache hit → end event with Provider "(cache)".
	pc := mk(map[string][]RouteTarget{"glm": {{Provider: "z", Model: "glm"}}}, true)
	ch, _, cancel := pc.events.subscribe()
	defer cancel()
	pxc := httptest.NewServer(http.HandlerFunc(pc.handler))
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
		Providers: map[string]Provider{"dead": {OpenAIBaseURL: "http://127.0.0.1:1", Provider: "static"}},
		Routes:    map[string][]RouteTarget{"glm": {{Provider: "dead", Model: "glm"}}},
	}
	pf2 := NewProxy(cfg)
	pf2.providers["dead"] = &testProv{key: "k"}
	ch2, _, cancel2 := pf2.events.subscribe()
	defer cancel2()
	pxf := httptest.NewServer(http.HandlerFunc(pf2.handler))
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
