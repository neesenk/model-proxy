package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"model-proxy/internal/accounts"
	"model-proxy/internal/login"
	"model-proxy/internal/observe/counters"
	runtimewire "model-proxy/internal/runtime/wirecap"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---- web_status_test.go ----

func TestAPIStatus(t *testing.T) {
	w, _ := newTestWeb(t)
	mux := http.NewServeMux()
	w.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/status", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var status map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatalf("status body is not a JSON object: %v: %s", err, rec.Body.String())
	}
	// Exact structural fields must be present (parsed key presence — a raw
	// substring would also match keys nested inside values).
	for _, want := range []string{"uptime", "version", "listen", "health", "schedule", "counters"} {
		if _, ok := status[want]; !ok {
			t.Errorf("status body missing %q: %s", want, rec.Body.String())
		}
	}
}

// TestAPIModelsEndToEnd: GET /api/models serves the startup protocol probe's
// capability matrix through the REAL chain (Proxy.modelCaps → admin port
// closure → admin projection → web transport): verdict strings, fingerprint,
// probed_at — and an empty store projecting {"providers":{}}.
func TestAPIModelsEndToEnd(t *testing.T) {
	w, p := newTestWeb(t)

	// Empty store → {"providers":{}} (non-null object).
	rec := httptest.NewRecorder()
	serveWeb(w, rec, httptest.NewRequest("GET", "/api/models", nil))
	if rec.Code != 200 {
		t.Fatalf("empty store: status=%d want 200", rec.Code)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != `{"providers":{}}` {
		t.Fatalf("empty store body = %s, want {\"providers\":{}}", body)
	}

	probed := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	p.modelCaps.Put("zhipu", "0123456789abcdef", "glm",
		runtimewire.ModelProtocols{Chat: triYes, Anthropic: triNo, Responses: triUnknown}, probed)

	rec = httptest.NewRecorder()
	serveWeb(w, rec, httptest.NewRequest("GET", "/api/models", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var got struct {
		Providers map[string]struct {
			Fingerprint string `json:"fingerprint"`
			ProbedAt    string `json:"probed_at"`
			Models      map[string]struct {
				Chat      string `json:"chat"`
				Anthropic string `json:"anthropic"`
				Responses string `json:"responses"`
			} `json:"models"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("body is not the models document: %v: %s", err, rec.Body.String())
	}
	zhipu, ok := got.Providers["zhipu"]
	if !ok || zhipu.Fingerprint != "0123456789abcdef" || zhipu.ProbedAt != "2026-09-01T10:00:00Z" {
		t.Fatalf("providers[zhipu] = %+v (%s)", zhipu, rec.Body.String())
	}
	if m := zhipu.Models["glm"]; m.Chat != "yes" || m.Anthropic != "no" || m.Responses != "unknown" {
		t.Errorf("models[glm] = %+v, want yes/no/unknown verdict strings", m)
	}
}

// TestAPIStatusCacheField: /api/status exposes cache observability — with
// cache.enabled on, the cache object carries enabled + hits/misses/entries;
// with the cache off (default), it reports enabled:false.
func TestAPIStatusCacheField(t *testing.T) {
	// Disabled path (newTestWeb's config has no cache block).
	w, _ := newTestWeb(t)
	mux := http.NewServeMux()
	w.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/status", nil))
	var off struct {
		Cache struct {
			Enabled bool `json:"enabled"`
		} `json:"cache"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &off); err != nil {
		t.Fatalf("parse status (off): %v", err)
	}
	if off.Cache.Enabled {
		t.Errorf("cache.enabled=true want false (cache disabled): %s", rec.Body.String())
	}

	// Enabled path with one recorded hit + one stored entry.
	cfg, _ := LoadConfigFromBytes("test", []byte(`listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x}
cache: {enabled: true, ttl: 1h}
`))
	p := newTestProxy(t, cfg)
	if p.cache == nil {
		t.Fatal("cache not created despite cache.enabled")
	}
	now := time.Now()
	p.cache.Put("k", "m", http.StatusOK, nil, []byte("x"), now)
	if _, ok := p.cache.Lookup("k", "m", now); !ok {
		t.Fatal("seeded cache entry should hit")
	}
	w2 := NewWebServer(p, "test-config.yaml")
	mux2 := http.NewServeMux()
	w2.Register(mux2)
	rec2 := httptest.NewRecorder()
	mux2.ServeHTTP(rec2, httptest.NewRequest("GET", "/api/status", nil))
	var on struct {
		Cache struct {
			Enabled bool   `json:"enabled"`
			Hits    uint64 `json:"hits"`
			Misses  uint64 `json:"misses"`
			Entries uint64 `json:"entries"`
			Models  []struct {
				Model   string `json:"model"`
				Hits    uint64 `json:"hits"`
				Misses  uint64 `json:"misses"`
				Entries uint64 `json:"entries"`
			} `json:"models"`
		} `json:"cache"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &on); err != nil {
		t.Fatalf("parse status (on): %v", err)
	}
	if !on.Cache.Enabled || on.Cache.Hits != 1 || on.Cache.Entries != 1 {
		t.Errorf("cache status = %+v want enabled=true hits=1 entries=1: %s", on.Cache, rec2.Body.String())
	}
}

// TestAPIStatusModelLocks: /api/status exposes ACTIVE model locks (provider →
// [{model, until}]) so `doctor --live` can explain a route whose targets are
// locked out. Expired locks are omitted — same future-only convention as
// circuit_until / rate_limited_until.
func TestAPIStatusModelLocks(t *testing.T) {
	w, p := newTestWeb(t)
	mux := http.NewServeMux()
	w.Register(mux)
	p.recordModelFailure("zhipu", "glm-x", Scheduling{ModelLockout: "1h"})
	p.runtimeState.RecordModelFailure("zhipu", "old", -time.Minute, 0)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/status", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var out struct {
		ModelLocks map[string][]struct {
			Model string `json:"model"`
			Until string `json:"until"`
		} `json:"model_locks"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("parse status: %v", err)
	}
	locks := out.ModelLocks["zhipu"]
	if len(locks) != 1 || locks[0].Model != "glm-x" {
		t.Fatalf("model_locks = %+v, want exactly the active (zhipu, glm-x) lock", out.ModelLocks)
	}
	until, err := time.Parse(time.RFC3339, locks[0].Until)
	if err != nil || !time.Now().Before(until) {
		t.Errorf("until = %q, want a future RFC3339 time", locks[0].Until)
	}
}

// TestAPIStatusUnknown404 asserts the catch-all still 404s for unknown /api paths
// once the first real route (/api/status) is wired. Guards against a future
// router change silently swallowing unknown paths.
func TestAPIStatusUnknown404(t *testing.T) {
	w, _ := newTestWeb(t)
	mux := http.NewServeMux()
	w.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/no-such-route", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown /api path status=%d want 404", rec.Code)
	}
}

// TestAPITokens verifies /api/tokens returns the snapshot and /api/tokens/reset
// zeros it. The snapshot key shape is {provider, model} → {input, output, ...}.
func TestAPITokens(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	w, p := newTestWeb(t)
	p.tokens.Commit(counters.TokenKey{Provider: "zhipu", Model: "glm-5"}, counters.TokenUsage{Input: 30, Output: 12})

	mux := http.NewServeMux()
	w.Register(mux)

	// GET /api/tokens returns the committed usage.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/tokens", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"input":30`) || !strings.Contains(rec.Body.String(), `"output":12`) {
		t.Errorf("tokens body missing committed usage: %s", rec.Body.String())
	}

	// POST /api/tokens/reset clears the counter.
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, httptest.NewRequest("POST", "/api/tokens/reset", nil))
	if rec2.Code != 200 {
		t.Fatalf("reset status=%d want 200", rec2.Code)
	}
	if len(p.tokens.Snapshot()) != 0 {
		t.Errorf("after reset, snapshot non-empty: %+v", p.tokens.Snapshot())
	}
}

// TestAPIQuotaRefresh verifies POST /api/quota/refresh triggers an immediate
// quota poll of every provider (so the Web UI's "Refresh usage" button re-polls
// on demand instead of waiting for the next interval). A counting provider
// asserts Quota() was actually invoked - a 200-only check would miss a no-op.
func TestAPIQuotaRefresh(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	w, p := newTestWeb(t)
	// Inject a counting provider; the background poll goroutine is debounced by
	// its 10s bootstrap delay, so calls here are from the refresh endpoint.
	var calls atomic.Int32
	prov := &quotaCallProv{calls: &calls}
	p.providers["zhipu"] = prov
	mux := http.NewServeMux()
	w.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/quota/refresh", nil))
	if rec.Code != 200 {
		t.Fatalf("refresh status=%d want 200, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "refreshed") {
		t.Errorf("body=%s want refreshed", rec.Body.String())
	}
	// The endpoint runs pollAll synchronously, so the Quota() call has landed by
	// the time the response returns.
	if got := calls.Load(); got < 1 {
		t.Errorf("refresh did not poll: Quota() called %d times, want >=1", got)
	}
	// The snapshot is populated (proves the poll result was stored).
	if s := p.quota.Snapshot("zhipu"); s == nil || s.RemainingPct != 0.5 {
		t.Errorf("after refresh, snapshot=%+v want RemainingPct 0.5", s)
	}
}

// TestAPIQuotaRefreshOne verifies POST /api/quota/refresh {"provider":"<key>"}
// polls ONLY that one account's provider (not every provider). Mirrors the
// per-account "Refresh usage" button: the provider key is a config name or a
// pooled-account virtual id. Asserts the named provider was polled AND another
// was not - a 200-only check would miss a "poll all" regression. An unknown key
// is a 404.
func TestAPIQuotaRefreshOne(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	w, p := newTestWeb(t)
	var zhipuCalls, deepseekCalls atomic.Int32
	p.providers["zhipu"] = &quotaCallProv{calls: &zhipuCalls}
	p.providers["deepseek"] = &quotaCallProv{calls: &deepseekCalls}
	mux := http.NewServeMux()
	w.Register(mux)

	// Poll just zhipu.
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/quota/refresh",
		strings.NewReader(`{"provider":"zhipu"}`)))
	if rec.Code != 200 {
		t.Fatalf("refresh-one status=%d want 200, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"provider":"zhipu"`) {
		t.Errorf("body=%s want provider echo", rec.Body.String())
	}
	if got := zhipuCalls.Load(); got != 1 {
		t.Errorf("zhipu Quota() called %d times, want 1", got)
	}
	// deepseek must NOT have been polled - this is the single-account contract.
	if got := deepseekCalls.Load(); got != 0 {
		t.Errorf("deepseek Quota() called %d times, want 0 (single-account refresh)", got)
	}

	// Unknown provider key -> 404, nothing polled.
	before := zhipuCalls.Load()
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, httptest.NewRequest("POST", "/api/quota/refresh",
		strings.NewReader(`{"provider":"ghost"}`)))
	if rec2.Code != 404 {
		t.Errorf("unknown provider status=%d want 404, body=%s", rec2.Code, rec2.Body.String())
	}
	if got := zhipuCalls.Load(); got != before {
		t.Errorf("unknown-provider refresh polled zhipu %d extra times", got-before)
	}
}
func TestAPILogs(t *testing.T) {
	tmp := t.TempDir() + "/model-proxy.log"
	logContent := "line1\nline2\nline3\nline4\nline5\n"
	if err := os.WriteFile(tmp, []byte(logContent), 0o644); err != nil {
		t.Fatal(err)
	}
	w, _ := newTestWeb(t)
	w.logFile = tmp // override hook for tests

	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	w.Register(mux)
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/logs?tail=2", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	got := rec.Body.String()
	// last 2 lines
	if !strings.Contains(got, "line4") || !strings.Contains(got, "line5") {
		t.Errorf("tail=2 missing last lines: %q", got)
	}
	if strings.Contains(got, "line1") {
		t.Errorf("tail=2 should drop line1: %q", got)
	}
}

// TestAPIStatusCredentialStore pins the S1 observability closeout: /api/status
// carries credential_store naming the resolved credstore backend. Test binaries
// resolve to file mode (credstore hermeticity guard), which the assertion pins.
func TestAPIStatusCredentialStore(t *testing.T) {
	w, _ := newTestWeb(t)
	mux := http.NewServeMux()
	w.Register(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/status", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d want 200", rec.Code)
	}
	var v struct {
		CredentialStore string `json:"credential_store"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("parse status: %v", err)
	}
	if v.CredentialStore != "file" {
		t.Fatalf("credential_store = %q, want \"file\" under the test-binary guard", v.CredentialStore)
	}
}

// ---- web_lifecycle_test.go ----

func TestWebCloseCancelsAqpLoginBeforeCredentialCommit(t *testing.T) {
	setPoolHome(t, t.TempDir())
	pollStarted := make(chan struct{})
	pollCancelled := make(chan struct{})

	w, p := newTestWeb(t)
	p.mu.Lock()
	p.cfg.Providers["aqp"] = Provider{Provider: "aqp", OpenAIBaseURL: "https://unused.invalid"}
	p.mu.Unlock()
	w.newAqpClientFn = func(storePath string) *login.AqpClient {
		client := login.NewAqpClient(storePath)
		client.Base = "https://aqp.invalid"
		client.HTTP.Transport = webLifecycleRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
			switch req.URL.Path {
			case login.AqpAuthLoginPath:
				return webLifecycleResponse(req, http.StatusUnauthorized, `{"result":"https://login.invalid"}`), nil
			case login.AqpAuthInfoPath:
				close(pollStarted)
				<-req.Context().Done()
				close(pollCancelled)
				return nil, req.Context().Err()
			default:
				t.Errorf("unexpected AQP request path %q", req.URL.Path)
				return webLifecycleResponse(req, http.StatusInternalServerError, `{}`), nil
			}
		})
		return client
	}

	rec := httptest.NewRecorder()
	serveWeb(w, rec, httptest.NewRequest(http.MethodPost, "/api/login/aqp/start", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("start status = %d, body = %s", rec.Code, rec.Body.String())
	}
	sessionID := decodeLoginSessionID(t, rec)
	waitForSignal(t, pollStarted, "AQP poll did not start")

	closeDone := make(chan struct{})
	go func() {
		w.Close()
		close(closeDone)
	}()
	waitForSignal(t, pollCancelled, "AQP poll request was not cancelled")
	waitForSignal(t, closeDone, "Web close did not join the cancelled AQP poll")

	assertLoginCancelledWithoutCredential(t, w, sessionID, accounts.AuthFilePath("aqp", "oauth_auth"))
}

func TestWebCloseCancelsCodexLoginBeforeCredentialCommit(t *testing.T) {
	setPoolHome(t, t.TempDir())
	pollStarted := make(chan struct{})
	pollCancelled := make(chan struct{})

	w, p := newTestWeb(t)
	p.mu.Lock()
	p.cfg.Providers["codex"] = Provider{Provider: "codex", OpenAIBaseURL: "https://unused.invalid"}
	p.mu.Unlock()
	w.newCodexOptions = func() *login.CodexLoginServerOptions {
		return &login.CodexLoginServerOptions{
			UsercodeURL:  "https://auth.invalid/usercode",
			DeviceTokURL: "https://auth.invalid/devtok",
			TokenURL:     "https://auth.invalid/token",
			HTTPClient: &http.Client{Transport: webLifecycleRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
				switch req.URL.Path {
				case "/usercode":
					return webLifecycleResponse(req, http.StatusOK, `{"device_auth_id":"device","user_code":"CODE","interval":"60"}`), nil
				case "/devtok":
					close(pollStarted)
					<-req.Context().Done()
					close(pollCancelled)
					return nil, req.Context().Err()
				default:
					t.Errorf("unexpected Codex request path %q", req.URL.Path)
					return webLifecycleResponse(req, http.StatusInternalServerError, `{}`), nil
				}
			})},
		}
	}

	rec := httptest.NewRecorder()
	serveWeb(w, rec, httptest.NewRequest(http.MethodPost, "/api/login/codex/start", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("start status = %d, body = %s", rec.Code, rec.Body.String())
	}
	sessionID := decodeLoginSessionID(t, rec)
	waitForSignal(t, pollStarted, "Codex poll did not start")

	closeDone := make(chan struct{})
	go func() {
		w.Close()
		close(closeDone)
	}()
	waitForSignal(t, pollCancelled, "Codex poll request was not cancelled")
	waitForSignal(t, closeDone, "Web close did not join the cancelled Codex poll")

	assertLoginCancelledWithoutCredential(t, w, sessionID, accounts.AuthFilePath("codex", "oauth_auth"))
}

type webLifecycleRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f webLifecycleRoundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func webLifecycleResponse(req *http.Request, status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}
}

func decodeLoginSessionID(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var response struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode login start response: %v", err)
	}
	if response.SessionID == "" {
		t.Fatal("login start response has no session_id")
	}
	return response.SessionID
}

func waitForSignal(t *testing.T, signal <-chan struct{}, failure string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal(failure)
	}
}

func assertLoginCancelledWithoutCredential(t *testing.T, w *WebServer, sessionID, path string) {
	t.Helper()
	recorder := httptest.NewRecorder()
	serveWeb(w,
		recorder,
		httptest.NewRequest(http.MethodGet, "/api/login/"+sessionID+"/poll", nil),
	)
	if recorder.Code != http.StatusOK {
		t.Fatalf(
			"cancelled login poll status = %d, body = %s",
			recorder.Code,
			recorder.Body.String(),
		)
	}
	var session struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &session); err != nil {
		t.Fatalf("decode cancelled login poll: %v", err)
	}
	if session.State != "error" {
		t.Fatalf("cancelled login state = %q, want error", session.State)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credential was persisted before commit boundary: stat error = %v", err)
	}
}

// ---- web_events_route_test.go ----

// newWebMux mirrors the production composition in NewRuntime: proxy.Handler on
// "/" plus the web transport's /ui/ and /api/ subtrees (web.enabled default).
func newWebMux(t *testing.T, proxy *Proxy) *http.ServeMux {
	t.Helper()
	w := NewWebServer(proxy, "test-config.yaml")
	mux := http.NewServeMux()
	mux.HandleFunc("/", proxy.Handler)
	w.Register(mux)
	return mux
}

// TestMuxServesAPIEventsWithWebEnabled: with web.enabled (the default), GET
// /api/events must reach the SSE stream, not the web layer's 404 default.
// ServeMux dispatches /api/events to the more specific "/api/" subtree, so the
// branch inside proxy.Handler is only reachable with web disabled — the Live
// tab was dead in every default deployment (EventSource stuck reconnecting).
func TestMuxServesAPIEventsWithWebEnabled(t *testing.T) {
	proxy := newTestProxy(t, &Config{
		Providers: map[string]Provider{"z": {OpenAIBaseURL: "http://x", Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"glm": {{Provider: "z", Model: "glm"}}},
	})
	mux := newWebMux(t, proxy)

	// The SSE handler runs on the server's own goroutine and writes headers while
	// this test observes them — a shared httptest.ResponseRecorder is NOT
	// thread-safe for that (data race on its header map). Serving through a real
	// httptest.Server keeps the assertion honest: net/http serializes every
	// header write before the bytes hit the wire, so client.Do returning means
	// the terminal status + content-type are already decided — no polling needed.
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/events", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	// Exactly what the UI Live tab's EventSource sends: a same-origin browser
	// request against the loopback listener.
	req.Header.Set("Origin", "http://127.0.0.1:15721")
	req.Host = "127.0.0.1:15721"
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /api/events: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /api/events status = %d body=%q, want 200 (SSE stream)", resp.StatusCode, body)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	// Cancelling the request context must end the SSE stream: read until EOF
	// with a deadline-ish bound via ctx (already cancelled below) — the server
	// side handler returns on r.Context().Done(), closing the response body.
	cancel()
	if _, err := io.ReadAll(resp.Body); err != nil && ctx.Err() == nil {
		t.Fatalf("read SSE stream after cancel: %v", err)
	}
}

// TestMuxAPIEventsGuardedAgainstRebinding: a DNS-rebinding-shaped browser
// request (non-loopback Host) must be rejected on the events endpoint too —
// the live stream carries agent/model/provider metadata, so it joins the same
// loopback trust boundary as the rest of the admin API.
func TestMuxAPIEventsGuardedAgainstRebinding(t *testing.T) {
	proxy := newTestProxy(t, &Config{
		Providers: map[string]Provider{"z": {OpenAIBaseURL: "http://x", Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"glm": {{Provider: "z", Model: "glm"}}},
	})
	mux := newWebMux(t, proxy)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx)
	req.Header.Set("Origin", "http://evil.example")
	req.Host = "evil.example"
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /api/events with rebinding Host status = %d, want 403", rec.Code)
	}
}

// TestDebugScheduleGuardedAgainstRebinding: /debug/schedule exposes routing,
// sticky-session and pin metadata; a DNS-rebinding-shaped browser request
// (non-loopback Host) must be rejected exactly like the admin API — it rides
// the same proxy handler but must not escape the loopback trust boundary.
func TestDebugScheduleGuardedAgainstRebinding(t *testing.T) {
	proxy := newTestProxy(t, &Config{
		Providers: map[string]Provider{"z": {OpenAIBaseURL: "http://x", Provider: testProviderID}},
		Routes:    map[string][]RouteTarget{"glm": {{Provider: "z", Model: "glm"}}},
	})
	req := httptest.NewRequest(http.MethodGet, "/debug/schedule", nil)
	req.Header.Set("Origin", "http://evil.example")
	req.Host = "evil.example"
	rec := httptest.NewRecorder()
	proxy.Handler(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("GET /debug/schedule with rebinding Host status = %d body=%q, want 403", rec.Code, rec.Body.String())
	}
	// Plain local CLI/curl (no browser identity headers) keeps full access.
	req2 := httptest.NewRequest(http.MethodGet, "/debug/schedule", nil)
	rec2 := httptest.NewRecorder()
	proxy.Handler(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("GET /debug/schedule without browser headers status = %d, want 200", rec2.Code)
	}
}

// ---- web_reload_test.go ----

// TestHandleAccountAdd_ReloadFailureWarning (bug 8): a credential mutation is
// persisted to the pool file BEFORE reload, so a reload failure (config.yaml
// unreadable/invalid — the mutation itself never touches config.yaml) must NOT
// be swallowed as a false success. The handler keeps 2xx (the account IS saved)
// but surfaces a `warning` so the UI can tell the user the runtime is stale until
// config.yaml is fixed + reloaded. The error is also logged for troubleshooting.
func TestHandleAccountAdd_ReloadFailureWarning(t *testing.T) {
	setPoolHome(t, t.TempDir())
	// openai_base_url with no usage_url now validates via the apiKeyValidationURL
	// fallback (GET openai_base_url/models); point it at a reachable mock so the
	// add succeeds and the reload-failure warning path is what's under test.
	valSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer valSrv.Close()
	cfg, _ := LoadConfigFromBytes("test", []byte("listen: 127.0.0.1:0\nproviders:\n  zhipu: {provider_id: zhipu, openai_base_url: "+valSrv.URL+"}\n"))
	p := newTestProxy(t, cfg)
	// configFile points at a non-existent path → reload's LoadConfig read fails.
	w := NewWebServer(p, "/no/such/config.yaml")
	mux := http.NewServeMux()
	w.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/accounts/zhipu",
		strings.NewReader(`{"api_key":"sk-test-warning"}`)))
	if rec.Code != 200 {
		t.Fatalf("status=%d want 200 (account persists even when reload fails): %s", rec.Code, rec.Body.String())
	}
	var out struct {
		ID, Status, Warning string
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Status != "added" {
		t.Errorf("status=%q want added", out.Status)
	}
	if out.Warning == "" {
		t.Errorf("reload failed but response carries no warning; UI would show false success: %s", rec.Body.String())
	}
}

// ---- web_ui_test.go ----

func TestWebServesUI(t *testing.T) {
	w, _ := newTestWeb(t)
	mux := http.NewServeMux()
	w.Register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/ui/", nil))
	if rec.Code != 200 {
		t.Fatalf("GET /ui/ status=%d want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<!doctype html>") {
		t.Errorf("/ui/ body missing doctype: %q", rec.Body.String())
	}

	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, httptest.NewRequest("GET", "/ui/app.js", nil))
	if rec2.Code != 200 {
		t.Fatalf("GET /ui/app.js status=%d want 200", rec2.Code)
	}

	// app.js is an ES module importing './pure.js' — that target must be
	// served too, with the JS content type, or the SPA fails to load.
	rec3 := httptest.NewRecorder()
	mux.ServeHTTP(rec3, httptest.NewRequest("GET", "/ui/pure.js", nil))
	if rec3.Code != 200 || !strings.Contains(rec3.Header().Get("content-type"), "javascript") {
		t.Fatalf("GET /ui/pure.js status=%d content-type=%q — the app.js import target must be served", rec3.Code, rec3.Header().Get("content-type"))
	}
}

// ---- web_test_support_test.go ----

func newTestWeb(t *testing.T) (*WebServer, *Proxy) {
	t.Helper()
	cfg, _ := LoadConfigFromBytes("test", []byte(`listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x}
`))
	p := newTestProxy(t, cfg)
	return NewWebServer(p, "test-config.yaml"), p
}

// serveWeb dispatches one request through the mux the WebServer registers on —
// the same routing production uses — for tests that exercise endpoints without
// a listener.
func serveWeb(w *WebServer, recorder *httptest.ResponseRecorder, req *http.Request) {
	mux := http.NewServeMux()
	w.Register(mux)
	mux.ServeHTTP(recorder, req)
}
