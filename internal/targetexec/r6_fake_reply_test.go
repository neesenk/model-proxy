package targetexec

import (
	"bytes"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const r6LoginPage = "<!DOCTYPE html><html><head><title>Sign in to relay</title></head><body>login</body></html>"

// r6ReadSpyBody is the shape of a slow-TTFT upstream that has sent its
// response headers and nothing else: Read records that it happened, then
// parks until Close releases it.
type r6ReadSpyBody struct {
	readCalled atomic.Bool
	closes     atomic.Int32
	released   chan struct{}
	closeOnce  sync.Once
}

func newR6ReadSpyBody() *r6ReadSpyBody {
	return &r6ReadSpyBody{released: make(chan struct{})}
}

func (body *r6ReadSpyBody) Read(p []byte) (int, error) {
	body.readCalled.Store(true)
	<-body.released
	return 0, io.EOF
}

func (body *r6ReadSpyBody) Close() error {
	body.closes.Add(1)
	body.closeOnce.Do(func() { close(body.released) })
	return nil
}

// r6HTMLResponse builds the 2xx a relay/WAF answers a fake success with: the
// page served through body under the given Content-Type.
func r6HTMLResponse(contentType string, body *closeCountingBody) *http.Response {
	response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}
	response.Header.Set("Content-Type", contentType)
	return response
}

// TestR6ExecutorFakeAPIReplyClosesUpstreamBody: both 2xx-HTML verdict arms
// return before the commit path — the only place that used to close
// response.Body — so a gateway answering "200 + login page" leaked one
// upstream body per request. net/http gives a body no finalizer: an unclosed
// one pins its persistent connection and read-loop goroutine for good, which
// is exactly the credential-storm shape that triggers this verdict. The
// verdict and its accounting are pinned by TestExecutorFakeAPIReplyFailsOver;
// this pins the release: the upstream body is closed exactly once.
func TestR6ExecutorFakeAPIReplyClosesUpstreamBody(t *testing.T) {
	cases := []struct{ name, contentType string }{
		{"content type html", "text/html; charset=utf-8"},
		{"html body with json content type", "application/json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provider := &executorTestProvider{}
			attempt, writer := testAttempt(provider, `{}`, Policy{LastTarget: true})
			body := &closeCountingBody{reader: bytes.NewBufferString(r6LoginPage)}
			state, effects := &executorState{}, &executorEffects{}
			result := (Executor{
				Client:  &sequenceDoer{responses: []*http.Response{r6HTMLResponse(tc.contentType, body)}},
				State:   state,
				Effects: effects,
			}).Execute(attempt)
			if result.Committed || result.Outcome != OutcomeFailedHard || result.AttemptStatus != http.StatusBadGateway ||
				state.failures != 1 || effects.failovers != 1 || effects.commits != 0 || writer.Body.Len() != 0 {
				t.Fatalf("result=%+v state=%+v effects=%+v written=%q", result, state, effects, writer.Body.String())
			}
			if got := body.closes.Load(); got != 1 {
				t.Fatalf("upstream body Close calls = %d, want exactly 1 (an unclosed body pins its connection and read loop)", got)
			}
		})
	}
}

// TestR6ExecutorFakeAPIReplyStreamDoesNotReadBody: the Content-Type arm
// settles a STREAMING request from the headers alone. Its title peek is an
// io.ReadAll that only returns at EOF or at the 2 KiB cap — on a slow-TTFT
// upstream it parks with the whole scheduling timeout ahead of it, before any
// heartbeat can be written, which is exactly what the SSE keepalive contract
// for slow streams forbids (routing-and-failure.md, 伪成功). The spy turns
// "did not read" into an observed fact: Read flips readCalled and then parks
// until Close, so a verdict that touched the body would hang here rather than
// return. Non-stream callers keep the title peek — their body read is about
// to happen anyway (TestExecutorFakeAPIReplyFailsOver pins the title).
func TestR6ExecutorFakeAPIReplyStreamDoesNotReadBody(t *testing.T) {
	provider := &executorTestProvider{}
	attempt, writer := testAttempt(provider, `{"model":"model","stream":true}`, Policy{LastTarget: true})
	body := newR6ReadSpyBody()
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:       body,
	}
	state, effects := &executorState{}, &executorEffects{}
	executor := Executor{
		Client:  &sequenceDoer{responses: []*http.Response{response}},
		State:   state,
		Effects: effects,
	}
	done := make(chan Result, 1)
	go func() { done <- executor.Execute(attempt) }()
	var result Result
	select {
	case result = <-done:
	case <-time.After(10 * time.Second):
		// Hang guard only: release the parked Read so the executor goroutine
		// can exit behind the failing test.
		body.Close()
		t.Fatalf("Execute did not return: the Content-Type arm read the body of a streaming request (readCalled=%v)", body.readCalled.Load())
	}
	if result.Committed || result.Outcome != OutcomeFailedHard || result.AttemptStatus != http.StatusBadGateway ||
		state.failures != 1 || state.modelFailures != 0 || effects.failovers != 1 || effects.commits != 0 || writer.Body.Len() != 0 {
		t.Fatalf("result=%+v state=%+v effects=%+v written=%q", result, state, effects, writer.Body.String())
	}
	if body.readCalled.Load() {
		t.Fatal("upstream body was read for a streaming request: the verdict must settle from the Content-Type header alone")
	}
	if got := body.closes.Load(); got != 1 {
		t.Errorf("upstream body Close calls = %d, want exactly 1", got)
	}
	// No title was peeked, so the leg carries the header-only evidence.
	if got, want := string(result.AttemptBody), "upstream answered 200 with an HTML page: html document"; got != want {
		t.Errorf("AttemptBody = %q, want %q", got, want)
	}
}
