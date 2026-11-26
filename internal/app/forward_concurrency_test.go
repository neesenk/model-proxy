package app

import (
	"bytes"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	configdomain "model-proxy/internal/config"
	runtimestate "model-proxy/internal/runtime"
	"model-proxy/internal/testsyn"
)

// serveLog is a log sink safe to read while request goroutines still write.
type serveLog struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *serveLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *serveLog) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func captureServeLog(t *testing.T) *serveLog {
	t.Helper()
	sink := &serveLog{}
	original := log.Writer()
	log.SetOutput(sink)
	t.Cleanup(func() { log.SetOutput(original) })
	return sink
}

// postUnstuck sends one request with the per-request sticky opt-out, so every
// request is scheduled from scratch by priority.
func postUnstuck(t *testing.T, url, body string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-mp-sticky", "off")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(got), resp.Header
}

// concurrencyRoute is a two-provider route — a first by priority, b the
// fallback — served by the given upstreams.
func concurrencyRoute(t *testing.T, a, b *fakeUpstream) (*Proxy, *httptest.Server) {
	t.Helper()
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"a": {OpenAIBaseURL: a.srv.URL, Provider: testProviderID},
			"b": {OpenAIBaseURL: b.srv.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{"m1": {
			{Provider: "a", Model: "m1", Priority: 1},
			{Provider: "b", Model: "m1", Priority: 2},
		}},
	}
	proxy, _, _ := newReqLogProxy(t, cfg)
	proxy.providers["a"] = &testProv{key: "a"}
	proxy.providers["b"] = &testProv{key: "b"}
	server := httptest.NewServer(http.HandlerFunc(proxy.Handler))
	t.Cleanup(server.Close)
	return proxy, server
}

// holder is a request parked inside upstream a, occupying its only slot
// until released.
type holder struct {
	release chan struct{}
	once    sync.Once
	arrived chan struct{}
}

func newHolder(t *testing.T) *holder {
	h := &holder{release: make(chan struct{}), arrived: make(chan struct{}, 1)}
	t.Cleanup(h.free)
	return h
}

// hold is the upstream behaviour: the holderHit-th request is the holder's
// own — it signals its arrival, then blocks until freed. later serves every
// other request.
func (h *holder) hold(holderHit int, later http.HandlerFunc) func(hit int, w http.ResponseWriter, r *http.Request) {
	return func(hit int, w http.ResponseWriter, r *http.Request) {
		if hit != holderHit {
			later(w, r)
			return
		}
		h.arrived <- struct{}{}
		<-h.release
		staticResponder(http.StatusOK, `{"ok":"holder"}`)(w, r)
	}
}

func (h *holder) free() { h.once.Do(func() { close(h.release) }) }

const concurrencyChatBody = `{"model":"m1","messages":[{"role":"user","content":"ping"}]}`

// TestForward_SaturatedAccountIsSkippedWithoutUpstreamCall: an account whose
// concurrency limit is known is routed around while its slots are full — the
// scheduler never hands it the request, so no upstream round trip is spent
// on a 429. The limit comes from the upstream's own 429, which caps the
// account without benching it for a cooldown.
func TestForward_SaturatedAccountIsSkippedWithoutUpstreamCall(t *testing.T) {
	logs := captureServeLog(t)
	held := newHolder(t)
	defer held.free()
	a := newHitFakeUpstream(t, func(hit int, w http.ResponseWriter, r *http.Request) {
		if hit == 1 {
			staticResponder(http.StatusTooManyRequests, `{"error":{"message":"concurrency reached, current: 2, limit: 1","type":"rate_limited"}}`)(w, r)
			return
		}
		held.hold(2, staticResponder(http.StatusOK, `{"ok":"direct"}`))(hit, w, r)
	})
	b := newFakeUpstream(t, staticResponder(http.StatusOK, `{"ok":"fallback"}`))
	proxy, server := concurrencyRoute(t, a, b)
	url := server.URL + "/v1/chat/completions"

	// The upstream states its limit on a 429: the request fails over and the
	// account is capped at one concurrent request.
	status, got, _ := postUnstuck(t, url, concurrencyChatBody)
	if status != http.StatusOK || !strings.Contains(got, "fallback") {
		t.Fatalf("first request = %d %s, want the fallback's 200", status, got)
	}
	if cap := proxy.runtimeState.Concurrency("a"); cap != (runtimestate.ConcurrencyStatus{Cap: 1, Stated: true}) {
		t.Fatalf("concurrency of a = %+v, want a stated cap of 1", cap)
	}
	if !strings.Contains(logs.text(), "429 states a concurrency limit of 1 — dispatch capped") {
		t.Errorf("serve.log does not name the learned limit:\n%s", logs.text())
	}

	// One request takes the single slot and stays there.
	holderDone := make(chan int, 1)
	go func() {
		status, _, _ := postUnstuck(t, url, concurrencyChatBody)
		holderDone <- status
	}()
	<-held.arrived

	// The next request goes straight to the sibling: a is never contacted.
	status, got, _ = postUnstuck(t, url, concurrencyChatBody)
	if status != http.StatusOK || !strings.Contains(got, "fallback") {
		t.Fatalf("request beside a saturated account = %d %s, want the fallback's 200", status, got)
	}
	if a.hits() != 2 {
		t.Errorf("upstream a hits = %d, want 2 (the 429 and the holder); a saturated account must not be contacted", a.hits())
	}

	// The slot frees with the holder, and the account takes traffic again.
	held.free()
	if status := <-holderDone; status != http.StatusOK {
		t.Fatalf("holder = %d, want 200", status)
	}
	status, got, _ = postUnstuck(t, url, concurrencyChatBody)
	if status != http.StatusOK || !strings.Contains(got, "direct") {
		t.Fatalf("request after the slot freed = %d %s, want a's 200", status, got)
	}
	// The slot goes back once the response is fully written: the client can
	// read the last byte a moment before the executor's deferred release runs.
	testsyn.WaitUntil(t, "a's slot to be released", func() bool {
		return proxy.runtimeState.Concurrency("a") == (runtimestate.ConcurrencyStatus{Cap: 1, Stated: true})
	})
}

// TestForward_PinDispatchesIntoASaturatedAccount: a pin is a hard choice.
// The pinned account gets the request even at its limit — no redirect, no
// wait — and the request still occupies a slot while it runs.
func TestForward_PinDispatchesIntoASaturatedAccount(t *testing.T) {
	held := newHolder(t)
	defer held.free()
	a := newHitFakeUpstream(t, held.hold(1, staticResponder(http.StatusOK, `{"ok":"pinned"}`)))
	b := newFakeUpstream(t, staticResponder(http.StatusOK, `{"ok":"fallback"}`))
	proxy, server := concurrencyRoute(t, a, b)
	url := server.URL + "/v1/chat/completions"
	proxy.runtimeState.NoteConcurrencyLimit("a", 1, 0, proxy.runtimeState.Generation())
	proxy.runtimeState.SetPin("m1", runtimestate.Pin{Provider: "a"})

	holderDone := make(chan int, 1)
	go func() {
		status, _, _ := postUnstuck(t, url, concurrencyChatBody)
		holderDone <- status
	}()
	<-held.arrived

	status, got, _ := postUnstuck(t, url, concurrencyChatBody)
	if status != http.StatusOK || !strings.Contains(got, "pinned") {
		t.Fatalf("pinned request into a saturated account = %d %s, want a's own 200", status, got)
	}
	if a.hits() != 2 || b.hits() != 0 {
		t.Errorf("hits a/b = %d/%d, want 2/0 (dispatched into the pin, never redirected)", a.hits(), b.hits())
	}
	held.free()
	if status := <-holderDone; status != http.StatusOK {
		t.Fatalf("holder = %d, want 200", status)
	}
}

// TestForward_AllTargetsSaturatedWaitsForASlot: when every target is at its
// limit the request parks, and the request that frees a slot wakes it — it
// is served, not answered 429.
func TestForward_AllTargetsSaturatedWaitsForASlot(t *testing.T) {
	logs := captureServeLog(t)
	held := newHolder(t)
	defer held.free()
	a := newHitFakeUpstream(t, held.hold(1, staticResponder(http.StatusOK, `{"ok":"after-slot"}`)))
	proxy, _, _ := newReqLogProxy(t, &configdomain.Config{
		Providers:  map[string]configdomain.Provider{"a": {OpenAIBaseURL: a.srv.URL, Provider: testProviderID}},
		Routes:     map[string][]configdomain.RouteTarget{"m1": {{Provider: "a", Model: "m1"}}},
		Scheduling: configdomain.Scheduling{RetryWait: "5s"},
	})
	proxy.providers["a"] = &testProv{key: "a"}
	proxy.runtimeState.NoteConcurrencyLimit("a", 1, 0, proxy.runtimeState.Generation())
	server := httptest.NewServer(http.HandlerFunc(proxy.Handler))
	t.Cleanup(server.Close)
	url := server.URL + "/v1/chat/completions"

	holderDone := make(chan int, 1)
	go func() {
		status, _, _ := postUnstuck(t, url, concurrencyChatBody)
		holderDone <- status
	}()
	<-held.arrived

	second := make(chan int, 1)
	go func() {
		status, body, _ := postUnstuck(t, url, concurrencyChatBody)
		if !strings.Contains(body, "after-slot") {
			status = http.StatusTeapot
		}
		second <- status
	}()
	// The second request is parked once serve.log reports the wait; only then
	// does freeing the holder prove the wake-up.
	testsyn.WaitUntil(t, "the saturated wait to begin", func() bool {
		return strings.Contains(logs.text(), "every target is at its concurrency limit — waiting up to 5s for a slot")
	})
	held.free()

	if status := <-holderDone; status != http.StatusOK {
		t.Fatalf("holder = %d, want 200", status)
	}
	if status := <-second; status != http.StatusOK {
		t.Fatalf("request parked on a saturated route = %d, want 200 after a slot freed", status)
	}
	if a.hits() != 2 {
		t.Errorf("upstream a hits = %d, want 2 (holder, then the woken request)", a.hits())
	}
}

// TestForward_AllTargetsSaturatedAnswers429AfterTheWaits: a slot that never
// frees within the budget ends in 429 with a one-second Retry-After.
func TestForward_AllTargetsSaturatedAnswers429AfterTheWaits(t *testing.T) {
	held := newHolder(t)
	defer held.free()
	a := newHitFakeUpstream(t, held.hold(1, staticResponder(http.StatusOK, `{"ok":"later"}`)))
	proxy, _, _ := newReqLogProxy(t, &configdomain.Config{
		Providers:  map[string]configdomain.Provider{"a": {OpenAIBaseURL: a.srv.URL, Provider: testProviderID}},
		Routes:     map[string][]configdomain.RouteTarget{"m1": {{Provider: "a", Model: "m1"}}},
		Scheduling: configdomain.Scheduling{RetryWait: "20ms"},
	})
	proxy.providers["a"] = &testProv{key: "a"}
	proxy.runtimeState.NoteConcurrencyLimit("a", 1, 0, proxy.runtimeState.Generation())
	server := httptest.NewServer(http.HandlerFunc(proxy.Handler))
	t.Cleanup(server.Close)
	url := server.URL + "/v1/chat/completions"

	holderDone := make(chan int, 1)
	go func() {
		status, _, _ := postUnstuck(t, url, concurrencyChatBody)
		holderDone <- status
	}()
	<-held.arrived

	status, _, header := postUnstuck(t, url, concurrencyChatBody)
	if status != http.StatusTooManyRequests || header.Get("Retry-After") != "1" {
		t.Fatalf("request on a route that stayed saturated = %d Retry-After %q, want 429 with Retry-After 1", status, header.Get("Retry-After"))
	}
	if a.hits() != 1 {
		t.Errorf("upstream a hits = %d, want 1 (only the holder; the parked request was never sent)", a.hits())
	}
	held.free()
	if status := <-holderDone; status != http.StatusOK {
		t.Fatalf("holder = %d, want 200", status)
	}
}
