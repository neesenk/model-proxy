package targetexec

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/protocol"
)

// Regression (serve audit 2026-11-22): 51 passthrough streams were cut before
// their terminal and left NO line in serve.log — 35 of them force-closed by the
// 21:14 restart's drain deadline, 16 by client disconnects. The terminal
// watcher only reports an end it OBSERVED (upstream EOF/read error); when the
// client-side write fails first, the copy loop returns streamClientGone before
// the watcher ever sees the upstream end, so nothing is synthesized and the
// attribution switch below it is skipped entirely. The same day 40
// streams ended with an upstream error chunk (200 committed, then
// `{"error":{"message":"upstream stream terminated unexpectedly"}}`) — also
// invisible in serve.log, because an upstream error terminal deliberately
// suppresses synthesis AND the log that hangs off it.

// upstreamErrorChunk is the observed live mid-stream abort frame (11-22, 40×): an
// error chunk with no finish_reason and no [DONE].
const upstreamErrorChunk = `data: {"error":{"message":"upstream stream terminated unexpectedly","type":"api_error","code":null,"param":null}}` + "\n\n"

// closedPeerWriter models a net/http response writer whose client connection
// is gone (client disconnect or the server's own shutdown force-close): the
// first small Write still lands in the response bufio buffer and returns nil,
// the Flush onto the dead conn fails silently (http.Flusher.Flush has no error
// return), and only the NEXT Write surfaces the sticky error.
var errClosedPeer = errors.New("write tcp: use of closed network connection")

type closedPeerWriter struct {
	writes int
}

func (writer *closedPeerWriter) Header() http.Header { return make(http.Header) }
func (writer *closedPeerWriter) WriteHeader(int)     {}
func (*closedPeerWriter) Flush()                     {}
func (writer *closedPeerWriter) Write(p []byte) (int, error) {
	writer.writes++
	if writer.writes >= 2 {
		return 0, errClosedPeer
	}
	return len(p), nil
}

// flowingBody keeps yielding non-terminal deltas (the upstream is still
// generating) and only parks once every part was served — the reader-behind-
// writer shape of 37 concurrent streams at a drain deadline, where buffered
// upstream data wins the race against context-cancel propagation.
type flowingBody struct {
	// parts, when set, is served one per Read (distinct arrival shapes);
	// otherwise `chunks` copies of parkedChunk are.
	parts  []string
	chunks int
	served int
	// readsAfterParts counts Reads that reached the parked tail.
	readsAfterParts int
	closed          chan struct{}
	closeOnce       sync.Once
}

func (body *flowingBody) Read(p []byte) (int, error) {
	part := parkedChunk
	if body.parts != nil {
		if body.served >= len(body.parts) {
			body.readsAfterParts++
			<-body.closed
			return 0, io.EOF
		}
		part = body.parts[body.served]
	} else if body.served >= body.chunks {
		body.readsAfterParts++
		<-body.closed
		return 0, io.EOF
	}
	body.served++
	return copy(p, part), nil
}

func (body *flowingBody) Close() error {
	body.closeOnce.Do(func() { close(body.closed) })
	return nil
}

type flowingSSEDoer struct{ body *flowingBody }

func (doer *flowingSSEDoer) Do(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       doer.body,
	}, nil
}

// streamingAttemptWithRuntime is streamingAttempt for attempts that need a
// scheduling config (the keepalive copy path keys off StreamKeepalive).
func streamingAttemptWithRuntime(t *testing.T, runtime Runtime, request *http.Request, writer http.ResponseWriter) Attempt {
	t.Helper()
	plan := NewPlan(PlanInput{
		Target:          configdomain.RouteTarget{Provider: "upstream", Model: "model"},
		ProviderConfig:  configdomain.Provider{Provider: "test", OpenAIBaseURL: "https://upstream.test"},
		Provider:        &executorTestProvider{},
		ClientProtocol:  protocol.OpenAI,
		BackendProtocol: protocol.OpenAI,
		ClientPath:      "/v1/chat/completions",
	})
	return NewAttempt(runtime, plan,
		Exchange{Request: request, Writer: writer, Body: []byte(`{"model":"model","stream":true,"messages":[]}`)},
		Scope{}, Policy{})
}

// TestExecutorStreamPeerClosedWhileDataFlowsIsLoggedAsClientDisconnect: the
// client connection is lost while the upstream still streams and no terminal
// has arrived — serve.log must carry the client-disconnect attribution line
// (it used to carry nothing at all).
func TestExecutorStreamPeerClosedWhileDataFlowsIsLoggedAsClientDisconnect(t *testing.T) {
	logs := captureLogs(t)
	request := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", nil)
	body := &flowingBody{chunks: 3, closed: make(chan struct{})}
	result := (Executor{Client: &flowingSSEDoer{body: body}, State: &executorState{}}).Execute(
		streamingAttemptWithRuntime(t, Runtime{}, request, &closedPeerWriter{}))
	if !result.Committed {
		t.Fatalf("result = %+v; headers had committed, the stream copy must finish committed", result)
	}
	assertLoggedOnce(t, logs.String(),
		"[proto=openai provider=upstream] client disconnected mid-stream before its terminal — nothing synthesized; not an upstream fault")
	if strings.Contains(logs.String(), "upstream stream ended") {
		t.Fatalf("client-side write failure was misattributed as an upstream truncation:\n%s", logs.String())
	}
}

// Same shape on the production copy path: stream_keepalive > 0 selects
// flushCopyHeartbeat, whose reader goroutine delivers the buffered delta that
// trips the sticky write error before any context cancel reaches the Read.
func TestExecutorStreamKeepalivePeerClosedWhileDataFlowsIsLoggedAsClientDisconnect(t *testing.T) {
	logs := captureLogs(t)
	request := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", nil)
	body := &flowingBody{chunks: 3, closed: make(chan struct{})}
	runtime := Runtime{Scheduling: configdomain.Scheduling{StreamKeepalive: "1h"}}
	result := (Executor{Client: &flowingSSEDoer{body: body}, State: &executorState{}}).Execute(
		streamingAttemptWithRuntime(t, runtime, request, &closedPeerWriter{}))
	if !result.Committed {
		t.Fatalf("result = %+v; headers had committed, the stream copy must finish committed", result)
	}
	assertLoggedOnce(t, logs.String(),
		"[proto=openai provider=upstream] client disconnected mid-stream before its terminal — nothing synthesized; not an upstream fault")
}

// Control: once the upstream delivered its terminal, a later write failure cut
// nothing upstream — it is not a mid-stream disconnect and must stay unlogged.
// The terminal arrives in its OWN read so the failing write is the one that
// carried it: the watcher scans on read, before the write can fail.
func TestExecutorStreamPeerClosedAfterTerminalIsNotLoggedAsDisconnect(t *testing.T) {
	logs := captureLogs(t)
	request := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", nil)
	doer := &flowingSSEDoer{body: &flowingBody{parts: []string{parkedChunk, "data: [DONE]\n\n"}, closed: make(chan struct{})}}
	result := (Executor{Client: doer, State: &executorState{}}).Execute(
		streamingAttemptWithRuntime(t, Runtime{}, request, &closedPeerWriter{}))
	if !result.Committed {
		t.Fatalf("result = %+v", result)
	}
	if doer.body.served != 2 || doer.body.readsAfterParts != 0 {
		t.Fatalf("served = %d, parked reads = %d; want the failing write to be the one carrying [DONE]",
			doer.body.served, doer.body.readsAfterParts)
	}
	if strings.Contains(logs.String(), "disconnected") || strings.Contains(logs.String(), "upstream stream ended") {
		t.Fatalf("a complete upstream stream must not log a stream abort:\n%s", logs.String())
	}
}

// TestExecutorStreamUpstreamErrorTerminalIsLogged: the upstream itself ends a
// committed 200 stream with an error terminal (passed through untouched, never
// re-synthesized). serve.log must name the upstream failure — the request log
// records it as a plain status 200, so this line is the only failure evidence.
func TestExecutorStreamUpstreamErrorTerminalIsLogged(t *testing.T) {
	logs := captureLogs(t)
	request := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", nil)
	doer := &sequenceDoer{responses: []*http.Response{{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(bytes.NewBufferString(parkedChunk + upstreamErrorChunk)),
	}}}
	writer := httptest.NewRecorder()
	result := (Executor{Client: doer, State: &executorState{}}).Execute(
		streamingAttemptWithRuntime(t, Runtime{}, request, writer))
	if !result.Committed {
		t.Fatalf("result = %+v", result)
	}
	if !strings.Contains(writer.Body.String(), upstreamErrorChunk) {
		t.Fatalf("the upstream error terminal must reach the client byte-identical:\n%s", writer.Body.String())
	}
	if strings.Contains(writer.Body.String(), "ended without a terminal finish_reason") {
		t.Fatal("an upstream error terminal must not be re-synthesized")
	}
	logged := logs.String()
	assertLoggedOnce(t, logged,
		"[proto=openai provider=upstream] upstream error terminal ended the stream — forwarded to the client as-is, nothing synthesized: "+
			`{"error":{"message":"upstream stream terminated unexpectedly","type":"api_error","code":null,"param":null}}`)
	if strings.Contains(logged, "client disconnected") {
		t.Fatalf("an upstream failure was attributed to the client:\n%s", logged)
	}
}

// Control: a complete passthrough stream leaves no stream-end line at all.
func TestExecutorStreamCompletePassthroughLogsNoStreamEnd(t *testing.T) {
	logs := captureLogs(t)
	request := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", nil)
	complete := parkedChunk + `data: {"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" + "data: [DONE]\n\n"
	doer := &sequenceDoer{responses: []*http.Response{{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(bytes.NewBufferString(complete)),
	}}}
	result := (Executor{Client: doer, State: &executorState{}}).Execute(
		streamingAttemptWithRuntime(t, Runtime{}, request, httptest.NewRecorder()))
	if !result.Committed {
		t.Fatalf("result = %+v", result)
	}
	if logged := logs.String(); strings.Contains(logged, "stream ended") || strings.Contains(logged, "disconnected") || strings.Contains(logged, "error terminal") {
		t.Fatalf("a complete upstream stream must not log a stream abort:\n%s", logged)
	}
}

func assertLoggedOnce(t *testing.T, logged, want string) {
	t.Helper()
	if n := strings.Count(logged, want); n != 1 {
		t.Fatalf("serve.log lines containing %q = %d, want exactly 1:\n%s", want, n, logged)
	}
}
