package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"model-proxy/internal/accounts"
	"model-proxy/internal/observe/requestlog"
	shadowexec "model-proxy/internal/shadow"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---- shadow_test.go ----

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
			"a": {OpenAIBaseURL: aUp.URL, Provider: testProviderID},
			"b": {OpenAIBaseURL: bUp.URL, Provider: testProviderID},
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
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
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
			"primary":   {Provider: testProviderID, OpenAIBaseURL: primaryUpstream.URL},
			"candidate": {Provider: testProviderID, OpenAIBaseURL: shadowUpstream.URL},
		},
		Routes: map[string][]RouteTarget{
			"alias": {{Provider: "primary", Model: "primary-model", Protocol: "openai"}},
		},
		Shadow: map[string]ShadowTarget{
			"alias": {Provider: "candidate", Model: "shadow-model", Protocol: "openai"},
		},
	}
	p := newTestProxy(t, oldConfig)
	p.reqLog = requestlog.New(requestlog.Options{
		Directory: t.TempDir(), MaxFileSize: 1 << 20, MaxBodyBytes: 1 << 10,
	})
	p.reqLogStarted = p.lifecycle.Run(func(<-chan struct{}) { p.reqLog.Run() })
	if !p.reqLogStarted {
		t.Fatal("request logger loop was not started")
	}
	p.providers["primary"] = &testProv{key: "primary"}
	p.providers["candidate"] = &testProv{key: "candidate"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
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
			"primary":   {Provider: testProviderID, OpenAIBaseURL: primaryUpstream.URL},
			"candidate": {Provider: testProviderID, OpenAIBaseURL: shadowUpstream.URL},
		},
		Routes:           oldConfig.Routes,
		Shadow:           oldConfig.Shadow,
		ShadowSampleRate: &zero,
	}
	// Match reload's atomic swap: later requests must see sampling disabled, but
	// this already-admitted request must retain the old sampling bundle.
	p.mu.Lock()
	p.cfg = newConfig
	p.shadow.Store(shadowexec.NewRuntime(shadowexec.Options{
		SampleRate:    newConfig.ShadowSampleRate,
		MaxConcurrent: newConfig.ShadowMaxConcurrent,
		Timeout:       newConfig.Scheduling.Timeout(),
	}))
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
// the old root targetPlan.rewriteModel used to write an empty "model" into the body).
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
			"candidate": {Provider: testProviderID, OpenAIBaseURL: upstream.URL},
		},
	}
	p := newTestProxy(t, cfg)
	p.reqLog = requestlog.New(requestlog.Options{
		Directory: t.TempDir(), MaxFileSize: 1 << 20, MaxBodyBytes: 1 << 10,
	})
	p.reqLogStarted = p.lifecycle.Run(func(<-chan struct{}) { p.reqLog.Run() })
	if !p.reqLogStarted {
		t.Fatal("request logger loop was not started")
	}
	runtime := p.SnapshotRuntime()
	shadowRuntime := p.shadow.Load()

	p.runShadow(
		runtime,
		shadowRuntime,
		nil,
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
			"primary": {OpenAIBaseURL: primaryUp.URL, Provider: testProviderID},
			"shadowp": {OpenAIBaseURL: shadowUp.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{"glm": {{Provider: "primary", Model: "glm"}}},
		Shadow: map[string]ShadowTarget{"glm": {Provider: "shadowp", Model: "glm-shadow"}},
	}
	p, dir, shutdown := newReqLogProxy(t, cfg)
	p.providers["primary"] = &testProv{key: "p"}
	p.providers["shadowp"] = &testProv{key: "s"}
	defer shutdown()
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
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

	// Shadow admission is asynchronous with respect to the client observing
	// the response end (Go 1.27's scheduler reliably lets the client win that
	// race), and Close only waits for ALREADY-admitted tasks — so wait for the
	// shadow request to actually reach the candidate backend first. Close then
	// waits for the admitted task; Shutdown drains every record it enqueued.
	deadline := time.Now().Add(2 * time.Second)
	for !shadowHit.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	p.Close()
	shutdown()
	var shadowRec *requestlog.Record
	for _, r := range allRecords(t, dir) {
		if strings.HasPrefix(r.RequestID, "shadow-") && r.Provider == "shadowp" {
			rr := r
			shadowRec = &rr
			break
		}
	}
	if shadowRec == nil {
		t.Fatalf("shadow record not logged (shadowHit=%v)", shadowHit.Load())
	}
	// A durable shadow record implies the shadow request completed: the
	// candidate backend must have been observed.
	if !shadowHit.Load() {
		t.Fatal("shadow record durable but the candidate backend was never observed")
	}
	if shadowRec.UpstreamModel != "glm-shadow" || shadowRec.Status != 200 {
		t.Errorf("shadow record = %+v want model glm-shadow / 200", shadowRec)
	}
	if !strings.Contains(shadowRec.ResponseBody, `"shadow":true`) {
		t.Errorf("shadow response body not captured: %q", shadowRec.ResponseBody)
	}
}

func TestRunShadowPartialResponseIsLogged(t *testing.T) {
	shadowUpstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		hijacker, ok := writer.(http.Hijacker)
		if !ok {
			t.Error("test server does not support hijacking")
			return
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("hijack shadow response: %v", err)
			return
		}
		_, _ = io.WriteString(
			conn,
			"HTTP/1.1 200 OK\r\n"+
				"Content-Type: application/json\r\n"+
				"X-Request-Id: partial-response\r\n"+
				"Content-Length: 20\r\n"+
				"Connection: close\r\n\r\n"+
				"partial",
		)
		_ = conn.Close()
	}))
	defer shadowUpstream.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"candidate": {Provider: testProviderID, OpenAIBaseURL: shadowUpstream.URL},
		},
	}
	proxy, logDir, shutdownLogger := newReqLogProxy(t, cfg)
	proxy.providers["candidate"] = &testProv{key: "candidate"}
	proxy.runShadow(
		proxy.SnapshotRuntime(),
		proxy.shadow.Load(),
		nil,
		"openai",
		"openai",
		"alias",
		"alias",
		ShadowTarget{Provider: "candidate", Model: "shadow-model", Protocol: "openai"},
		[]byte(`{"model":"alias","messages":[]}`),
		"partial-1",
	)
	shutdownLogger()

	records := allRecords(t, logDir)
	if len(records) != 1 {
		t.Fatalf("request-log records = %d, want one partial Shadow record: %+v", len(records), records)
	}
	record := records[0]
	if record.RequestID != "shadow-partial-1" || !record.Shadow ||
		record.Provider != "candidate" || record.UpstreamModel != "shadow-model" ||
		record.Status != http.StatusOK {
		t.Errorf("partial Shadow identity/status = %+v", record)
	}
	if record.ResponseBody != "partial" || record.ResponseSize != 7 {
		t.Errorf("partial Shadow body/size = %q/%d, want partial/7", record.ResponseBody, record.ResponseSize)
	}
	if record.ResponseHeaders != `{"content-type":"application/json","x-request-id":"partial-response"}` {
		t.Errorf("partial Shadow response headers = %s", record.ResponseHeaders)
	}
	if !strings.Contains(record.RequestBody, `"model":"shadow-model"`) {
		t.Errorf("partial Shadow request body = %s, want rewritten model", record.RequestBody)
	}
}

// TestShadow_PooledCrossProtocolPreservesVirtualIdentity exercises the combined
// Shadow boundary: an OpenAI Chat primary response is returned unchanged while a
// pooled shadow parent is resolved to one virtual account and converted to its
// declared Anthropic wire protocol. The recorded shadow provider must retain the
// selected virtual identity rather than collapsing back to the pool parent.
func TestShadow_PooledCrossProtocolPreservesVirtualIdentity(t *testing.T) {
	poolHome := t.TempDir()
	setPoolHome(t, poolHome)
	writePoolFile(t, "shadow-pool", "zhipu", "shadow-key-a", "shadow-key-b")

	const primaryBody = `{"id":"primary-marker","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"primary only"},"finish_reason":"stop"}]}`
	const shadowBody = `{"id":"shadow-marker","type":"message","role":"assistant","model":"shadow-model","stop_reason":"end_turn","content":[{"type":"text","text":"shadow only"}],"usage":{"input_tokens":3,"output_tokens":2}}`
	type shadowRequest struct {
		path          string
		authorization string
		body          []byte
	}
	shadowReceived := make(chan shadowRequest, 1)
	shadowUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read shadow request: %v", err)
			return
		}
		shadowReceived <- shadowRequest{
			path:          r.URL.Path,
			authorization: r.Header.Get("Authorization"),
			body:          body,
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(shadowBody))
	}))
	defer shadowUpstream.Close()

	primaryUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(primaryBody))
	}))
	defer primaryUpstream.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"primary":     {OpenAIBaseURL: primaryUpstream.URL, Provider: testProviderID},
			"shadow-pool": {AnthropicBaseURL: shadowUpstream.URL, Provider: "zhipu"},
		},
		Routes: map[string][]RouteTarget{
			"alias": {{Provider: "primary", Model: "primary-model", Protocol: "openai"}},
		},
		Shadow: map[string]ShadowTarget{
			"alias": {Provider: "shadow-pool", Model: "shadow-model", Protocol: "anthropic"},
		},
	}
	p, logDir, shutdownLogger := newReqLogProxy(t, cfg)
	defer shutdownLogger()
	p.providers["primary"] = &testProv{key: "primary-key"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	resp, err := http.Post(px.URL+"/v1/chat/completions", "application/json", strings.NewReader(
		`{"model":"alias","messages":[{"role":"user","content":"shadow user"}],"max_tokens":37}`))
	if err != nil {
		t.Fatal(err)
	}
	clientBody, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("client status = %d, want 200; body=%s", resp.StatusCode, clientBody)
	}
	if got := string(clientBody); got != primaryBody {
		t.Fatalf("client body = %s, want exact primary response %s", got, primaryBody)
	}
	if strings.Contains(string(clientBody), "shadow") {
		t.Fatalf("client response leaked shadow output: %s", clientBody)
	}

	var observed shadowRequest
	select {
	case observed = <-shadowReceived:
	case <-time.After(3 * time.Second):
		t.Fatal("shadow upstream did not receive the converted request")
	}
	if observed.path != "/v1/messages" {
		t.Errorf("shadow path = %q, want /v1/messages", observed.path)
	}
	const bearerPrefix = "Bearer "
	if !strings.HasPrefix(observed.authorization, bearerPrefix) {
		t.Fatalf("shadow Authorization = %q, want exact pooled Bearer key", observed.authorization)
	}
	observedKey := strings.TrimPrefix(observed.authorization, bearerPrefix)
	if observedKey != "shadow-key-a" && observedKey != "shadow-key-b" {
		t.Fatalf("shadow pooled key = %q, want one configured pool key", observedKey)
	}
	var anthropic struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Messages  []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(observed.body, &anthropic); err != nil {
		t.Fatalf("shadow request is not Anthropic JSON: %v; body=%s", err, observed.body)
	}
	if anthropic.Model != "shadow-model" {
		t.Errorf("shadow model = %q, want shadow-model", anthropic.Model)
	}
	if anthropic.MaxTokens != 37 {
		t.Errorf("shadow max_tokens = %d, want 37", anthropic.MaxTokens)
	}
	if len(anthropic.Messages) != 1 || anthropic.Messages[0].Role != "user" || len(anthropic.Messages[0].Content) != 1 || anthropic.Messages[0].Content[0].Type != "text" || anthropic.Messages[0].Content[0].Text != "shadow user" {
		t.Errorf("shadow Anthropic user message = %+v, want one user text block shadow user", anthropic.Messages)
	}

	// The upstream handler proves the asynchronous request reached the candidate.
	// Close waits for the fire-and-forget runner, then the logger shutdown drains
	// its record before the disk query.
	wantProvider := "shadow-pool#" + accounts.AccountID("zhipu", accounts.Credentials{APIKey: observedKey})
	p.Close()
	shutdownLogger()
	var shadowRecord *requestlog.Record
	for _, record := range allRecords(t, logDir) {
		if strings.HasPrefix(record.RequestID, "shadow-") {
			r := record
			shadowRecord = &r
			break
		}
	}
	if shadowRecord == nil {
		t.Fatal("shadow request log record was not durable after Proxy.Close and logger Shutdown")
	}
	if shadowRecord.Provider != wantProvider {
		t.Errorf("shadow log provider = %q, want selected virtual %q", shadowRecord.Provider, wantProvider)
	}
	if shadowRecord.UpstreamModel != "shadow-model" {
		t.Errorf("shadow log model = %q, want shadow-model", shadowRecord.UpstreamModel)
	}
	if shadowRecord.Status != http.StatusOK {
		t.Errorf("shadow log status = %d, want 200", shadowRecord.Status)
	}
	if shadowRecord.RequestBody != string(observed.body) {
		t.Errorf("shadow log request body = %s, want exact upstream request %s", shadowRecord.RequestBody, observed.body)
	}
	if shadowRecord.ResponseBody != shadowBody {
		t.Errorf("shadow log response body = %s, want exact shadow response %s", shadowRecord.ResponseBody, shadowBody)
	}
}

// TestShadow_ConcurrencyGateSaturatesAndDrops (P1): with shadow_max_concurrent
// at 1 and an in-flight shadow request, the SECOND request's shadow must be
// DROPPED by the gate (not queued behind the permit) while both primaries
// answer normally. Ordering is deterministic: the first shadow's upstream
// blocks until released, the test waits for it to have acquired the permit
// before sending the second request, and the runtime's Dropped counter is the
// barrier proving the second dispatch ran while the gate was still saturated
// (post() returning does not imply the post-commit dispatch has run).
func TestShadow_ConcurrencyGateSaturatesAndDrops(t *testing.T) {
	acquired := make(chan struct{}, 1)
	release := make(chan struct{})
	var shadowHits atomic.Int32
	primaryUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"primary":true}`))
	}))
	defer primaryUp.Close()
	shadowUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if shadowHits.Add(1) == 1 {
			acquired <- struct{}{} // the permit holder signals before blocking
			<-release
		}
		w.Write([]byte(`{"shadow":true}`))
	}))
	defer shadowUp.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"primary": {OpenAIBaseURL: primaryUp.URL, Provider: testProviderID},
			"shadowp": {OpenAIBaseURL: shadowUp.URL, Provider: testProviderID},
		},
		Routes:              map[string][]RouteTarget{"glm": {{Provider: "primary", Model: "glm"}}},
		Shadow:              map[string]ShadowTarget{"glm": {Provider: "shadowp", Model: "glm-shadow"}},
		ShadowSampleRate:    ptrFloat(1.0),
		ShadowMaxConcurrent: 1,
	}
	p, dir, shutdown := newReqLogProxy(t, cfg)
	p.providers["primary"] = &testProv{key: "p"}
	p.providers["shadowp"] = &testProv{key: "s"}
	defer shutdown()
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	// Request 1: primary answers; its shadow acquires the only permit and
	// parks inside the shadow upstream.
	if code, body := post(t, px.URL+"/v1/responses", `{"model":"glm","input":[]}`); code != 200 || !strings.Contains(body, `"primary":true`) {
		t.Fatalf("request 1: status=%d body=%s, want primary 200", code, body)
	}
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("first shadow never acquired the permit")
	}

	// Request 2 while the gate is saturated: primary still answers 200 and
	// the shadow is DROPPED — the candidate upstream is not hit again.
	if code, body := post(t, px.URL+"/v1/responses", `{"model":"glm","input":[]}`); code != 200 || !strings.Contains(body, `"primary":true`) {
		t.Fatalf("request 2 (gate saturated): status=%d body=%s, want primary 200", code, body)
	}
	// post() returning does NOT imply the post-commit shadow dispatch has run:
	// dispatchShadowAfterCommit executes in the request goroutine after the
	// response reaches the client. Wait on the runtime's drop counter so the
	// rejection provably happened while the gate was saturated, not by timing
	// luck (a late dispatch after close(release) would be legally admitted).
	waitUntil(t, "second shadow dropped by gate", func() bool {
		return p.shadow.Load().Dropped() >= 1
	})
	if got := shadowHits.Load(); got != 1 {
		t.Fatalf("shadow upstream hits while saturated = %d, want 1 (second shadow must be dropped, not queued)", got)
	}

	// Release the parked shadow; it completes, exactly ONE shadow record
	// lands in the request log, and nothing else fires.
	close(release)
	waitUntil(t, "parked shadow completes", func() bool {
		for _, r := range allRecords(t, dir) {
			if r.Shadow && r.Provider == "shadowp" {
				return true
			}
		}
		return false
	})
	if got := shadowHits.Load(); got != 1 {
		t.Errorf("shadow upstream hits after release = %d, want 1 (dropped shadow stayed dropped)", got)
	}
	shadowRecords := 0
	for _, r := range allRecords(t, dir) {
		if r.Shadow {
			shadowRecords++
		}
	}
	if shadowRecords != 1 {
		t.Errorf("shadow records = %d, want exactly 1 (the dropped shadow must not be recorded)", shadowRecords)
	}
}

func ptrFloat(v float64) *float64 { return &v }

// ---- shadow_regression_test.go ----

// TestShouldShadow: rate=0 → false, rate>=1 → true, rate between → probabilistic.
func TestShouldShadow(t *testing.T) {
	cfg := &Config{
		Providers: map[string]Provider{"z": {OpenAIBaseURL: "https://x", Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "z", Model: "m"}}},
	}
	// rate >= 1 → always true.
	p := newTestProxy(t, cfg)
	one := 1.0
	p.shadow.Store(shadowexec.NewRuntime(shadowexec.Options{SampleRate: &one, MaxConcurrent: 1}))
	if !p.shadow.Load().ShouldSample() {
		t.Error("rate=1.0 should return true")
	}
	// rate <= 0 → always false.
	zero := 0.0
	p.shadow.Store(shadowexec.NewRuntime(shadowexec.Options{SampleRate: &zero, MaxConcurrent: 1}))
	if p.shadow.Load().ShouldSample() {
		t.Error("rate=0 should return false")
	}
	// nil runtime → false.
	p.shadow.Store(nil)
	if p.shadow.Load().ShouldSample() {
		t.Error("nil shadow runtime should return false")
	}
}

// TestReload_ShadowDisabledStopsFiring (regression #6): the shadow sample rate /
// concurrency cap / client must update on reload. Disabling shadow via
// shadow_sample_rate: 0 + reload must stop firing shadow requests immediately —
// pre-fix the sample rate was cached at startup, so paid shadow requests kept
// firing until restart.
func TestReload_ShadowDisabledStopsFiring(t *testing.T) {
	useStaticProviderPools(t, "main", "cand")
	var candHits atomic.Int64
	mainUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer mainUp.Close()
	candUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		candHits.Add(1)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer candUp.Close()

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	reqDir := filepath.Join(t.TempDir(), "requests")
	base := fmt.Sprintf("listen: 127.0.0.1:0\n"+
		"providers:\n"+
		"  main:\n    openai_base_url: %s\n    provider_id: static\n"+
		"  cand:\n    openai_base_url: %s\n    provider_id: static\n"+
		"routes:\n  m:\n    - {provider: main, model: m}\n"+
		"shadow:\n  m:\n    provider: cand\n    model: m\n"+
		"request_log:\n  enabled: true\n  dir: %s\n", mainUp.URL, candUp.URL, reqDir)
	write := func(extra string) {
		if err := os.WriteFile(cfgPath, []byte(base+extra), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write("shadow_sample_rate: 1.0\n")
	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	p := newTestProxy(t, cfg)
	p.initRequestLog(cfg.RequestLog) // shadow only fires when reqLog is active
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	send := func() {
		resp, err := http.Post(px.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"m","input":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	// 1) shadow enabled → the candidate IS hit (fire-and-forget, so poll).
	send()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && candHits.Load() < 1 {
		time.Sleep(5 * time.Millisecond)
	}
	if first := candHits.Load(); first != 1 {
		t.Fatalf("shadow should have fired once after first send; candHits=%d", first)
	}

	// 2) disable shadow via reload (sample_rate: 0).
	write("shadow_sample_rate: 0.0\n")
	if err := p.Reload(cfgPath); err != nil {
		t.Fatalf("reload: %v", err)
	}
	send()
	// Sampling is checked synchronously before lifecycle admission. The captured
	// post-reload runtime must therefore reject the request deterministically.
	if p.shadow.Load().ShouldSample() {
		t.Fatal("post-reload shadow runtime still samples at rate 0")
	}
	if got := candHits.Load(); got != 1 {
		t.Errorf("after disabling shadow via reload, candHits=%d, want 1 (shadow kept firing — sample rate not reload-aware)", got)
	}
}

// TestShadow_PooledProvider (regression for the unified resolver, #10): a shadow
// target that names a POOLED parent must still be sampled. Pre-fix runShadow did
// provs[shadow.Provider] (nil for a parent) → "provider not available" → shadow
// silently stopped the moment a second account was added. After the fix the
// resolver picks one of the parent's virtuals.
func TestShadow_PooledProvider(t *testing.T) {
	dir := t.TempDir()
	setPoolHome(t, dir)
	writePoolFile(t, "zhipu-shadow", "zhipu", "SA", "SB")

	var shadowHits atomic.Int64
	mainUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer mainUp.Close()
	shadowUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		shadowHits.Add(1)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer shadowUp.Close()

	cfg := &Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]Provider{
			"main":         {OpenAIBaseURL: mainUp.URL, Provider: testProviderID},
			"zhipu-shadow": {OpenAIBaseURL: shadowUp.URL, Provider: "zhipu"},
		},
		Routes: map[string][]RouteTarget{"m": {{Provider: "main", Model: "m"}}},
		Shadow: map[string]ShadowTarget{"m": {Provider: "zhipu-shadow", Model: "glm"}},
	}
	p := newTestProxy(t, cfg)
	p.initRequestLog(RequestLogConfig{Enabled: true, Dir: filepath.Join(t.TempDir(), "requests")})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	resp, err := http.Post(px.URL+"/v1/responses", "application/json", strings.NewReader(`{"model":"m","input":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Shadow is fire-and-forget; poll for the hit.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && shadowHits.Load() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if shadowHits.Load() == 0 {
		t.Fatal("pooled shadow target (zhipu-shadow) never sampled — resolver did not resolve it to a virtual")
	}
}

// TestShadow_ConvertFail_Closed (regression #C): when a cross-protocol shadow
// request's conversion fails, the shadow must be SKIPPED — not sent with the
// unconverted body (which would ship an Anthropic body to an OpenAI endpoint or
// vice versa). Pre-fix runShadow logged the error and forwarded the raw body.
func TestShadow_ConvertFail_Closed(t *testing.T) {
	var shadowHits atomic.Int64
	mainUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`)) // primary 2xx so the shadow dispatch fires
	}))
	defer mainUp.Close()
	shadowUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		shadowHits.Add(1)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer shadowUp.Close()

	cfg := &Config{
		Listen: "127.0.0.1:1",
		Providers: map[string]Provider{
			"main":        {OpenAIBaseURL: mainUp.URL, Provider: testProviderID},
			"shadow-prov": {AnthropicBaseURL: shadowUp.URL, Provider: testProviderID}, // cross-proto (anthropic) shadow
		},
		Routes: map[string][]RouteTarget{"m": {{Provider: "main", Model: "m"}}},
		Shadow: map[string]ShadowTarget{"m": {Provider: "shadow-prov", Model: "sm", Protocol: "anthropic"}},
	}
	p := newTestProxy(t, cfg)
	p.providers["main"] = &testProv{key: "main"}
	p.providers["shadow-prov"] = &testProv{key: "shadow-prov"}
	p.initRequestLog(RequestLogConfig{Enabled: true, Dir: filepath.Join(t.TempDir(), "requests")})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	// extractModel returns "m" (fast path reads 3 tokens), but the full JSON is
	// malformed → the shadow's openai→anthropic convertRequest fails.
	resp, err := http.Post(px.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"m","input":[BAD`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	// Close waits for the admitted detached task, so the zero-hit assertion has
	// no asynchronous timing window.
	p.Close()
	if got := shadowHits.Load(); got != 0 {
		t.Errorf("shadow backend hit %d time(s) with an unconverted body after convert failure (fail-open); want 0", got)
	}
}

// TestRunShadow_NilRuntimeConfig: a zero-value RuntimeSnapshot (a future call
// site forgetting to populate targetexec.Attempt.Runtime) must log + return instead
// of panicking on runtime.cfg deep in runShadow.
func TestRunShadow_NilRuntimeConfig(t *testing.T) {
	p := newTestProxy(t, &Config{Providers: map[string]Provider{}})
	var buf bytes.Buffer
	log.SetOutput(&buf)
	defer log.SetOutput(os.Stderr)
	p.runShadow(RuntimeSnapshot{}, nil, nil, "anthropic", "anthropic", "m", "g",
		ShadowTarget{Provider: "p", Model: "m"}, []byte(`{}`), "rid")
	if !strings.Contains(buf.String(), "runtime snapshot has no config") {
		t.Fatalf("expected the nil-cfg guard log, got %q", buf.String())
	}
}

// TestHandleShadowReport_API: the /api/shadow-report endpoint returns
// enabled=false when request_log is off. The CLI-side rendering of this
// endpoint is covered by internal/cli/shadow_report_render_test.go against
// the production renderer.
func TestHandleShadowReport_API(t *testing.T) {
	// Off → enabled=false.
	w := NewWebServer(newTestProxy(t, &Config{
		Providers: map[string]Provider{"z": {OpenAIBaseURL: "https://x", Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"glm": {{Provider: "z", Model: "glm"}}},
	}), "test-config.yaml")
	mux := http.NewServeMux()
	w.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/shadow-report", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Errorf("shadow-report off: code=%d body=%s", rec.Code, rec.Body.String())
	}
}

// ---- shadow_shutdown_test.go ----

// TestCloseCancelsInFlightShadowRequest: shutdown must cancel an in-flight
// shadow dispatch instead of waiting out the shadow client's full upstream
// timeout. The supervisor SIGKILLs the worker 10s after SIGTERM — an
// unbounded WaitBeforeLogDrain would drop every final flush that follows it
// (request-log drain, quota persist, stats flush). Red line: background tasks
// need owner, stop, wait AND a stop signal the task actually observes.
func TestCloseCancelsInFlightShadowRequest(t *testing.T) {
	release := make(chan struct{})
	shadowHit := make(chan struct{}, 1)
	shadowUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case shadowHit <- struct{}{}:
		default:
		}
		<-release
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer shadowUp.Close()
	// LIFO: release the parked handler BEFORE shadowUp.Close() waits it out.
	defer close(release)
	primaryUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer primaryUp.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"primary":   {OpenAIBaseURL: primaryUp.URL, Provider: testProviderID},
			"candidate": {OpenAIBaseURL: shadowUp.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"alias": {{Provider: "primary", Model: "primary-model", Protocol: "openai"}},
		},
		Shadow: map[string]ShadowTarget{
			"alias": {Provider: "candidate", Model: "shadow-model", Protocol: "openai"},
		},
		// The shadow client's only bound absent cancellation: Close must not
		// wait anywhere near this out.
		Scheduling: Scheduling{UpstreamTimeout: "30s"},
	}
	p := newTestProxy(t, cfg)
	p.reqLog = requestlog.New(requestlog.Options{
		Directory: t.TempDir(), MaxFileSize: 1 << 20, MaxBodyBytes: 1 << 10,
	})
	p.reqLogStarted = p.lifecycle.Run(func(<-chan struct{}) { p.reqLog.Run() })
	if !p.reqLogStarted {
		t.Fatal("request logger loop was not started")
	}

	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()
	resp := postForStatus(t, px.URL+"/v1/chat/completions", `{"model":"alias","messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("primary status = %d, want 200", resp.StatusCode)
	}
	// The shadow dispatch has reached its (hanging) upstream — the request is
	// now parked inside the shadow client for as long as we hold `release`.
	select {
	case <-shadowHit:
	case <-time.After(2 * time.Second):
		t.Fatal("shadow upstream was never called")
	}

	closed := make(chan struct{})
	go func() {
		p.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("Proxy.Close blocked on an in-flight shadow request — no stop signal reaches the shadow client")
	}
}
