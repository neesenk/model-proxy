package app

import (
	"context"
	"encoding/json"
	"io"
	configdomain "model-proxy/internal/config"
	"model-proxy/internal/observe/counters"
	"model-proxy/internal/targetexec"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---- client_cancel_test.go ----

// Regression: a client disconnect while waiting for upstream response HEADERS
// used to be classified as a hard upstream failure. One cancelled request then
// burned the whole failover chain (every remaining target failed instantly on
// the dead request context, each earning a circuit tick), so three user
// interrupts could open every circuit on the route. Post-fix: the loop stops
// at the first cancelled target, nothing is recorded, and the live monitor
// closes the request with a 499 end event like the cooldown-wait cancel path.
func TestUC_ClientCancelDuringHeadersStopsFailoverAndKeepsCircuitClosed(t *testing.T) {
	var blockedHits int32
	// Go's http1 transport does not promptly close an outbound connection that
	// carried a request BODY when the round trip is canceled, so the upstream
	// handler's r.Context() never fires here. Release the handlers explicitly
	// instead of relying on cancel propagation (in production the executor's
	// upstream timeout bounds the lingering connection).
	release := make(chan struct{})
	var blockedHitsDone int32
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&blockedHits, 1)
		// Never answer: hang until the proxy drops the outbound request.
		select {
		case <-r.Context().Done():
		case <-release:
		}
		atomic.AddInt32(&blockedHitsDone, 1)
	}))
	defer blocked.Close()
	fallback, fallbackSeen := newCaptureUpstream(200, `{"ok":true}`)
	defer fallback.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"blocked":  {OpenAIBaseURL: blocked.URL, Provider: testProviderID},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"m1": {
				{Provider: "blocked", Model: "m1", Priority: 1},
				{Provider: "fallback", Model: "m1", Priority: 2},
			},
			"m2": {{Provider: "fallback", Model: "m2"}},
		},
		// Threshold 3: pre-fix, three cancelled requests recorded 3 failures
		// per provider → both circuits open. retry_wait 0 keeps the terminal
		// contrast immediate (no cooldown-wait rounds).
		Scheduling: configdomain.Scheduling{CircuitThreshold: 3, RetryWait: "0"},
	}
	p := newProxyWithStatic(t, cfg, map[string]string{"blocked": "b", "fallback": "f"})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	cancelledEnds := func() int {
		n := 0
		for _, e := range p.events.Snapshot() {
			if e.Type == "end" && e.Status == 499 {
				n++
			}
		}
		return n
	}

	for round := 1; round <= 3; round++ {
		ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, px.URL+"/v1/chat/completions", stringReader(`{"model":"m1","messages":[]}`))
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		cancel()
		// Deterministic hand-off: the handler publishes the 499 end event right
		// before returning, so once it appears this round is fully finished.
		deadline := time.Now().Add(2 * time.Second)
		for cancelledEnds() < round && time.Now().Before(deadline) {
			time.Sleep(5 * time.Millisecond)
		}
		if got := cancelledEnds(); got != round {
			t.Fatalf("round %d: 499 end events=%d — cancelled request was not classified client-gone", round, got)
		}
	}

	if hits := atomic.LoadInt32(&blockedHits); hits != 3 {
		t.Errorf("blocked upstream hits=%d want 3", hits)
	}
	// Unblock the hung upstream handlers before server teardown.
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&blockedHitsDone) < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&blockedHitsDone); got != 3 {
		t.Fatalf("blocked upstream handlers exited=%d, want 3", got)
	}
	if burned := len(*fallbackSeen); burned != 0 {
		t.Errorf("failover burned %d fallback calls on a dead request context", burned)
	}

	// No failure was ever recorded, so fallback must still serve immediately.
	// Pre-fix its circuit was open after round 3 → 502.
	resp, err := http.Post(px.URL+"/v1/chat/completions", "application/json", stringReader(`{"model":"m2","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Errorf("status=%d want 200 (cancellations must not open circuits), body=%s", resp.StatusCode, body)
	}
}

// ---- context_retry_test.go ----

// TestIsContextOverflow: the 4xx body classifier — hits the context-overflow
// shapes of the known backends, never an ordinary client error.
func TestIsContextOverflow(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"openai context_length_exceeded", 400,
			`{"error":{"message":"This model's maximum context length is 128000 tokens. However, your messages resulted in 200001 tokens. Please reduce the length of the messages.","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`, true},
		{"deepseek maximum context length", 400,
			`{"error":{"message":"This model's maximum context length is 65536 tokens. However, you requested 100000 tokens (100000 in the messages, 0 in the completion). Please reduce the length of the messages.","type":"invalid_request_error","param":null,"code":"invalid_request_error"}}`, true},
		{"anthropic prompt is too long", 400,
			`{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 213432 tokens > 200000 maximum"}}`, true},
		{"zhipu-style context length", 400,
			`{"error":{"code":"1308","message":"prompt tokens exceed the model context length limit"}}`, true},
		{"too many tokens", 400,
			`{"error":{"message":"Request contains too many tokens: 300000"}}`, true},
		{"case-insensitive", 400,
			`{"error":{"code":"CONTEXT_LENGTH_EXCEEDED"}}`, true},
		{"invalid api key", 401,
			`{"error":{"message":"Incorrect API key provided","type":"invalid_request_error"}}`, false},
		{"missing model field", 400,
			`{"error":{"message":"Missing required field: model"}}`, false},
		{"model not found", 400,
			`{"error":{"message":"model 'foo' does not exist"}}`, false},
		{"2xx never an overflow", 200,
			`{"error":{"code":"context_length_exceeded"}}`, false},
		{"5xx never an overflow", 500,
			`context_length_exceeded`, false},
		{"empty body", 400, ``, false},
	}
	for _, c := range cases {
		if got := targetexec.IsContextOverflow(c.status, []byte(c.body)); got != c.want {
			t.Errorf("%s: isContextOverflow(%d, body) = %v, want %v", c.name, c.status, got, c.want)
		}
	}
}

// overflowServer returns an httptest.Server that always answers 400 with a
// context-overflow error body carrying the given marker text.
func overflowServer(body string, hits *int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		(*hits)++
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(body))
	}))
}

// TestForward_ContextOverflowRetry: a small in-route request (estimate fits, so
// proactive routing keeps it) whose upstream still answers a context-overflow
// 400 is retried ONCE on the strictly-larger-context cross-route target; the
// client gets that target's 2xx. The overflow attempt counts as a failover,
// not a served request.
func TestForward_ContextOverflowRetry(t *testing.T) {
	var smallHits, bigHits int
	smallUp := overflowServer(`{"error":{"code":"context_length_exceeded","message":"maximum context length is 8000 tokens"}}`, &smallHits)
	defer smallUp.Close()
	bigUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bigHits++
		w.Write([]byte(`{"from":"big"}`))
	}))
	defer bigUp.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"small-prov": {OpenAIBaseURL: smallUp.URL, Provider: testProviderID},
			"big-prov":   {OpenAIBaseURL: bigUp.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"glm":      {{Provider: "small-prov", Model: "small"}},
			"glm-long": {{Provider: "big-prov", Model: "big"}},
		},
	}
	p := newTestProxy(t, cfg)
	p.providers["small-prov"] = &testProv{key: "s"}
	p.providers["big-prov"] = &testProv{key: "b"}
	p.catalog = testCatalog(map[string]struct {
		Context int64
		Input   []string
	}{"small": {Context: 8000}, "big": {Context: 128000}})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/responses", strings.NewReader(`{"model":"glm","input":"hi"}`))
	resp, err := http.DefaultClient.Do(req.WithContext(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 || string(got) != `{"from":"big"}` {
		t.Errorf("status=%d body=%s, want big-prov's 200 response", resp.StatusCode, got)
	}
	if smallHits != 1 || bigHits != 1 {
		t.Errorf("small=%d big=%d, want one attempt each", smallHits, bigHits)
	}
	// The client can finish reading a Content-Length body before the server
	// handler goroutine runs the post-copy Committed effect that records
	// metrics — wait for it instead of asserting on a racy snapshot.
	m := awaitCommitMetrics(t, p, counters.PMKey{Provider: "big-prov", Model: "big"})
	if s := m[counters.PMKey{Provider: "small-prov", Model: "small"}]; s.Failovers != 1 || s.Requests != 0 || s.Failures != 0 {
		t.Errorf("small metrics = %+v, want failovers=1 requests=0 failures=0", s)
	}
}

// TestForward_ContextOverflowRetry_RespectsCapability (regression #7): the
// context-overflow retry must RE-CHECK image/tools capability, not only look for
// a larger context window. An image request that overflows a small vision model
// must NOT be retried onto a larger-context model that lacks image support
// (that would just fail again). With no capable larger model, the 400 commits.
func TestForward_ContextOverflowRetry_RespectsCapability(t *testing.T) {
	var smallHits, bigTextHits int
	smallUp := overflowServer(`{"error":{"code":"context_length_exceeded","message":"maximum context length is 8000 tokens"}}`, &smallHits)
	defer smallUp.Close()
	bigTextUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bigTextHits++
		w.Write([]byte(`{"from":"big-text"}`))
	}))
	defer bigTextUp.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"small-prov": {OpenAIBaseURL: smallUp.URL, Provider: testProviderID},
			"big-text":   {OpenAIBaseURL: bigTextUp.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"vision": {{Provider: "small-prov", Model: "small-vision"}},
			"text":   {{Provider: "big-text", Model: "big-text"}},
		},
	}
	p := newTestProxy(t, cfg)
	p.providers["small-prov"] = &testProv{key: "s"}
	p.providers["big-text"] = &testProv{key: "bt"}
	p.catalog = testCatalog(map[string]struct {
		Context int64
		Input   []string
	}{
		"small-vision": {Context: 8000, Input: []string{"text", "image"}},
		"big-text":     {Context: 128000, Input: []string{"text"}}, // larger ctx, NO image
	})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	// An IMAGE request large enough to overflow small-vision's 8000 window. The
	// only larger-context model is big-text, which lacks image support — the
	// retry must NOT send an image request to a text-only model.
	body := `{"model":"vision","input":[{"type":"image","content":"` + strings.Repeat("qwxz!", 8000) + `"}]}`
	req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/responses", strings.NewReader(body))
	resp, err := http.DefaultClient.Do(req.WithContext(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if bigTextHits != 0 {
		t.Errorf("big-text (no image support) hit %d time(s) by the overflow retry — capability was not re-checked", bigTextHits)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status=%d, want 400 (overflow commits when no larger model supports the request's capability)", resp.StatusCode)
	}
}

// TestForward_ContextOverflowRetry_OnlyOnce: the retry is one-shot — when the
// retried (larger) target also overflows, its 400 commits to the client
// byte-complete rather than triggering a second retarget to an even larger
// model.
func TestForward_ContextOverflowRetry_OnlyOnce(t *testing.T) {
	var smallHits, bigHits, hugeHits int
	smallUp := overflowServer(`{"err":"small-overflow","code":"context_length_exceeded"}`, &smallHits)
	defer smallUp.Close()
	bigBody := `{"err":"big-overflow","code":"context_length_exceeded"}`
	bigUp := overflowServer(bigBody, &bigHits)
	defer bigUp.Close()
	hugeUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hugeHits++
		w.Write([]byte(`{"from":"huge"}`))
	}))
	defer hugeUp.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"small-prov": {OpenAIBaseURL: smallUp.URL, Provider: testProviderID},
			"big-prov":   {OpenAIBaseURL: bigUp.URL, Provider: testProviderID},
			"huge-prov":  {OpenAIBaseURL: hugeUp.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"glm":       {{Provider: "small-prov", Model: "small"}},
			"glm-long":  {{Provider: "big-prov", Model: "big", Priority: 1}},
			"glm-xlong": {{Provider: "huge-prov", Model: "huge", Priority: 2}},
		},
	}
	p := newTestProxy(t, cfg)
	p.providers["small-prov"] = &testProv{key: "s"}
	p.providers["big-prov"] = &testProv{key: "b"}
	p.providers["huge-prov"] = &testProv{key: "h"}
	p.catalog = testCatalog(map[string]struct {
		Context int64
		Input   []string
	}{"small": {Context: 8000}, "big": {Context: 128000}, "huge": {Context: 1000000}})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/responses", strings.NewReader(`{"model":"glm","input":"hi"}`))
	resp, err := http.DefaultClient.Do(req.WithContext(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// small overflows → retry picks big (priority 1 over huge) → big overflows
	// → retry spent → big's 400 commits byte-complete.
	if resp.StatusCode != 400 || string(got) != bigBody {
		t.Errorf("status=%d body=%s, want big-prov's 400 %q byte-complete", resp.StatusCode, got, bigBody)
	}
	if smallHits != 1 || bigHits != 1 {
		t.Errorf("small=%d big=%d, want one attempt each", smallHits, bigHits)
	}
	if hugeHits != 0 {
		t.Errorf("huge=%d, want 0 — the context-overflow retry is one-shot", hugeHits)
	}
}

// TestForward_ContextOverflowRetry_Ordinary400: a 400 that is NOT a context
// overflow (bad API key) commits unchanged — no peek-triggered retarget.
func TestForward_ContextOverflowRetry_Ordinary400(t *testing.T) {
	var smallHits, bigHits int
	badKey := `{"error":{"message":"Incorrect API key provided","type":"invalid_request_error"}}`
	smallUp := overflowServer(badKey, &smallHits)
	defer smallUp.Close()
	bigUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bigHits++
		w.Write([]byte(`{"from":"big"}`))
	}))
	defer bigUp.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"small-prov": {OpenAIBaseURL: smallUp.URL, Provider: testProviderID},
			"big-prov":   {OpenAIBaseURL: bigUp.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"glm":      {{Provider: "small-prov", Model: "small"}},
			"glm-long": {{Provider: "big-prov", Model: "big"}},
		},
	}
	p := newTestProxy(t, cfg)
	p.providers["small-prov"] = &testProv{key: "s"}
	p.providers["big-prov"] = &testProv{key: "b"}
	p.catalog = testCatalog(map[string]struct {
		Context int64
		Input   []string
	}{"small": {Context: 8000}, "big": {Context: 128000}})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/responses", strings.NewReader(`{"model":"glm","input":"hi"}`))
	resp, err := http.DefaultClient.Do(req.WithContext(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 400 || string(got) != badKey {
		t.Errorf("status=%d body=%s, want the upstream 400 %q unchanged", resp.StatusCode, got, badKey)
	}
	if smallHits != 1 || bigHits != 0 {
		t.Errorf("small=%d big=%d, want small only (an ordinary 400 never retries)", smallHits, bigHits)
	}
	// Same commit-metrics race as above: wait for the server-side Committed
	// effect before asserting the snapshot.
	m := awaitCommitMetrics(t, p, counters.PMKey{Provider: "small-prov", Model: "small"})
	if s := m[counters.PMKey{Provider: "small-prov", Model: "small"}]; s.Failovers != 0 {
		t.Errorf("small metrics = %+v, want failovers=0 (committed 4xx)", s)
	}
}

// TestForward_ContextOverflowRetry_NoBiggerTarget: an overflow with no larger
// context window anywhere commits the upstream 400 byte-complete.
func TestForward_ContextOverflowRetry_NoBiggerTarget(t *testing.T) {
	var smallHits int
	body := `{"err":"small-overflow","code":"context_length_exceeded"}`
	smallUp := overflowServer(body, &smallHits)
	defer smallUp.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"small-prov": {OpenAIBaseURL: smallUp.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"glm": {{Provider: "small-prov", Model: "small"}},
		},
	}
	p := newTestProxy(t, cfg)
	p.providers["small-prov"] = &testProv{key: "s"}
	p.catalog = testCatalog(map[string]struct {
		Context int64
		Input   []string
	}{"small": {Context: 8000}})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/responses", strings.NewReader(`{"model":"glm","input":"hi"}`))
	resp, err := http.DefaultClient.Do(req.WithContext(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 400 || string(got) != body {
		t.Errorf("status=%d body=%s, want the upstream 400 %q byte-complete", resp.StatusCode, got, body)
	}
	if smallHits != 1 {
		t.Errorf("small=%d, want exactly one attempt", smallHits)
	}
}

// TestForward_ContextOverflowRetry_NoCatalog: without a catalog the feature is
// a no-op — an overflow-shaped 400 commits unchanged (mirrors the proactive
// routing degradation).
// TestForward_ContextOverflowRetry_F3_UntriedTargets: a route has
// [A(8k, p1), B(128k, p2)]. A overflows. Before the F3 fix, `tried := ordered`
// captured BOTH targets (including untried B), so maxContext=128k and the
// "strictly larger" filter found nothing → B was never tried. After the fix,
// `alreadyTried := ordered[:ti+1]` captures only A → maxContext=8k → B(128k) >
// 8k → retry succeeds on B.
func TestForward_ContextOverflowRetry_F3_UntriedTargets(t *testing.T) {
	var smallHits, bigHits int
	smallUp := overflowServer(`{"error":{"code":"context_length_exceeded","message":"context length exceeded"}}`, &smallHits)
	defer smallUp.Close()
	bigUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bigHits++
		w.Write([]byte(`{"from":"big"}`))
	}))
	defer bigUp.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"small-prov": {OpenAIBaseURL: smallUp.URL, Provider: testProviderID},
			"big-prov":   {OpenAIBaseURL: bigUp.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
			// SAME route: small (priority 1, tried first) + big (priority 2).
			"glm": {
				{Provider: "small-prov", Model: "small", Priority: 1},
				{Provider: "big-prov", Model: "big", Priority: 2},
			},
		},
	}
	p := newTestProxy(t, cfg)
	p.providers["small-prov"] = &testProv{key: "s"}
	p.providers["big-prov"] = &testProv{key: "b"}
	p.catalog = testCatalog(map[string]struct {
		Context int64
		Input   []string
	}{"small": {Context: 8000}, "big": {Context: 128000}})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/responses", strings.NewReader(`{"model":"glm","input":"hi"}`))
	resp, err := http.DefaultClient.Do(req.WithContext(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 || string(got) != `{"from":"big"}` {
		t.Errorf("status=%d body=%s — big-prov (untried, larger context) should have been the retry target", resp.StatusCode, got)
	}
	if smallHits != 1 {
		t.Errorf("small-prov hits=%d want 1 (tried first, overflowed)", smallHits)
	}
	if bigHits != 1 {
		t.Errorf("big-prov hits=%d want 1 (retry target)", bigHits)
	}
}

func TestForward_ContextOverflowRetry_NoCatalog(t *testing.T) {
	var smallHits int
	body := `{"err":"small-overflow","code":"context_length_exceeded"}`
	smallUp := overflowServer(body, &smallHits)
	defer smallUp.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"small-prov": {OpenAIBaseURL: smallUp.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"glm": {{Provider: "small-prov", Model: "small"}},
		},
	}
	p := newTestProxy(t, cfg)
	p.providers["small-prov"] = &testProv{key: "s"}
	// p.catalog left nil.
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	req, _ := http.NewRequest(http.MethodPost, px.URL+"/v1/responses", strings.NewReader(`{"model":"glm","input":"hi"}`))
	resp, err := http.DefaultClient.Do(req.WithContext(context.Background()))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 400 || string(got) != body {
		t.Errorf("status=%d body=%s, want the upstream 400 %q unchanged (no catalog → no retry)", resp.StatusCode, got, body)
	}
}

// ---- attempt_outcomes_test.go ----

// TestForward_AttemptOutcomeCounters: every upstream attempt is classified
// into the virtual ("attempts", outcome) series — a failing first target and
// a committed second target must yield exactly one hard + one ok, with the
// real per-provider counters unchanged in shape.
func TestForward_AttemptOutcomeCounters(t *testing.T) {
	failingUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"boom"}`))
	}))
	defer failingUp.Close()
	okUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"ok":true}`))
	}))
	defer okUp.Close()

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"bad-prov":  {OpenAIBaseURL: failingUp.URL, Provider: testProviderID},
			"good-prov": {OpenAIBaseURL: okUp.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{
			"glm": {{Provider: "bad-prov", Model: "m"}, {Provider: "good-prov", Model: "m"}},
		},
	}
	p := newTestProxy(t, cfg)
	p.providers["bad-prov"] = &testProv{key: "x"}
	p.providers["good-prov"] = &testProv{key: "y"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	resp, err := http.DefaultClient.Post(px.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"glm","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want failover to good-prov and 200", resp.StatusCode)
	}

	// Wait for the server-side Committed effect (attempts/ok + good-prov
	// Requests land post-copy in the handler goroutine; a Content-Length
	// client can finish reading first) before asserting the snapshot.
	snap := awaitCommitMetrics(t, p, counters.PMKey{Provider: "good-prov", Model: "m"})
	hard := snap[counters.PMKey{Provider: "attempts", Model: "hard"}].Requests
	ok := snap[counters.PMKey{Provider: "attempts", Model: "ok"}].Requests
	rateLimited := snap[counters.PMKey{Provider: "attempts", Model: "rate_limited"}].Requests
	if hard != 1 || ok != 1 || rateLimited != 0 {
		t.Errorf("attempt outcomes = hard %d, ok %d, rate_limited %d; want 1/1/0", hard, ok, rateLimited)
	}
	// The real provider rows keep their own accounting: one request committed
	// on good-prov, one failure on bad-prov.
	if snap[counters.PMKey{Provider: "good-prov", Model: "m"}].Requests != 1 {
		t.Errorf("good-prov requests = %d, want 1", snap[counters.PMKey{Provider: "good-prov", Model: "m"}].Requests)
	}
	if snap[counters.PMKey{Provider: "bad-prov", Model: "m"}].Failures != 1 {
		t.Errorf("bad-prov failures = %d, want exactly 1 (double-accounting regression)", snap[counters.PMKey{Provider: "bad-prov", Model: "m"}].Failures)
	}

	// Routing-decision overhead: one observation per request under the virtual
	// ("routing","decision") row, latency sum populated.
	routing := snap[counters.PMKey{Provider: "routing", Model: "decision"}]
	if routing.Requests != 1 {
		t.Errorf("routing observations = %d, want exactly 1 (one decision per request)", routing.Requests)
	}
}

// ---- strict_lossy_test.go ----

// TestForward_StrictLossyRefusesAndAnswers400: with conversion.strict_lossy
// on, a request whose conversion is lossy-but-degradable is refused by every
// converting target and the client gets the 400 unsupported envelope (no
// silent degradation); with strict off the same request converts and 200s.
func TestForward_StrictLossyRefusesAndAnswers400(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"id":"r1","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer up.Close()

	newProxy := func(strict bool) *httptest.Server {
		cfg := &configdomain.Config{
			Providers: map[string]configdomain.Provider{"backend": {OpenAIBaseURL: up.URL, Provider: testProviderID}},
			Routes: map[string][]configdomain.RouteTarget{
				"glm": {{Provider: "backend", Model: "backend-model", Protocol: "responses"}},
			},
			Conversion: configdomain.ConversionConfig{StrictLossy: strict},
		}
		p := newTestProxy(t, cfg)
		p.providers["backend"] = &testProv{key: "k"}
		px := httptest.NewServer(http.HandlerFunc(p.Handler))
		t.Cleanup(px.Close)
		return px
	}
	// anthropic client → responses backend with `stop` (no Responses
	// equivalent → stop_dropped diagnostic).
	body := `{"model":"glm","max_tokens":16,"stop_sequences":["END"],"messages":[{"role":"user","content":"hi"}]}`

	resp, err := http.Post(newProxy(false).URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("strict off: status = %d, want 200 (lossy tolerated)", resp.StatusCode)
	}

	resp, err = http.Post(newProxy(true).URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("strict on: status = %d, want 400: %s", resp.StatusCode, raw)
	}
	var envelope struct {
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatalf("400 body not an anthropic error envelope: %s", raw)
	}
	if !strings.Contains(string(raw), "strict_lossy") {
		t.Fatalf("400 body lacks the strict_lossy feature marker: %s", raw)
	}
}

// TestForward_LearnsDeveloperRoleRename pins the developer-role learning
// retry: a chat upstream that 400s the developer role gets the request
// retried once with developer renamed to system, the lesson persists for the
// (provider, model), and the SECOND request is pre-renamed before it leaves.
func TestForward_LearnsDeveloperRoleRename(t *testing.T) {
	var up *fakeUpstream
	up = newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		// newFakeUpstream drains r.Body before dispatching; reject by hit
		// count (first hit = the developer-role request).
		if up.hits() == 1 {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, "{\"code\":\"InvalidParameter\",\"message\":\"The parameter `messages.role` specified in the request are not valid: invalid value: `developer`, supported values are [system user assistant tool]\"}")
			return
		}
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"id":"c1","choices":[{"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	})
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{"v": {OpenAIBaseURL: up.srv.URL, Provider: testProviderID}},
		Routes:    map[string][]configdomain.RouteTarget{"m1": {{Provider: "v", Model: "m1"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["v"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	// Chat client speaking chat backend (passthrough): a developer-roled
	// system message rides through byte-level.
	body := `{"model":"m1","messages":[{"role":"developer","content":"be brief"},{"role":"user","content":"ping"}]}`
	resp, err := http.Post(px.URL+"/v1/chat/completions", "application/json", stringReader(body))
	if err != nil {
		t.Fatal(err)
	}
	rb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first request status = %d: %s", resp.StatusCode, rb)
	}

	if up.hits() != 2 {
		t.Fatalf("upstream hits = %d, want 2 (reject + renamed retry)", up.hits())
	}
	if !strings.Contains(up.lastBody(), `"system"`) || strings.Contains(up.lastBody(), `"developer"`) {
		t.Errorf("retry body not renamed: %s", up.lastBody())
	}
	if !p.runtimeState.ParamBlocked("v", "m1", "developer_role") {
		t.Error("developer_role lesson not persisted")
	}
	firstRound := up.hits()

	// Second request: pre-renamed on the way out (no reject round-trip).
	resp2, err := http.Post(px.URL+"/v1/chat/completions", "application/json", stringReader(body))
	if err != nil {
		t.Fatal(err)
	}
	rb2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second request status = %d: %s", resp2.StatusCode, rb2)
	}
	if up.hits() != firstRound+1 {
		t.Errorf("upstream hits after second request = %d, want %d (no reject round-trip)", up.hits(), firstRound+1)
	}
	if strings.Contains(up.lastBody(), `"developer"`) {
		t.Errorf("second request body not pre-renamed: %s", up.lastBody())
	}
}

// TestForward_LearnsThinkingAdaptiveE2E pins the thinking_adaptive lesson end
// to end through the full forward pipeline: an anthropic upstream that
// rejects budget-based thinking (shopee's wording) gets the request retried
// once with {"type":"adaptive"}, the lesson persists, and the SECOND request
// is pre-rewritten before it leaves.
func TestForward_LearnsThinkingAdaptiveE2E(t *testing.T) {
	var up *fakeUpstream
	up = newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		if up.lastBody() == "" || !strings.Contains(up.lastBody(), `"adaptive"`) {
			// first hit carries budget-based thinking (the only shape the
			// client sends) — reject with shopee's exact wording
			if up.hits() == 1 {
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"\"thinking.type.enabled\" is not supported for this model. Use \"thinking.type.adaptive\""}}`)
				return
			}
		}
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	})
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{"s": {AnthropicBaseURL: up.srv.URL, Provider: testProviderID}},
		Routes:    map[string][]configdomain.RouteTarget{"m": {{Provider: "s", Model: "m", Protocol: "anthropic"}}},
	}
	p := newTestProxy(t, cfg)
	p.providers["s"] = &testProv{key: "k"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	body := `{"model":"m","max_tokens":100,"thinking":{"type":"enabled","budget_tokens":8000},"messages":[{"role":"user","content":"ping"}]}`
	resp, err := http.Post(px.URL+"/v1/messages", "application/json", stringReader(body))
	if err != nil {
		t.Fatal(err)
	}
	rb, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first request status = %d: %s", resp.StatusCode, rb)
	}
	if up.hits() != 2 {
		t.Fatalf("upstream hits = %d, want 2 (reject + adaptive retry)", up.hits())
	}
	if !strings.Contains(up.lastBody(), `{"type":"adaptive"}`) {
		t.Errorf("retry body thinking not adaptive: %s", up.lastBody())
	}
	if !p.runtimeState.ParamBlocked("s", "m", "thinking_adaptive") {
		t.Error("thinking_adaptive lesson not persisted")
	}

	resp2, err := http.Post(px.URL+"/v1/messages", "application/json", stringReader(body))
	if err != nil {
		t.Fatal(err)
	}
	rb2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("second request status = %d: %s", resp2.StatusCode, rb2)
	}
	if up.hits() != 3 {
		t.Errorf("upstream hits after second request = %d, want 3 (no reject round-trip)", up.hits())
	}
	if !strings.Contains(up.lastBody(), `{"type":"adaptive"}`) {
		t.Errorf("second request not pre-rewritten: %s", up.lastBody())
	}
}

// TestForward_ErrorDegradationVisibleE2E pins the error-degradation contract
// end to end: an upstream 4xx with an unrecognized envelope (FastAPI detail,
// empty body) must reach cross-protocol clients as a translated envelope
// carrying the upstream message — never an opaque "response conversion
// failed" 502.
func TestForward_ErrorDegradationVisibleE2E(t *testing.T) {
	cases := []struct {
		name       string
		upstream   string
		status     int
		clientPath string
		wantBody   string
	}{
		{"fastapi detail via anthropic client", `{"detail":"The 'gpt-x' model is not supported when using Codex with a ChatGPT account."}`, 400, "/v1/messages", "not supported when using Codex"},
		{"no body via anthropic client", ``, 400, "/v1/messages", "upstream request failed with status 400"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.upstream)
			})
			cfg := &configdomain.Config{
				Providers: map[string]configdomain.Provider{"c": {OpenAIBaseURL: up.srv.URL, Provider: testProviderID}},
				Routes:    map[string][]configdomain.RouteTarget{"m": {{Provider: "c", Model: "m", Protocol: "responses"}}},
			}
			p := newTestProxy(t, cfg)
			p.providers["c"] = &testProv{key: "k"}
			px := httptest.NewServer(http.HandlerFunc(p.Handler))
			defer px.Close()

			resp, err := http.Post(px.URL+tc.clientPath, "application/json",
				stringReader(`{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"ping"}]}`))
			if err != nil {
				t.Fatal(err)
			}
			rb, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d (want the upstream 400 preserved, not a 502): %s", resp.StatusCode, rb)
			}
			if !strings.Contains(string(rb), tc.wantBody) {
				t.Errorf("client body lost the upstream diagnosis: %s", rb)
			}
			if !strings.Contains(string(rb), `"type":"error"`) {
				t.Errorf("no anthropic error envelope: %s", rb)
			}
		})
	}
}

// TestForward_ModelDeniedNewWordingE2E pins the v3 IsModelDenied marker end
// to end: shopee's retcode-40403 wording must count as a model denial (model
// failure recorded → model lock), while an adjacent plan-tier "not supported"
// 400 must NOT.
func TestForward_ModelDeniedNewWordingE2E(t *testing.T) {
	run := func(t *testing.T, upstreamBody string, wantLocked bool) {
		up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, upstreamBody)
		})
		cfg := &configdomain.Config{
			Providers: map[string]configdomain.Provider{"s": {OpenAIBaseURL: up.srv.URL, Provider: testProviderID}},
			Routes:    map[string][]configdomain.RouteTarget{"m": {{Provider: "s", Model: "m"}}},
		}
		p := newTestProxy(t, cfg)
		p.providers["s"] = &testProv{key: "k"}
		px := httptest.NewServer(http.HandlerFunc(p.Handler))
		defer px.Close()

		resp, err := http.Post(px.URL+"/v1/chat/completions", "application/json",
			stringReader(`{"model":"m","messages":[{"role":"user","content":"ping"}]}`))
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()

		locked := p.modelLocked("s", "m", time.Now())
		if locked != wantLocked {
			t.Errorf("model locked = %v, want %v", locked, wantLocked)
		}
	}
	t.Run("retcode 40403 locks the model", func(t *testing.T) {
		run(t, `{"retcode":40403,"message":"Model not supported by this endpoint"}`, true)
	})
	t.Run("plan-tier not supported does not lock", func(t *testing.T) {
		run(t, `{"error":{"message":"Streaming is not supported for this plan tier"}}`, false)
	})
}
