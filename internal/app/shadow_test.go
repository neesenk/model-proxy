package app

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

	"model-proxy/internal/accounts"
	"model-proxy/internal/observe/requestlog"
	shadowexec "model-proxy/internal/shadow"
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
	wantProvider := "shadow-pool#" + accounts.AccountID("zhipu", AccountCred{APIKey: observedKey})
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
// blocks until released, and the test waits for it to have acquired the permit
// before sending the second request.
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
