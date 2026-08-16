package app

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"model-proxy/internal/observe/requestlog"
)

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
