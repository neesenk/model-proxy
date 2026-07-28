package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
		if got := isContextOverflow(c.status, []byte(c.body)); got != c.want {
			t.Errorf("%s: isContextOverflow(%d, body) = %v, want %v", c.name, c.status, got, c.want)
		}
	}
}

// overflowServer returns an httptest server that always answers 400 with a
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

	cfg := &Config{
		Providers: map[string]Provider{
			"small-prov": {OpenAIBaseURL: smallUp.URL, Provider: testProviderID},
			"big-prov":   {OpenAIBaseURL: bigUp.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
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
	px := httptest.NewServer(http.HandlerFunc(p.handler))
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
	m := p.metrics.snapshot()
	if s := m[pmKey{Provider: "small-prov", Model: "small"}]; s.Failovers != 1 || s.Requests != 0 || s.Failures != 0 {
		t.Errorf("small metrics = %+v, want failovers=1 requests=0 failures=0", s)
	}
	if b := m[pmKey{Provider: "big-prov", Model: "big"}]; b.Requests != 1 {
		t.Errorf("big metrics = %+v, want requests=1", b)
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

	cfg := &Config{
		Providers: map[string]Provider{
			"small-prov": {OpenAIBaseURL: smallUp.URL, Provider: testProviderID},
			"big-text":   {OpenAIBaseURL: bigTextUp.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
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
	px := httptest.NewServer(http.HandlerFunc(p.handler))
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

	cfg := &Config{
		Providers: map[string]Provider{
			"small-prov": {OpenAIBaseURL: smallUp.URL, Provider: testProviderID},
			"big-prov":   {OpenAIBaseURL: bigUp.URL, Provider: testProviderID},
			"huge-prov":  {OpenAIBaseURL: hugeUp.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
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
	px := httptest.NewServer(http.HandlerFunc(p.handler))
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

	cfg := &Config{
		Providers: map[string]Provider{
			"small-prov": {OpenAIBaseURL: smallUp.URL, Provider: testProviderID},
			"big-prov":   {OpenAIBaseURL: bigUp.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
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
	px := httptest.NewServer(http.HandlerFunc(p.handler))
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
	m := p.metrics.snapshot()
	if s := m[pmKey{Provider: "small-prov", Model: "small"}]; s.Requests != 1 || s.Failovers != 0 {
		t.Errorf("small metrics = %+v, want requests=1 failovers=0 (committed 4xx)", s)
	}
}

// TestForward_ContextOverflowRetry_NoBiggerTarget: an overflow with no larger
// context window anywhere commits the upstream 400 byte-complete.
func TestForward_ContextOverflowRetry_NoBiggerTarget(t *testing.T) {
	var smallHits int
	body := `{"err":"small-overflow","code":"context_length_exceeded"}`
	smallUp := overflowServer(body, &smallHits)
	defer smallUp.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"small-prov": {OpenAIBaseURL: smallUp.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"glm": {{Provider: "small-prov", Model: "small"}},
		},
	}
	p := newTestProxy(t, cfg)
	p.providers["small-prov"] = &testProv{key: "s"}
	p.catalog = testCatalog(map[string]struct {
		Context int64
		Input   []string
	}{"small": {Context: 8000}})
	px := httptest.NewServer(http.HandlerFunc(p.handler))
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

	cfg := &Config{
		Providers: map[string]Provider{
			"small-prov": {OpenAIBaseURL: smallUp.URL, Provider: testProviderID},
			"big-prov":   {OpenAIBaseURL: bigUp.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
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
	px := httptest.NewServer(http.HandlerFunc(p.handler))
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

	cfg := &Config{
		Providers: map[string]Provider{
			"small-prov": {OpenAIBaseURL: smallUp.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"glm": {{Provider: "small-prov", Model: "small"}},
		},
	}
	p := newTestProxy(t, cfg)
	p.providers["small-prov"] = &testProv{key: "s"}
	// p.catalog left nil.
	px := httptest.NewServer(http.HandlerFunc(p.handler))
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
