package app

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/testsyn"
)

// TestForward_ClientStopsReadingIsBoundedByUpstreamTimeout: the production
// path end to end — proxy handler, request log, forward's byte-counting
// writer, the executor's timing writer, the connection. A peer that keeps
// its connection but reads nothing parked two handlers for 8h+ on
// 2026-11-26 (they only ended on a restart's forced close). The account's
// in-flight slot must come back once the request's upstream timeout (plus the
// tail grace) has expired, the account must stay schedulable (a stalled
// client is not an upstream fault), and serve.log must name the stopped
// reader.
func TestForward_ClientStopsReadingIsBoundedByUpstreamTimeout(t *testing.T) {
	logs := captureServeLog(t)
	const budget = 250 * time.Millisecond
	goroutines := testsyn.Goroutines()
	t.Cleanup(func() { testsyn.LeakCheck(t, goroutines, "stalled-client forward") })

	// Streaming requests get an endless stream; anything else is served
	// normally, so the account's schedulability can be probed afterwards.
	frame := bytes.Repeat([]byte(`data: {"id":"c1","choices":[{"index":0,"delta":{"content":"hi"}}]}`+"\n\n"), 512)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"stream":true`) {
			staticResponder(http.StatusOK, `{"ok":"direct"}`)(w, r)
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		for {
			if _, err := w.Write(frame); err != nil {
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			select {
			case <-r.Context().Done():
				return
			default:
			}
		}
	}))
	t.Cleanup(upstream.Close)

	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{
			"a": {OpenAIBaseURL: upstream.URL, Provider: testProviderID},
		},
		Routes: map[string][]configdomain.RouteTarget{"m1": {
			{Provider: "a", Model: "m1", Priority: 1},
		}},
		Scheduling: configdomain.Scheduling{UpstreamTimeout: budget.String(), StreamKeepalive: "20ms"},
	}
	proxy, _, _ := newReqLogProxy(t, cfg)
	proxy.providers["a"] = &testProv{key: "a"}

	// The stalled peer connects over an unbuffered pipe: its first response
	// byte is already blocked, with no kernel socket buffers to fill first.
	listener := testsyn.NewSilentPeerListener()
	finished := make(chan struct{}, 1)
	server := &httptest.Server{
		Listener: listener,
		Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			proxy.Handler(w, r)
			finished <- struct{}{}
		})},
	}
	server.Start()
	t.Cleanup(server.Close)

	client, err := listener.Connect()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	body := `{"model":"m1","stream":true,"messages":[{"role":"user","content":"ping"}]}`
	sent := time.Now()
	if _, err := fmt.Fprintf(client, "POST /v1/chat/completions HTTP/1.1\r\nHost: proxy.test\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body); err != nil {
		t.Fatal(err)
	}

	// The request is dispatched and holds the account's slot while its
	// response sits behind the silent peer.
	testsyn.WaitUntil(t, "the stalled request to occupy account a", func() bool {
		return proxy.runtimeState.Concurrency("a").Inflight == 1
	})

	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatalf("proxy handler still parked in the client write %v after its %v upstream timeout expired", 10*time.Second, budget)
	}
	if elapsed := time.Since(sent); elapsed < 2*budget-20*time.Millisecond {
		t.Fatalf("handler returned after %v, before the %v budget + tail grace expired", elapsed, budget)
	}
	if inflight := proxy.runtimeState.Concurrency("a").Inflight; inflight != 0 {
		t.Fatalf("account a still holds %d in-flight request(s); a stalled peer must not pin its occupancy", inflight)
	}

	// The stalled line is the only stream attribution; nothing blames the
	// upstream, and the account keeps serving.
	const stalled = "client stopped reading — a client write blocked past the upstream timeout; stream abandoned; not an upstream fault"
	if n := strings.Count(logs.text(), stalled); n != 1 {
		t.Fatalf("serve.log lines naming the stalled client = %d, want exactly 1:\n%s", n, logs.text())
	}
	if logged := logs.text(); strings.Contains(logged, "upstream error") || strings.Contains(logged, "scheduling timeout") || strings.Contains(logged, "client disconnected mid-stream") {
		t.Fatalf("a stalled client was misattributed:\n%s", logged)
	}
	probe := httptest.NewServer(http.HandlerFunc(proxy.Handler))
	t.Cleanup(probe.Close)
	status, got, _ := postUnstuck(t, probe.URL+"/v1/chat/completions", concurrencyChatBody)
	if status != http.StatusOK || !strings.Contains(got, "direct") {
		t.Fatalf("request after the stall = %d %s, want account a's 200", status, got)
	}
}
