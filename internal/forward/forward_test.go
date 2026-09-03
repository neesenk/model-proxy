package forward

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	responsecache "model-proxy/internal/cache"
	"model-proxy/internal/guard"
	guardsession "model-proxy/internal/guard/session"
	"model-proxy/internal/observe/counters"
)

func openaiOKResponder(text string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl_1","choices":[{"index":0,"message":{"role":"assistant","content":`+strconv_(text)+`},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	}
}

func strconv_(s string) string {
	return `"` + s + `"`
}

func openaiChatBody() string {
	return `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
}

// TestServeHappyPathCommit: a routed openai request reaches the upstream,
// commits its 2xx, dispatches the shadow port and closes the live event pair.
func TestServeHappyPathCommit(t *testing.T) {
	up := newFakeUpstream(t, openaiOKResponder("hello"))
	h := newHarness()
	cfg := &Config{
		Providers: map[string]Provider{"up": {OpenAIBaseURL: up.srv.URL, Provider: "test-static"}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "up", Model: "real-model"}}},
	}
	w := h.serve(h.snapshot(cfg), "openai", "/v1/chat/completions", openaiChatBody(), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "hello") {
		t.Fatalf("body missing upstream content: %s", w.Body.String())
	}
	if !strings.Contains(up.lastBody(), "real-model") {
		t.Errorf("upstream body missing rewritten model: %s", up.lastBody())
	}
	if len(h.shadow) != 1 || h.shadow[0].provider != "up" {
		t.Errorf("shadow dispatch = %+v, want one call for up", h.shadow)
	}
	// The commit end event is published by the app-side Effects adapter
	// (targetExecutionEffects), not by the pipeline — here the fake effects
	// sink observes the commit through the port.
	if h.fx.committed != 1 {
		t.Errorf("effects committed = %d, want 1", h.fx.committed)
	}
	if h.gate.successes["up"] != 1 {
		t.Errorf("gate successes = %d, want 1", h.gate.successes["up"])
	}
}

// TestServeRejectsBeforeRouting: the three pre-routing terminals — unreadable
// model field, unrouted model, oversized body — each close with a terminal
// live event and no upstream call.
func TestServeRejectsBeforeRouting(t *testing.T) {
	up := newFakeUpstream(t, openaiOKResponder("hello"))
	h := newHarness()
	cfg := &Config{
		Providers: map[string]Provider{"up": {OpenAIBaseURL: up.srv.URL, Provider: "test-static"}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "up", Model: "real-model"}}},
	}
	snap := h.snapshot(cfg)

	w := h.serve(snap, "openai", "/v1/chat/completions", `{"nomodel":true}`, nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("missing model status = %d, want 400", w.Code)
	}

	w = h.serve(snap, "openai", "/v1/chat/completions", `{"model":"ghost","messages":[]}`, nil)
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "not found in routes") {
		t.Errorf("unrouted model status = %d body = %s, want 502", w.Code, w.Body.String())
	}

	small := h.snapshot(&Config{
		MaxRequestBodyBytes: 16,
		Providers:           cfg.Providers,
		Routes:              cfg.Routes,
	})
	w = h.serve(small, "openai", "/v1/chat/completions", openaiChatBody(), nil)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body status = %d, want 413", w.Code)
	}

	if up.hits() != 0 {
		t.Errorf("upstream hits = %d, want 0 (all terminals precede routing)", up.hits())
	}
	if ends := h.endEvents("req-test"); len(ends) != 3 {
		t.Errorf("terminal end events = %d, want 3 (one per terminal)", len(ends))
	}
}

// TestServeClaudeAliasRoute: an anthropic client's claude-* name resolves via
// an explicit alias route; the upstream sees the route's target model (the
// post-claude_mapping alias mechanism).
func TestServeClaudeAliasRoute(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	})
	h := newHarness()
	cfg := &Config{
		Providers: map[string]Provider{"up": {AnthropicBaseURL: up.srv.URL, Provider: "test-static"}},
		Routes:    map[string][]RouteTarget{"claude-sonnet": {{Provider: "up", Model: "glm-5"}}},
	}
	body := `{"model":"claude-sonnet","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`
	w := h.serve(h.snapshot(cfg), "anthropic", "/v1/messages", body, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(up.lastBody(), "glm-5") {
		t.Errorf("upstream body = %s, want alias route target model glm-5", up.lastBody())
	}
	if up.paths[0] != "/v1/messages" {
		t.Errorf("anthropic keeps /v1 prefix: path = %s", up.paths[0])
	}
}

// TestServeProviderPrefixedModel: a "provider/model" called name (no exact
// route key) decomposes — route by the bare model narrowed to the named
// provider; an exact route key named "p/m" still wins; unknown prefixes and
// providers not serving the model 502.
func TestServeProviderPrefixedModel(t *testing.T) {
	upA := newFakeUpstream(t, openaiOKResponder("from-a"))
	upB := newFakeUpstream(t, openaiOKResponder("from-b"))
	h := newHarness()
	cfg := &Config{
		Providers: map[string]Provider{
			"a": {OpenAIBaseURL: upA.srv.URL, Provider: "test-static"},
			"b": {OpenAIBaseURL: upB.srv.URL, Provider: "test-static"},
		},
		Routes: map[string][]RouteTarget{
			"glm":  {{Provider: "a", Model: "glm-a"}, {Provider: "b", Model: "glm-b"}},
			"solo": {{Provider: "a", Model: "x"}},
		},
	}
	snap := h.snapshot(cfg)
	chat := func(model string) string {
		return `{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`
	}

	// 1) "b/glm" → only b is hit, upstream sees the target's real model.
	w := h.serve(snap, "openai", "/v1/chat/completions", chat("b/glm"), nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "from-b") {
		t.Fatalf("b/glm: status = %d body = %s", w.Code, w.Body.String())
	}
	if upA.hits() != 0 || upB.hits() != 1 {
		t.Errorf("hits a=%d b=%d, want 0/1", upA.hits(), upB.hits())
	}
	if !strings.Contains(upB.lastBody(), "glm-b") {
		t.Errorf("upstream body = %s, want glm-b", upB.lastBody())
	}

	// 2) unknown prefix → treated as a plain (unrouted) model name → 502.
	w = h.serve(snap, "openai", "/v1/chat/completions", chat("ghost/glm"), nil)
	if w.Code != http.StatusBadGateway {
		t.Errorf("ghost/glm: status = %d, want 502", w.Code)
	}

	// 3) provider not serving the model → 502, no upstream call.
	w = h.serve(snap, "openai", "/v1/chat/completions", chat("b/solo"), nil)
	if w.Code != http.StatusBadGateway {
		t.Errorf("b/solo: status = %d, want 502", w.Code)
	}
	if upA.hits() != 0 || upB.hits() != 1 {
		t.Errorf("after misses: hits a=%d b=%d, want 0/1", upA.hits(), upB.hits())
	}

	// 4) an exact route key literally named "b/glm" wins over decomposition.
	cfgExact := &Config{
		Providers: cfg.Providers,
		Routes: map[string][]RouteTarget{
			"glm":   cfg.Routes["glm"],
			"b/glm": {{Provider: "a", Model: "glm-a"}},
		},
	}
	w = h.serve(h.snapshot(cfgExact), "openai", "/v1/chat/completions", chat("b/glm"), nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "from-a") {
		t.Errorf("exact route precedence: status = %d body = %s, want from-a", w.Code, w.Body.String())
	}
}

// TestServeOpenAIStripsV1Prefix: an openai client path loses its /v1 prefix
// (the provider base URL carries its own version segment).
func TestServeOpenAIStripsV1Prefix(t *testing.T) {
	up := newFakeUpstream(t, openaiOKResponder("hello"))
	h := newHarness()
	cfg := &Config{
		Providers: map[string]Provider{"up": {OpenAIBaseURL: up.srv.URL + "/v3", Provider: "test-static"}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "up", Model: "real-model"}}},
	}
	w := h.serve(h.snapshot(cfg), "openai", "/v1/chat/completions", openaiChatBody(), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	if up.paths[0] != "/v3/chat/completions" {
		t.Errorf("path = %s, want /v3/chat/completions (/v1 stripped)", up.paths[0])
	}
}

// TestServeCacheHitReplayAndBypass: an identical second request replays from
// the exact-match cache; a pin and a force-provider override each bypass it.
func TestServeCacheHitReplayAndBypass(t *testing.T) {
	up := newFakeUpstream(t, openaiOKResponder("cached"))
	h := newHarness()
	cfg := &Config{
		Providers: map[string]Provider{"up": {OpenAIBaseURL: up.srv.URL, Provider: "test-static"}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "up", Model: "real-model"}}},
	}
	snap := h.snapshot(cfg)
	snap.Cache = responsecache.New(responsecache.Options{TTL: time.Hour, MaxEntries: 8, MaxBodyBytes: 1 << 16})

	if w := h.serve(snap, "openai", "/v1/chat/completions", openaiChatBody(), nil); w.Code != 200 {
		t.Fatalf("first status = %d", w.Code)
	}
	w := h.serve(snap, "openai", "/v1/chat/completions", openaiChatBody(), nil)
	if w.Code != 200 || w.Header().Get("x-mp-cache") != "hit" {
		t.Fatalf("second status = %d x-mp-cache = %q, want cache hit", w.Code, w.Header().Get("x-mp-cache"))
	}
	if up.hits() != 1 {
		t.Fatalf("upstream hits = %d, want 1 (second served from cache)", up.hits())
	}
	cacheEnd := false
	for _, e := range h.endEvents("req-test") {
		if e.Provider == "(cache)" {
			cacheEnd = true
		}
	}
	if !cacheEnd {
		t.Error("cache hit must emit a (cache) end event")
	}

	// Pin: bypasses the cache and forces the pinned backend even with its
	// circuit open (pin is an explicit hard selection).
	h.state.pins["m"] = true
	h.gate.halfOpenOpen["up"] = true
	if w := h.serve(snap, "openai", "/v1/chat/completions", openaiChatBody(), nil); w.Code != 200 {
		t.Fatalf("pinned status = %d, want 200 (circuit bypassed by pin)", w.Code)
	}
	if up.hits() != 2 {
		t.Fatalf("upstream hits = %d, want 2 (pin bypasses cache)", up.hits())
	}
	delete(h.state.pins, "m")
	delete(h.gate.halfOpenOpen, "up")

	// Force-provider: also a cache bypass, and a typo is a hard 400.
	w = h.serve(snap, "openai", "/v1/chat/completions", openaiChatBody(), map[string]string{"x-mp-force-provider": "typo"})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "is not a target") {
		t.Errorf("force-provider typo status = %d body = %s, want 400", w.Code, w.Body.String())
	}
	w = h.serve(snap, "openai", "/v1/chat/completions", openaiChatBody(), map[string]string{"x-mp-force-provider": "up"})
	if w.Code != 200 || up.hits() != 3 {
		t.Errorf("force-provider status = %d hits = %d, want 200 and a fresh upstream call", w.Code, up.hits())
	}
}

// TestServeAllTargetsHardFail502: every target failing hard ends in an honest
// 502 with a closing end event (and agent failure attribution).
func TestServeAllTargetsHardFail502(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	h := newHarness()
	cfg := &Config{
		Providers: map[string]Provider{"up": {OpenAIBaseURL: up.srv.URL, Provider: "test-static"}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "up", Model: "real-model"}}},
	}
	w := h.serve(h.snapshot(cfg), "openai", "/v1/chat/completions", openaiChatBody(), nil)
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "all targets failed") {
		t.Fatalf("status = %d body = %s, want 502", w.Code, w.Body.String())
	}
	if ends := h.endEvents("req-test"); len(ends) != 1 || ends[0].Status != 502 {
		t.Errorf("end events = %+v, want one 502", ends)
	}
	if h.gate.failures["up"] == 0 {
		t.Error("provider failure never recorded on the gate")
	}
}

// TestServeRateLimitedTerminal429: a pure rate-limit failure class answers 429
// with Retry-After when the earliest recovery is beyond the wait window.
func TestServeRateLimitedTerminal429(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "120")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	h := newHarness()
	h.state.allRateLimited = true
	h.state.earliest = time.Now().Add(2 * time.Minute)
	cfg := &Config{
		Providers:  map[string]Provider{"up": {OpenAIBaseURL: up.srv.URL, Provider: "test-static"}},
		Routes:     map[string][]RouteTarget{"m": {{Provider: "up", Model: "real-model"}}},
		Scheduling: Scheduling{RetryWait: "1ms"},
	}
	w := h.serve(h.snapshot(cfg), "openai", "/v1/chat/completions", openaiChatBody(), nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d body = %s, want 429", w.Code, w.Body.String())
	}
	if w.Header().Get("Retry-After") == "" {
		t.Error("429 terminal must carry Retry-After")
	}
	if _, ok := h.gate.rateLimits["up"]; !ok {
		t.Error("rate limit never recorded on the gate")
	}
}

// TestServeCooldownWaitRetryThenCommit: when every target is cooling and the
// earliest recovery is inside retry_wait, the pipeline waits it out and
// re-runs the pass instead of erroring immediately.
func TestServeCooldownWaitRetryThenCommit(t *testing.T) {
	var hits int
	up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		openaiOKResponder("recovered")(w, r)
	})
	h := newHarness()
	h.state.allDown = true
	h.state.allRateLimited = true
	h.state.earliest = time.Now().Add(5 * time.Millisecond)
	cfg := &Config{
		Providers:  map[string]Provider{"up": {OpenAIBaseURL: up.srv.URL, Provider: "test-static"}},
		Routes:     map[string][]RouteTarget{"m": {{Provider: "up", Model: "real-model"}}},
		Scheduling: Scheduling{RetryWait: "2s"},
	}
	w := h.serve(h.snapshot(cfg), "openai", "/v1/chat/completions", openaiChatBody(), nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "recovered") {
		t.Fatalf("status = %d body = %s, want the post-wait commit", w.Code, w.Body.String())
	}
	if up.hits() != 2 {
		t.Errorf("upstream hits = %d, want 2 (failed pass + retried pass)", up.hits())
	}
}

// TestServeClientGoneDuringCooldownWait: a client disconnect during the wait
// aborts the retry loop with a 499 live event and no written status.
func TestServeClientGoneDuringCooldownWait(t *testing.T) {
	up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTooManyRequests) })
	h := newHarness()
	h.state.allDown = true
	h.state.allRateLimited = true
	h.state.earliest = time.Now().Add(time.Minute)
	cfg := &Config{
		Providers:  map[string]Provider{"up": {OpenAIBaseURL: up.srv.URL, Provider: "test-static"}},
		Routes:     map[string][]RouteTarget{"m": {{Provider: "up", Model: "real-model"}}},
		Scheduling: Scheduling{RetryWait: "1h"},
	}
	snap := h.snapshot(cfg)
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(openaiChatBody())).WithContext(ctx)
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	w := httptest.NewRecorder()
	Serve(h.svc, h.state, snap, "openai", w, r, "req-gone")
	ends := h.endEvents("req-gone")
	if len(ends) != 1 || ends[0].Status != statusClientGone {
		t.Errorf("end events = %+v, want one 499 (client closed)", ends)
	}
}

// TestServeRecoveredUntriedRetriesImmediately: the TOCTOU path — a cooled-down
// target recovered mid-pass but was never tried — re-runs the pass at once.
func TestServeRecoveredUntriedRetriesImmediately(t *testing.T) {
	var hits int
	up := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		openaiOKResponder("second-pass")(w, r)
	})
	h := newHarness()
	h.state.recoveredUntried = true
	cfg := &Config{
		Providers:  map[string]Provider{"up": {OpenAIBaseURL: up.srv.URL, Provider: "test-static"}},
		Routes:     map[string][]RouteTarget{"m": {{Provider: "up", Model: "real-model"}}},
		Scheduling: Scheduling{RetryWait: "1s"},
	}
	w := h.serve(h.snapshot(cfg), "openai", "/v1/chat/completions", openaiChatBody(), nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "second-pass") {
		t.Fatalf("status = %d body = %s, want the immediate-retry commit", w.Code, w.Body.String())
	}
}

// TestServeFailoverToSecondTarget: the first target failing hard falls through
// to the next route target in order.
func TestServeFailoverToSecondTarget(t *testing.T) {
	bad := newFakeUpstream(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) })
	good := newFakeUpstream(t, openaiOKResponder("fallback"))
	h := newHarness()
	cfg := &Config{
		Providers: map[string]Provider{
			"bad":  {OpenAIBaseURL: bad.srv.URL, Provider: "test-static"},
			"good": {OpenAIBaseURL: good.srv.URL, Provider: "test-static"},
		},
		Routes: map[string][]RouteTarget{"m": {{Provider: "bad", Model: "x"}, {Provider: "good", Model: "y"}}},
	}
	w := h.serve(h.snapshot(cfg), "openai", "/v1/chat/completions", openaiChatBody(), nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "fallback") {
		t.Fatalf("status = %d body = %s, want the fallback commit", w.Code, w.Body.String())
	}
	if bad.hits() != 1 || good.hits() != 1 {
		t.Errorf("hits bad=%d good=%d, want 1/1", bad.hits(), good.hits())
	}
}

// TestServeUnknownProviderTargetSkipped: a target naming an unknown provider is
// skipped at plan time; with no servable target the pass ends in 502.
func TestServeUnknownProviderTargetSkipped(t *testing.T) {
	h := newHarness()
	cfg := &Config{
		Providers: map[string]Provider{},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "ghost", Model: "x"}}},
	}
	w := h.serve(h.snapshot(cfg), "openai", "/v1/chat/completions", openaiChatBody(), nil)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502 (no servable target)", w.Code)
	}
}

// TestServeConversionUnsupportedFailsClosed400: an unconvertible client
// feature (n>1 for an anthropic backend) fails closed with the unsupported
// conversion 400 — the unconverted body never reaches the backend.
func TestServeConversionUnsupportedFailsClosed400(t *testing.T) {
	up := newFakeUpstream(t, openaiOKResponder("never"))
	h := newHarness()
	cfg := &Config{
		Providers: map[string]Provider{"up": {AnthropicBaseURL: up.srv.URL, Provider: "test-static"}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "up", Model: "real-model", Protocol: "anthropic"}}},
	}
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}],"n":2}`
	w := h.serve(h.snapshot(cfg), "openai", "/v1/chat/completions", body, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body = %s, want 400 unsupported conversion", w.Code, w.Body.String())
	}
	if up.hits() != 0 {
		t.Errorf("upstream hits = %d, want 0 (fail closed before send)", up.hits())
	}
}

// TestForceProvider (moved from app): header wins over query; nil-safe.
func TestForceProvider(t *testing.T) {
	request := &http.Request{
		Header: http.Header{"X-Mp-Force-Provider": []string{"header-provider"}},
		URL:    &url.URL{RawQuery: "force_provider=query-provider"},
	}
	if got := ForcedProviderFromRequest(request); got != "header-provider" {
		t.Errorf("header precedence = %q, want header-provider", got)
	}
	request.Header.Del("x-mp-force-provider")
	if got := ForcedProviderFromRequest(request); got != "query-provider" {
		t.Errorf("query fallback = %q, want query-provider", got)
	}
	if got := ForcedProviderFromRequest(nil); got != "" {
		t.Errorf("nil request = %q, want empty", got)
	}
	if got := ForcedProviderFromRequest(&http.Request{}); got != "" {
		t.Errorf("nil URL = %q, want empty", got)
	}
}

// TestPublishTerminalEvent: the terminal live event carries the request id,
// detected agent, protocol, exposed name and status.
func TestPublishTerminalEvent(t *testing.T) {
	h := newHarness()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	r.Header.Set("User-Agent", "claude-cli/1.0")
	PublishTerminalEvent(h.events, "req-term", r, "anthropic", "glm", 400)
	ends := h.endEvents("req-term")
	if len(ends) != 1 {
		t.Fatalf("end events = %+v, want exactly 1", ends)
	}
	e := ends[0]
	if e.Status != 400 || e.Protocol != "anthropic" || e.Exposed != "glm" || e.Agent == "" {
		t.Errorf("event = %+v, want status 400 with proto/exposed/agent", e)
	}
}

// --- guard pass ---

const guardTestSecret = "sk-fragmented-secret-value-1234567890"

func guardScanner(t *testing.T, extraPaths []string) *guard.Scanner {
	t.Helper()
	sc, err := guard.NewScannerWithOptions(nil, []string{guardTestSecret}, extraPaths, guard.Options{Decode: true})
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

func guardBaseConfig(up *fakeUpstream, g GuardConfig) *Config {
	return &Config{
		Providers: map[string]Provider{"up": {OpenAIBaseURL: up.srv.URL, Provider: "test-static"}},
		Routes:    map[string][]RouteTarget{"m": {{Provider: "up", Model: "real-model"}}},
		Guard:     g,
	}
}

func secretBody(s string) string {
	return `{"model":"m","messages":[{"role":"user","content":"` + s + `"}]}`
}

// TestServeGuardSecretsLogAndRedact: log counts/emits and forwards; redact
// rewrites the body before any branch sees it (the upstream never receives
// the secret).
func TestServeGuardSecretsLogAndRedact(t *testing.T) {
	up := newFakeUpstream(t, openaiOKResponder("ok"))
	h := newHarness()
	snap := h.snapshot(guardBaseConfig(up, GuardConfig{Secrets: "log", Paths: "off"}))
	snap.Guard = guardScanner(t, nil)

	w := h.serve(snap, "openai", "/v1/chat/completions", secretBody("token "+guardTestSecret), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("log action status = %d, want 200", w.Code)
	}
	guardEvent := false
	for _, e := range h.events.Snapshot() {
		if e.Type == "guard" && strings.Contains(e.Detail, "known_secret") {
			guardEvent = true
		}
	}
	if !guardEvent {
		t.Error("secrets=log must publish a guard live event")
	}
	if got := h.svc.Metrics.Snapshot()[counters.PMKey{Provider: "guard", Model: "known_secret"}].Requests; got == 0 {
		t.Error("secrets=log must increment the (guard, known_secret) counter")
	}

	up2 := newFakeUpstream(t, openaiOKResponder("ok"))
	h2 := newHarness()
	snap2 := h2.snapshot(guardBaseConfig(up2, GuardConfig{Secrets: "redact", Paths: "off"}))
	snap2.Guard = guardScanner(t, nil)
	w = h2.serve(snap2, "openai", "/v1/chat/completions", secretBody("token "+guardTestSecret), nil)
	if w.Code != http.StatusOK {
		t.Fatalf("redact action status = %d, want 200", w.Code)
	}
	if strings.Contains(up2.lastBody(), guardTestSecret) {
		t.Errorf("upstream received the unredacted secret: %s", up2.lastBody())
	}
	if !strings.Contains(up2.lastBody(), "[REDACTED]") {
		t.Errorf("upstream body missing redaction marker: %s", up2.lastBody())
	}
}

// TestServeGuardSecretsBlock: a blocked secret 400s before any upstream call.
func TestServeGuardSecretsBlock(t *testing.T) {
	up := newFakeUpstream(t, openaiOKResponder("never"))
	h := newHarness()
	snap := h.snapshot(guardBaseConfig(up, GuardConfig{Secrets: "block", Paths: "off"}))
	snap.Guard = guardScanner(t, nil)
	w := h.serve(snap, "openai", "/v1/chat/completions", secretBody("token "+guardTestSecret), nil)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "guard.secrets=block") {
		t.Fatalf("status = %d body = %s, want the secrets block 400", w.Code, w.Body.String())
	}
	if up.hits() != 0 {
		t.Errorf("upstream hits = %d, want 0", up.hits())
	}
}

// TestServeGuardPathsStrongBlockWeakLog: a sensitive path in a tool-call
// position is STRONG (block can 400 it); the same path in prose is WEAK
// (counted as *_text, never blocked).
func TestServeGuardPathsStrongBlockWeakLog(t *testing.T) {
	up := newFakeUpstream(t, openaiOKResponder("ok"))
	h := newHarness()
	snap := h.snapshot(guardBaseConfig(up, GuardConfig{Secrets: "off", Paths: "block", ExtraPaths: []string{"/secret/custom"}}))
	snap.Guard = guardScanner(t, []string{"/secret/custom"})

	strong := `{"model":"m","messages":[{"role":"user","content":"x"}],"tool_calls":[{"function":{"arguments":"{\"path\":\"/secret/custom\"}"}}]}`
	w := h.serve(snap, "openai", "/v1/chat/completions", strong, nil)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "guard.paths=block") {
		t.Errorf("strong path status = %d body = %s, want the paths block 400", w.Code, w.Body.String())
	}

	weak := secretBody("please read /secret/custom for me")
	w = h.serve(snap, "openai", "/v1/chat/completions", weak, nil)
	if w.Code != http.StatusOK {
		t.Errorf("weak path status = %d, want 200 (weak never blocks)", w.Code)
	}
	if got := h.svc.Metrics.Snapshot()[counters.PMKey{Provider: "guard", Model: "custom_path_text"}].Requests; got == 0 {
		t.Error("weak path must increment (guard, custom_path_text)")
	}
}

// TestServeGuardFragmentedSecretBlock: a known secret smuggled in two
// fragments across one session completes on the second request and is blocked
// as known_secret_fragmented.
func TestServeGuardFragmentedSecretBlock(t *testing.T) {
	up := newFakeUpstream(t, openaiOKResponder("ok"))
	h := newHarness()
	h.svc.SessionScan = guardsession.NewStore()
	g := GuardConfig{Secrets: "block", Paths: "off", SessionScan: true, KnownSecrets: true}
	snap := h.snapshot(guardBaseConfig(up, g))
	snap.Guard = guardScanner(t, nil)

	headers := map[string]string{"x-claude-code-session-id": "sess-1"}
	first := secretBody("part one: " + guardTestSecret[:16])
	if w := h.serve(snap, "openai", "/v1/chat/completions", first, headers); w.Code != http.StatusOK {
		t.Fatalf("first fragment status = %d, want 200 (incomplete)", w.Code)
	}
	second := secretBody("part two: " + guardTestSecret[16:])
	w := h.serve(snap, "openai", "/v1/chat/completions", second, headers)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "fragmented") {
		t.Fatalf("second fragment status = %d body = %s, want the fragmented block 400", w.Code, w.Body.String())
	}
	if got := h.svc.Metrics.Snapshot()[counters.PMKey{Provider: "guard", Model: "known_secret_fragmented"}].Requests; got == 0 {
		t.Error("fragmented completion must increment (guard, known_secret_fragmented)")
	}
}

// TestEvaluateRequestGuard: the pure decision — nil scanner passthrough,
// block/off channel combinations — shared by the live path and /debug/route.
func TestEvaluateRequestGuard(t *testing.T) {
	body := []byte(secretBody("token " + guardTestSecret))
	d := EvaluateRequestGuard(GuardConfig{Secrets: "block"}, nil, body)
	if len(d.Secrets) != 0 || string(d.ForwardBody) != string(body) {
		t.Errorf("nil scanner must pass through untouched: %+v", d)
	}

	sc := guardScanner(t, nil)
	d = EvaluateRequestGuard(GuardConfig{Secrets: "log", Paths: "off"}, sc, body)
	if len(d.Secrets) == 0 {
		t.Error("secrets=log must report the hit names")
	}
	if kind, _ := d.Blocks(GuardConfig{Secrets: "log"}); kind != "" {
		t.Errorf("log action never blocks, got %q", kind)
	}
	if kind, names := d.Blocks(GuardConfig{Secrets: "block"}); kind != "secret" || len(names) == 0 {
		t.Errorf("block action = %q %v, want secret", kind, names)
	}

	d = EvaluateRequestGuard(GuardConfig{Secrets: "redact", Paths: "off"}, sc, body)
	if strings.Contains(string(d.ForwardBody), guardTestSecret) {
		t.Error("redact must rewrite the secret out of the forward body")
	}

	d = EvaluateRequestGuard(GuardConfig{Secrets: "off", Paths: "log", ExtraPaths: []string{"/secret/custom"}}, guardScanner(t, []string{"/secret/custom"}), []byte(secretBody("see /secret/custom")))
	if len(d.WeakPath) == 0 {
		t.Error("paths-only scan must classify the prose hit weak")
	}
}

// TestAuditGuardHitNilLogger: nil logger (audit off for the generation) is a
// no-op, never a panic.
func TestAuditGuardHitNilLogger(t *testing.T) {
	AuditGuardHit(nil, "secret", []string{"known_secret"}, "log", "r", "a", "openai", "m")
}
