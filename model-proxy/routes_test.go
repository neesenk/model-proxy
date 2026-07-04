package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newCaptureUpstream returns a mock upstream that records the `model` field of
// each request body and responds with the given status + body.
func newCaptureUpstream(status int, body string) (*httptest.Server, *[]string) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m struct {
			Model string `json:"model"`
		}
		json.Unmarshal(b, &m)
		seen = append(seen, m.Model)
		w.Header().Set("content-type", "application/json")
		if status != 200 {
			w.WriteHeader(status)
		}
		w.Write([]byte(body))
	}))
	return srv, &seen
}

// TestForward_ClaudeMapping: anthropic translates claude-sonnet-4-6 via
// claude_mapping to the exposed "glm-5.2" route (and rewrites the body model);
// a name not in the mapping is used as-is; openai skips the mapping entirely.
func TestForward_ClaudeMapping(t *testing.T) {
	up, seen := newCaptureUpstream(200, `{}`)
	defer up.Close()
	cfg := &Config{
		Providers: map[string]Provider{
			"compass": {OpenAIBaseURL: up.URL, AnthropicBaseURL: up.URL, Provider: "static"},
		},
		Routes: map[string][]RouteTarget{
			"glm-5.2": {{Provider: "compass", Model: "glm-5.2"}},
		},
		ClaudeMapping: map[string]string{
			"claude-sonnet-4-6": "glm-5.2",
		},
	}
	p := NewProxy(cfg)
	p.providers["compass"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	// 1) anthropic claude-sonnet-4-6 → mapped to glm-5.2 → upstream sees glm-5.2.
	*seen = nil
	post(t, px.URL+"/v1/messages", `{"model":"claude-sonnet-4-6","messages":[]}`)
	if len(*seen) != 1 || (*seen)[0] != "glm-5.2" {
		t.Errorf("anthropic mapped name: upstream model=%v, want [glm-5.2]", *seen)
	}

	// 2) anthropic glm-5.2 (not in claude_mapping) → used as-is → upstream sees glm-5.2.
	*seen = nil
	post(t, px.URL+"/v1/messages", `{"model":"glm-5.2","messages":[]}`)
	if len(*seen) != 1 || (*seen)[0] != "glm-5.2" {
		t.Errorf("anthropic unmapped name: upstream model=%v, want [glm-5.2]", *seen)
	}

	// 3) openai claude-sonnet-4-6 → no mapping → not in routes → 502.
	resp, err := http.Post(px.URL+"/v1/chat/completions", "application/json", stringReader(`{"model":"claude-sonnet-4-6","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Errorf("openai claude name (no mapping): status=%d, want 502", resp.StatusCode)
	}
}

// TestForward_Failover: when the primary target returns 5xx, the proxy fails
// over to the next target and the client gets the fallback's 200.
func TestForward_Failover(t *testing.T) {
	primary, primarySeen := newCaptureUpstream(500, `{"error":"primary"}`)
	defer primary.Close()
	fallback, fallbackSeen := newCaptureUpstream(200, `{"ok":true}`)
	defer fallback.Close()
	cfg := &Config{
		Providers: map[string]Provider{
			"primary":  {OpenAIBaseURL: primary.URL, Provider: "static"},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: "static"},
		},
		Routes: map[string][]RouteTarget{
			"m1": {
				{Provider: "primary", Model: "m1", Priority: 1},
				{Provider: "fallback", Model: "m1", Priority: 2},
			},
		},
	}
	p := NewProxy(cfg)
	p.providers["primary"] = &testProv{key: "p"}
	p.providers["fallback"] = &testProv{key: "f"}
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	resp, err := http.Post(px.URL+"/v1/chat/completions", "application/json", stringReader(`{"model":"m1","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("failover: status=%d body=%s, want 200 from fallback", resp.StatusCode, body)
	}
	if len(*primarySeen) != 1 {
		t.Errorf("primary should be tried once, got %d", len(*primarySeen))
	}
	if len(*fallbackSeen) != 1 {
		t.Errorf("fallback should be tried once, got %d", len(*fallbackSeen))
	}
}

// TestForward_PeakHours: a target inside its peak_hours window is deprioritized
// (effective priority increased), so a same-priority non-peak target is tried first.
func TestForward_PeakHours(t *testing.T) {
	peakUp, peakSeen := newCaptureUpstream(200, `{"from":"peak"}`)
	defer peakUp.Close()
	normalUp, normalSeen := newCaptureUpstream(200, `{"from":"normal"}`)
	defer normalUp.Close()
	cfg := &Config{
		Providers: map[string]Provider{
			"peak":   {OpenAIBaseURL: peakUp.URL, Provider: "static", PeakHours: "00:00-23:59"}, // always in peak → deprioritized
			"normal": {OpenAIBaseURL: normalUp.URL, Provider: "static"},
		},
		Routes: map[string][]RouteTarget{
			"m1": {
				{Provider: "peak", Model: "m1", Priority: 0},
				{Provider: "normal", Model: "m1", Priority: 0}, // same priority, but non-peak → tried first
			},
		},
	}
	p := NewProxy(cfg)
	p.providers["peak"] = &testProv{key: "p"}
	p.providers["normal"] = &testProv{key: "n"}
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	post(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`)
	if len(*normalSeen) != 1 || len(*peakSeen) != 0 {
		t.Errorf("peak deprioritization: normal=%d peak=%d, want normal=1 peak=0", len(*normalSeen), len(*peakSeen))
	}
}

// TestForward_PeakGrouping: non-peak targets are tried before peak targets even
// when the peak target has a lower (better) priority — the schedule groups by
// peak-status first, then priority.
func TestForward_PeakGrouping(t *testing.T) {
	peakUp, peakSeen := newCaptureUpstream(200, `{}`)
	defer peakUp.Close()
	normalUp, normalSeen := newCaptureUpstream(200, `{}`)
	defer normalUp.Close()
	cfg := &Config{
		Providers: map[string]Provider{
			"peak":   {OpenAIBaseURL: peakUp.URL, Provider: "static", PeakHours: "00:00-23:59"},
			"normal": {OpenAIBaseURL: normalUp.URL, Provider: "static"},
		},
		Routes: map[string][]RouteTarget{
			"m1": {
				{Provider: "peak", Model: "m1", Priority: 0},   // peak group, best priority
				{Provider: "normal", Model: "m1", Priority: 9}, // non-peak group, worse priority — but non-peak wins
			},
		},
	}
	p := NewProxy(cfg)
	p.providers["peak"] = &testProv{key: "p"}
	p.providers["normal"] = &testProv{key: "n"}
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	post(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`)
	if len(*normalSeen) != 1 || len(*peakSeen) != 0 {
		t.Errorf("non-peak grouping: normal=%d peak=%d, want normal=1 peak=0 (non-peak before peak regardless of priority)", len(*normalSeen), len(*peakSeen))
	}
}
