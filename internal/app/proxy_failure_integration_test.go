package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// --- UC3: primary 5xx → failover to fallback, and circuit opens so 2nd call skips primary ---

func TestUC_FailoverAndCircuitSkipsOpenProvider(t *testing.T) {
	var primaryHits int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&primaryHits, 1)
		w.WriteHeader(500)
		w.Write([]byte(`{"e":"primary"}`))
	}))
	defer primary.Close()
	fallback, fallbackSeen := newCaptureUpstream(200, `{"ok":true}`)
	defer fallback.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"primary":  {OpenAIBaseURL: primary.URL, Provider: testProviderID},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"m1": {
				{Provider: "primary", Model: "m1", Priority: 1},
				{Provider: "fallback", Model: "m1", Priority: 2},
			},
		},
		Scheduling: Scheduling{CircuitThreshold: 3},
	}
	p := newProxyWithStatic(t, cfg, map[string]string{"primary": "p", "fallback": "f"})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	// Call 1-2: primary 500 → failover to fallback. Primary circuit not yet open (threshold 3).
	for i := 0; i < 2; i++ {
		resp, err := http.Post(px.URL+"/v1/chat/completions", "application/json", stringReader(`{"model":"m1","messages":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 {
			t.Errorf("call %d: status=%d want 200 (fallback)", i, resp.StatusCode)
		}
		resp.Body.Close()
	}

	// Call 3-4: after 3 failures primary circuit opens; primary should NOT be hit again.
	*fallbackSeen = nil
	for i := 0; i < 2; i++ {
		resp, _ := http.Post(px.URL+"/v1/chat/completions", "application/json", stringReader(`{"model":"m1","messages":[]}`))
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if got := atomic.LoadInt32(&primaryHits); got != 3 {
		t.Errorf("primary hits=%d, want exactly 3 (circuit opens AT the threshold, not before)", got)
	}
	if len(*fallbackSeen) != 2 {
		t.Errorf("fallback hits=%d want 2 (open circuit routes straight to fallback)", len(*fallbackSeen))
	}
}

// --- UC4: 401 → refresh → retry same provider → success ---

func TestUC_401RefreshRetrySucceeds(t *testing.T) {
	// First call 401, second call 200 (simulates refresh fixing the token).
	up := scriptedUpstream(
		respScript{status: 401, body: `{"e":"unauthorized"}`},
		respScript{status: 200, body: `{"ok":true}`},
	)
	defer up.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"codex": {OpenAIBaseURL: up.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"gpt-5.5": {{Provider: "codex", Model: "gpt-5.5"}},
		},
	}
	p := newTestProxy(t, cfg)
	rp := &recordingProv{testProv: testProv{key: "k"}}
	p.providers["codex"] = rp
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	resp, err := http.Post(px.URL+"/v1/responses", "application/json", stringReader(`{"model":"gpt-5.5","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("status=%d body=%s want 200 (401 should trigger refresh+retry)", resp.StatusCode, body)
	}
	if got := atomic.LoadInt32(&rp.refreshCalls); got != 1 {
		t.Errorf("Refresh calls=%d want 1 (exactly one refresh after 401)", got)
	}
}

// --- UC5: 401 → refresh → still 401 → failover to next provider ---

func TestUC_401RefreshFailsFailover(t *testing.T) {
	primary := scriptedUpstream(
		respScript{status: 401, body: `{"e":"bad"}`},
		respScript{status: 401, body: `{"e":"bad"}`},
	)
	defer primary.Close()
	fallback, fallbackSeen := newCaptureUpstream(200, `{"ok":true}`)
	defer fallback.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"primary":  {OpenAIBaseURL: primary.URL, Provider: testProviderID},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"m1": {
				{Provider: "primary", Model: "m1", Priority: 1},
				{Provider: "fallback", Model: "m1", Priority: 2},
			},
		},
	}
	p := newTestProxy(t, cfg)
	rp := &recordingProv{testProv: testProv{key: "p"}}
	p.providers["primary"] = rp
	p.providers["fallback"] = &testProv{key: "f"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	resp, err := http.Post(px.URL+"/v1/responses", "application/json", stringReader(`{"model":"m1","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("status=%d want 200 (should failover after 401-refresh fails)", resp.StatusCode)
	}
	if got := atomic.LoadInt32(&rp.refreshCalls); got != 1 {
		t.Errorf("Refresh calls=%d want 1 (401 → refresh → retry before failover)", got)
	}
	if len(*fallbackSeen) != 1 {
		t.Errorf("fallback hits=%d want 1 (failover target after repeated 401)", len(*fallbackSeen))
	}
}

// --- UC6: 429 with Retry-After → provider skipped on next call within the window ---

func TestUC_429RetryAfterSkipsProvider(t *testing.T) {
	var hits int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
		w.Write([]byte(`{"e":"rate"}`))
	}))
	defer up.Close()
	fallback, fallbackSeen := newCaptureUpstream(200, `{"ok":true}`)
	defer fallback.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"primary":  {OpenAIBaseURL: up.URL, Provider: testProviderID},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"m1": {
				{Provider: "primary", Model: "m1", Priority: 1},
				{Provider: "fallback", Model: "m1", Priority: 2},
			},
		},
	}
	p := newProxyWithStatic(t, cfg, map[string]string{"primary": "p", "fallback": "f"})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	// Call 1: primary 429 → failover to fallback.
	resp, _ := http.Post(px.URL+"/v1/responses", "application/json", stringReader(`{"model":"m1","input":[]}`))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Call 2: primary still rate-limited (60s) → straight to fallback, no primary hit.
	*fallbackSeen = nil
	hitsBefore := atomic.LoadInt32(&hits)
	resp, _ = http.Post(px.URL+"/v1/responses", "application/json", stringReader(`{"model":"m1","input":[]}`))
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if got := atomic.LoadInt32(&hits) - hitsBefore; got != 0 {
		t.Errorf("primary hits on call 2 = %d want 0 (Retry-After should skip it)", got)
	}
	if len(*fallbackSeen) != 1 {
		t.Errorf("fallback hits on call 2 = %d want 1", len(*fallbackSeen))
	}
}

// --- UC7: upstream timeout → failover ---

func TestUC_UpstreamTimeoutFailover(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.Write([]byte(`{}`))
	}))
	defer slow.Close()
	fallback, fallbackSeen := newCaptureUpstream(200, `{"ok":true}`)
	defer fallback.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"slow":     {OpenAIBaseURL: slow.URL, Provider: testProviderID},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"m1": {
				{Provider: "slow", Model: "m1", Priority: 1},
				{Provider: "fallback", Model: "m1", Priority: 2},
			},
		},
		Scheduling: Scheduling{UpstreamTimeout: "200ms"},
	}
	p := newProxyWithStatic(t, cfg, map[string]string{"slow": "s", "fallback": "f"})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	start := time.Now()
	resp, err := http.Post(px.URL+"/v1/responses", "application/json", stringReader(`{"model":"m1","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	elapsed := time.Since(start)

	if resp.StatusCode != 200 {
		t.Errorf("status=%d want 200 (should failover after timeout)", resp.StatusCode)
	}
	if elapsed > 1500*time.Millisecond {
		t.Errorf("elapsed=%v want <1.5s (timeout should fail fast, not wait full 2s)", elapsed)
	}
	if len(*fallbackSeen) != 1 {
		t.Errorf("fallback hits=%d want 1", len(*fallbackSeen))
	}
}

// --- UC8: client disconnects mid-stream → proxy stops pulling upstream ---

func TestUC_ClientDisconnectStopsUpstream(t *testing.T) {
	var upstreamRead int64
	var upstreamBroken int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Stream slowly; detect when the proxy closes the connection.
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(200)
		fl, _ := w.(http.Flusher)
		for i := 0; i < 100; i++ {
			if _, err := w.Write([]byte("data: chunk\n\n")); err != nil {
				atomic.StoreInt32(&upstreamBroken, 1)
				return
			}
			atomic.AddInt64(&upstreamRead, 1)
			if fl != nil {
				fl.Flush()
			}
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer up.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"codex": {OpenAIBaseURL: up.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"gpt-5.5": {{Provider: "codex", Model: "gpt-5.5"}},
		},
	}
	p := newProxyWithStatic(t, cfg, map[string]string{"codex": "k"})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	// Client cancels its request almost immediately.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", px.URL+"/v1/responses", stringReader(`{"model":"gpt-5.5","input":[]}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		// context deadline exceeded during the dial/read is fine — the point is
		// the proxy should stop pulling upstream shortly after.
		if resp != nil {
			resp.Body.Close()
		}
	} else {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	// The upstream write error is observable — poll for it instead of
	// sleeping a fixed 300ms (AGENTS.md: no fixed sleeps to prove timing).
	waitUntil(t, "upstream to see the disconnect write error", func() bool {
		return atomic.LoadInt32(&upstreamBroken) == 1
	})
}

// --- UC9: all targets fail → 502 ---

func TestUC_AllTargetsFailReturns502(t *testing.T) {
	a, _ := newCaptureUpstream(500, `{}`)
	defer a.Close()
	b, _ := newCaptureUpstream(500, `{}`)
	defer b.Close()
	cfg := &Config{
		Providers: map[string]Provider{
			"a": {OpenAIBaseURL: a.URL, Provider: testProviderID},
			"b": {OpenAIBaseURL: b.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"m1": {
				{Provider: "a", Model: "m1", Priority: 1},
				{Provider: "b", Model: "m1", Priority: 2},
			},
		},
	}
	p := newProxyWithStatic(t, cfg, map[string]string{"a": "a", "b": "b"})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	resp, err := http.Post(px.URL+"/v1/responses", "application/json", stringReader(`{"model":"m1","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Errorf("status=%d want 502 (all targets failed)", resp.StatusCode)
	}
}
