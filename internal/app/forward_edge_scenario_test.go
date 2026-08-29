package app

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestForward_CommittedSSEStreamMidFailureIsNotRecalled pins the "commit means
// no recall" end state: once an SSE stream has bytes committed to the client,
// an upstream dying mid-stream must NOT trigger failover — the client keeps
// the committed prefix, no fallback request is sent, and the post-commit death
// is not miscounted as a pre-commit hard failure (circuit untouched).
func TestForward_CommittedSSEStreamMidFailureIsNotRecalled(t *testing.T) {
	streamPrefix := "data: {\"choices\":[{\"delta\":{\"content\":\"par\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"tial\"}}]}\n\n"
	var pHits atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pHits.Add(1)
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, streamPrefix)
		if f, ok := w.(http.Flusher); ok {
			f.Flush() // commits the prefix to the proxy (and on to the client)
		}
		// Die mid-stream: httptest recovers the panic and drops the
		// connection, so the proxy sees the stream break after the prefix.
		panic("upstream died mid-stream")
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
	p, dir, shutdown := newReqLogProxy(t, cfg)
	defer shutdown()
	p.providers["primary"] = &testProv{key: "p"}
	p.providers["fallback"] = &testProv{key: "f"}
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(px.URL+"/v1/chat/completions", "application/json",
		stringReader(`{"model":"m1","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("client post: %v", err)
	}
	// The stream broke mid-transfer: the read may end in EOF or an unexpected-
	// EOF error — both are valid observations of the committed-prefix contract.
	got, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.HasPrefix(string(got), streamPrefix) {
		t.Fatalf("client did not receive the committed prefix verbatim (readErr=%v):\n%s", readErr, got)
	}

	// No recall: failover after commit must not send a fallback request.
	if got := pHits.Load(); got != 1 {
		t.Errorf("primary hits = %d, want 1", got)
	}
	if len(*fallbackSeen) != 0 {
		t.Errorf("fallback was hit %d time(s) after a COMMITTED stream died — commit must mean no recall", len(*fallbackSeen))
	}

	// Post-commit death is not a pre-commit hard failure: circuit untouched.
	if h, ok := p.runtimeState.Dashboard(time.Now()).Providers["primary"]; ok &&
		(h.ConsecutiveFailures != 0 || h.CircuitOpenUntil.After(time.Now())) {
		t.Errorf("mid-stream death poisoned the circuit: %+v", h)
	}

	// The request still reaches its terminal state: one committed end event
	// (status 200, the committed response) and a request-log record carrying
	// the partial stream the client saw.
	endStatus := 0
	for _, e := range p.events.Snapshot() {
		if e.Type == "end" && e.Provider == "primary" {
			endStatus = e.Status
		}
	}
	if endStatus != 200 {
		t.Errorf("end event status = %d, want 200 (committed)", endStatus)
	}
	deadline := time.Now().Add(2 * time.Second)
	found := false
	for time.Now().Before(deadline) && !found {
		for _, r := range allRecords(t, dir) {
			if r.Provider == "primary" && r.Status == 200 && strings.Contains(r.ResponseBody, "tial") {
				found = true
				break
			}
		}
		if !found {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if !found {
		t.Error("no request-log record for the committed partial stream")
	}
}

// TestForward_DialRefusedFailsOver covers the transport-level failure class:
// every other failover test injects an HTTP error (5xx/429/401), never a
// connection that cannot be established at all. A dial-refused primary must
// fail over to the sibling target with the client still getting a 200, and the
// network failure must land in health as a hard failure.
func TestForward_DialRefusedFailsOver(t *testing.T) {
	// Reserve a port, then close its listener: dialing it is refused.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	fallback, fallbackSeen := newCaptureUpstream(200, `{"ok":true}`)
	defer fallback.Close()

	cfg := &Config{
		Providers: map[string]Provider{
			"primary":  {OpenAIBaseURL: deadURL, Provider: testProviderID},
			"fallback": {OpenAIBaseURL: fallback.URL, Provider: testProviderID},
		},
		Routes: map[string][]RouteTarget{
			"m1": {
				{Provider: "primary", Model: "m1", Priority: 1},
				{Provider: "fallback", Model: "m1", Priority: 2},
			},
		},
		Scheduling: Scheduling{CircuitThreshold: 3, RetryWait: "0"},
	}
	p := newProxyWithStatic(t, cfg, map[string]string{"primary": "p", "fallback": "f"})
	px := httptest.NewServer(http.HandlerFunc(p.Handler))
	defer px.Close()

	code, body := post(t, px.URL+"/v1/chat/completions", `{"model":"m1","messages":[]}`)
	if code != 200 || body != `{"ok":true}` {
		t.Fatalf("dial-refused failover: status=%d body=%s, want 200 with the fallback body", code, body)
	}
	if len(*fallbackSeen) != 1 {
		t.Errorf("fallback seen %d time(s), want 1", len(*fallbackSeen))
	}
	h, ok := p.runtimeState.Dashboard(time.Now()).Providers["primary"]
	if !ok {
		t.Fatal("primary health missing after dial refusal")
	}
	if h.ConsecutiveFailures != 1 {
		t.Errorf("dial refusal recorded as %d consecutive failures, want 1 (hard failure)", h.ConsecutiveFailures)
	}
}
