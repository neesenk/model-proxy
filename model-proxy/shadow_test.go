package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestForceProvider_OverridesRouting: a request with x-mp-force-provider is
// narrowed to that provider, bypassing the normal schedule (which would pick the
// priority-1 provider).
func TestForceProvider_OverridesRouting(t *testing.T) {
	var aHit, bHit bool
	aUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		aHit = true
		w.Write([]byte(`{}`))
	}))
	defer aUp.Close()
	bUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bHit = true
		w.Write([]byte(`{}`))
	}))
	defer bUp.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"a": {OpenAIBaseURL: aUp.URL, Provider: "static"},
			"b": {OpenAIBaseURL: bUp.URL, Provider: "static"},
		},
		Routes: map[string][]RouteTarget{
			"glm": {
				{Provider: "a", Model: "glm", Priority: 1},
				{Provider: "b", Model: "glm", Priority: 2},
			},
		},
	}
	p := newTestProxy(t, cfg)
	p.providers["a"] = &testProv{key: "a"}
	p.providers["b"] = &testProv{key: "b"}
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/responses", strings.NewReader(`{"model":"glm","input":[]}`))
	req.Header.Set("x-mp-force-provider", "b")
	resp, err := http.DefaultClient.Do(req.WithContext(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if !bHit {
		t.Error("force-provider b was not hit")
	}
	if aHit {
		t.Error("priority-1 provider a was hit despite force-provider override")
	}
}

func TestShadowDispatchKeepsCapturedReloadGeneration(t *testing.T) {
	primaryStarted := make(chan struct{})
	releasePrimary := make(chan struct{})
	primaryUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(primaryStarted)
		<-releasePrimary
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"id":"c1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer primaryUpstream.Close()
	shadowHit := make(chan struct{}, 1)
	shadowUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		shadowHit <- struct{}{}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer shadowUpstream.Close()

	oldConfig := &Config{
		Providers: map[string]Provider{
			"primary":   {Provider: "static", OpenAIBaseURL: primaryUpstream.URL},
			"candidate": {Provider: "static", OpenAIBaseURL: shadowUpstream.URL},
		},
		Routes: map[string][]RouteTarget{
			"alias": {{Provider: "primary", Model: "primary-model", Protocol: "openai"}},
		},
		Shadow: map[string]ShadowTarget{
			"alias": {Provider: "candidate", Model: "shadow-model", Protocol: "openai"},
		},
	}
	p := newTestProxy(t, oldConfig)
	p.reqLog = newRequestLogger(t.TempDir(), 1<<20, 1<<10, 0)
	p.providers["primary"] = &testProv{key: "primary"}
	p.providers["candidate"] = &testProv{key: "candidate"}
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	requestDone := make(chan error, 1)
	go func() {
		resp, err := http.Post(px.URL+"/v1/chat/completions", "application/json", strings.NewReader(
			`{"model":"alias","messages":[{"role":"user","content":"hi"}]}`))
		if err == nil {
			_, err = io.ReadAll(resp.Body)
			resp.Body.Close()
		}
		requestDone <- err
	}()
	select {
	case <-primaryStarted:
		// The request already captured the old runtime + shadow bundle.
	case err := <-requestDone:
		close(releasePrimary)
		t.Fatalf("primary request ended before reaching upstream: %v", err)
	case <-time.After(2 * time.Second):
		close(releasePrimary)
		t.Fatal("primary request did not reach upstream")
	}

	zero := 0.0
	newConfig := &Config{
		Providers: map[string]Provider{
			"primary":   {Provider: "static", OpenAIBaseURL: primaryUpstream.URL},
			"candidate": {Provider: "static", OpenAIBaseURL: shadowUpstream.URL},
		},
		Routes:           oldConfig.Routes,
		Shadow:           oldConfig.Shadow,
		ShadowSampleRate: &zero,
	}
	// Match reload's atomic swap: later requests must see sampling disabled, but
	// this already-admitted request must retain the old sampling bundle.
	p.mu.Lock()
	p.cfg = newConfig
	p.shadow.Store(newShadowRuntime(newConfig))
	p.mu.Unlock()

	close(releasePrimary)
	select {
	case err := <-requestDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("primary request did not finish")
	}
	select {
	case <-shadowHit:
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight request used the reloaded shadow runtime instead of its captured generation")
	}
}

// TestShadowDispatchEmptyModelPassesThrough: a shadow target without an
// explicit model forwards the called model unchanged (regression: the shared
// targetPlan.rewriteModel used to write an empty "model" into the body).
func TestShadowDispatchEmptyModelPassesThrough(t *testing.T) {
	var gotBody atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotBody.Store(string(body))
		w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"candidate": {Provider: "static", OpenAIBaseURL: upstream.URL},
		},
	}
	p := newTestProxy(t, cfg)
	p.reqLog = newRequestLogger(t.TempDir(), 1<<20, 1<<10, 0)
	runtime := p.snapshotRuntime()
	shadowRuntime := p.shadow.Load()

	p.runShadow(
		runtime,
		shadowRuntime,
		"responses",
		"responses",
		"alias",
		"alias",
		ShadowTarget{Provider: "candidate"},
		[]byte(`{"model":"alias","input":[]}`),
		"request-1",
	)
	body, _ := gotBody.Load().(string)
	var decoded map[string]any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("shadow body = %q: %v", body, err)
	}
	if decoded["model"] != "alias" {
		t.Fatalf("shadow rewrote empty model into body: %s", body)
	}
}

// TestReplayTarget parses --to in both --to X and --to=X forms.
func TestReplayTarget(t *testing.T) {
	if got := replayTarget([]string{"id", "--to", "kimi"}); got != "kimi" {
		t.Errorf("--to X = %q want kimi", got)
	}
	if got := replayTarget([]string{"id", "--to=kimi"}); got != "kimi" {
		t.Errorf("--to=X = %q want kimi", got)
	}
	if got := replayTarget([]string{"id"}); got != "" {
		t.Errorf("missing --to = %q want empty", got)
	}
}

// TestShadow_LogsResult: a route with a shadow backend sends the same prompt to
// the shadow provider and logs its result (request_id prefixed "shadow-"), while
// the client only ever sees the primary's response. Shadow record is polled from
// the request log (it's written async by the logger goroutine).
func TestShadow_LogsResult(t *testing.T) {
	var shadowHit atomic.Bool
	primaryUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"primary":true}`))
	}))
	defer primaryUp.Close()
	shadowUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		shadowHit.Store(true)
		// Assert the shadow got the rewritten model.
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), `"model":"glm-shadow"`) {
			t.Errorf("shadow request model not rewritten: %s", string(b))
		}
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"shadow":true}`))
	}))
	defer shadowUp.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"primary": {OpenAIBaseURL: primaryUp.URL, Provider: "static"},
			"shadowp": {OpenAIBaseURL: shadowUp.URL, Provider: "static"},
		},
		Routes: map[string][]RouteTarget{"glm": {{Provider: "primary", Model: "glm"}}},
		Shadow: map[string]ShadowTarget{"glm": {Provider: "shadowp", Model: "glm-shadow"}},
	}
	p, dir, shutdown := newReqLogProxy(t, cfg)
	p.providers["primary"] = &testProv{key: "p"}
	p.providers["shadowp"] = &testProv{key: "s"}
	defer shutdown()
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	resp, err := http.Post(px.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"glm","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	// Client sees ONLY the primary response.
	if !strings.Contains(string(body), `"primary":true`) {
		t.Errorf("client response = %s, want the primary's body", string(body))
	}

	// Wait for the shadow goroutine to hit the shadow upstream + the logger to
	// flush its record, then read it back.
	deadline := time.Now().Add(3 * time.Second)
	var shadowRec *requestLogRecord
	for time.Now().Before(deadline) {
		if shadowHit.Load() {
			for _, r := range allRecords(t, dir) {
				if strings.HasPrefix(r.RequestID, "shadow-") && r.Provider == "shadowp" {
					rr := r
					shadowRec = &rr
					break
				}
			}
		}
		if shadowRec != nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if shadowRec == nil {
		t.Fatalf("shadow record not logged (shadowHit=%v)", shadowHit.Load())
	}
	if shadowRec.UpstreamModel != "glm-shadow" || shadowRec.Status != 200 {
		t.Errorf("shadow record = %+v want model glm-shadow / 200", shadowRec)
	}
	if !strings.Contains(shadowRec.ResponseBody, `"shadow":true`) {
		t.Errorf("shadow response body not captured: %q", shadowRec.ResponseBody)
	}
}
