package targetexec

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/protocol"
)

// Regression (observed live 2026-11-16, 132×/day): a client disconnect
// mid-stream cancels the caller-owned request context, which interrupts the
// parked upstream body Read with a context error. The passthrough terminal
// watcher cannot tell that apart from an upstream that abandoned its
// generation, so the executor logged the same "upstream stream ended before
// its terminal sequence" warn for both — a routine agent-side subagent abort
// and a genuine provider truncation were indistinguishable in serve.log, and
// the line even claimed the synthesized terminal was "delivered" to a client
// that had already walked away.

// parkedChunk is an SSE frame with NO terminal: no finish_reason, no [DONE].
const parkedChunk = `data: {"id":"c1","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n"

// parkedSSEDoer serves one 200 SSE response whose body delivers parkedChunk
// and then parks in Read until the request context ends — the exact shape the
// real transport shows when the caller disconnects mid-stream.
type parkedSSEDoer struct{ headersServed chan struct{} }

func (d *parkedSSEDoer) Do(r *http.Request) (*http.Response, error) {
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       &parkedBody{ctx: r.Context()},
	}
	close(d.headersServed)
	return response, nil
}

type parkedBody struct {
	ctx    context.Context
	served bool
}

func (b *parkedBody) Read(p []byte) (int, error) {
	if !b.served {
		b.served = true
		return copy(p, parkedChunk), nil
	}
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *parkedBody) Close() error { return nil }

// streamingAttempt builds a same-protocol streaming attempt (openai→openai,
// stream:true) against a fake upstream — the passthrough terminal-watcher
// path production traffic takes.
func streamingAttempt(t *testing.T, request *http.Request, writer http.ResponseWriter) Attempt {
	t.Helper()
	plan := NewPlan(PlanInput{
		Target:          configdomain.RouteTarget{Provider: "upstream", Model: "model"},
		ProviderConfig:  configdomain.Provider{Provider: "test", OpenAIBaseURL: "https://upstream.test"},
		Provider:        &executorTestProvider{},
		ClientProtocol:  protocol.OpenAI,
		BackendProtocol: protocol.OpenAI,
		ClientPath:      "/v1/chat/completions",
	})
	return NewAttempt(Runtime{}, plan,
		Exchange{Request: request, Writer: writer, Body: []byte(`{"model":"model","stream":true,"messages":[]}`)},
		Scope{}, Policy{})
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	original := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(original) })
	return &logs
}

// TestExecutorStreamClientCancelIsNotLoggedAsUpstreamTruncation: canceling the
// caller context mid-stream must NOT produce the upstream-truncation warn.
func TestExecutorStreamClientCancelIsNotLoggedAsUpstreamTruncation(t *testing.T) {
	logs := captureLogs(t)
	ctx, cancel := context.WithCancel(context.Background())
	request := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", nil).WithContext(ctx)
	doer := &parkedSSEDoer{headersServed: make(chan struct{})}
	done := make(chan Result, 1)
	go func() {
		done <- (Executor{Client: doer, State: &executorState{}}).Execute(streamingAttempt(t, request, httptest.NewRecorder()))
	}()
	<-doer.headersServed
	cancel()
	select {
	case result := <-done:
		if !result.Committed {
			t.Fatalf("result = %+v; headers had committed, the stream copy must finish committed", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Execute did not return after the client canceled")
	}

	if strings.Contains(logs.String(), "upstream stream ended before its terminal sequence") {
		t.Fatalf("client disconnect was misattributed as an upstream truncation:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "client disconnected mid-stream") {
		t.Fatalf("missing the client-disconnect attribution line:\n%s", logs.String())
	}
}

// TestExecutorStreamGenuineTruncationStillWarns: the control — an upstream
// that ends its stream without a terminal while the caller is still there
// keeps the original warn, byte-identical.
func TestExecutorStreamGenuineTruncationStillWarns(t *testing.T) {
	logs := captureLogs(t)
	request := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", nil)
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(bytes.NewBufferString(parkedChunk)),
	}
	result := (Executor{Client: &sequenceDoer{responses: []*http.Response{response}}, State: &executorState{}}).Execute(streamingAttempt(t, request, httptest.NewRecorder()))
	if !result.Committed {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(logs.String(), "upstream stream ended before its terminal sequence — synthesized a protocol-native error terminal for the client (delivered, never cached)") {
		t.Fatalf("genuine truncation warn missing or altered:\n%s", logs.String())
	}
}
