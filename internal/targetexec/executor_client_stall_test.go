package targetexec

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/testsyn"
)

// Regression (serve audit 2026-11-26): two requests were ended by the 10:36
// restart's forced close with latency 31,829,740 ms and 29,680,037 ms — 8h50m
// and 8h14m after their first byte — and recorded as status 200. Their
// attribution lines ("client disconnected mid-stream before its terminal —
// nothing synthesized") belong to the data-write-failure branch, and no
// `scheduling timeout` line exists in eleven days of serve.log: the copy loop
// was parked in the client Write, not in the upstream Read. A peer that stops
// reading but keeps its connection (a stalled worker, a leaked response)
// fills the socket buffers; that Write never returns and never fails, and
// the upstream-timeout context cannot reach it. The handler, its goroutines,
// the captured body and the account's in-flight slot leaked until the process
// was restarted.

// stallingBody is an upstream serving non-terminal SSE deltas, exactly chunk
// bytes per Read while they last (0 = fill the buffer). Once prefix bytes are
// served (-1 = without end) a Read parks until the body is closed or the
// upstream request's context ends — like a real transport, whose body Read
// fails when the request's upstream-timeout context expires. With chunk ==
// prefix the whole prefix arrives in ONE read, so what follows the write of
// it is the upstream ending, never another write.
type stallingBody struct {
	chunk  int
	prefix int64
	// pattern is the content repeated; it defaults to parkedChunk. A JSON
	// body must not look like SSE — the executor sniffs the first bytes for
	// event framing and streams whatever matches.
	pattern string
	served  atomic.Int64
	// ctx is the upstream request's context, set by the doer before any
	// reader goroutine exists.
	ctx    context.Context
	closed chan struct{}
	once   sync.Once
}

func (body *stallingBody) Read(p []byte) (int, error) {
	remaining := body.prefix - body.served.Load()
	if body.prefix < 0 || remaining > 0 {
		pattern := []byte(body.pattern)
		if len(pattern) == 0 {
			pattern = []byte(parkedChunk)
		}
		n := len(p)
		if body.chunk > 0 && body.chunk < n {
			n = body.chunk
		}
		if remaining > 0 && int64(n) > remaining {
			n = int(remaining)
		}
		for off := 0; off < n; off += len(pattern) {
			copy(p[off:n], pattern)
		}
		body.served.Add(int64(n))
		return n, nil
	}
	select {
	case <-body.closed:
		return 0, io.EOF
	case <-body.ctx.Done():
		return 0, body.ctx.Err()
	}
}

func (body *stallingBody) Close() error {
	body.once.Do(func() { close(body.closed) })
	return nil
}

type stallingDoer struct {
	body        *stallingBody
	contentType string
}

func (doer *stallingDoer) Do(request *http.Request) (*http.Response, error) {
	doer.body.ctx = request.Context()
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{doer.contentType}},
		Body:       doer.body,
	}, nil
}

// inflightCountingState counts the occupancy the executor takes and releases
// for the account (the plain test double answers both with no-ops).
type inflightCountingState struct {
	executorState
	taken, released int
	outcome         InflightOutcome
}

func (state *inflightCountingState) TakeInflight(string) (InflightSlot, bool) {
	state.taken++
	return InflightSlot{}, true
}

func (state *inflightCountingState) ReleaseInflight(_ InflightSlot, outcome InflightOutcome) (int, bool) {
	state.released++
	state.outcome = outcome
	return 0, false
}

const stalledClientLine = "[proto=openai provider=upstream] client stopped reading — a client write blocked past the upstream timeout; stream abandoned; not an upstream fault"

// TestExecutorClientStopsReadingIsBoundedByUpstreamTimeout: the peer keeps
// its connection and reads nothing while the upstream streams. The handler
// must return once the request's upstream timeout (plus the tail grace) has
// expired — it used to stay parked in the client Write forever — release the
// account's slot, close the upstream, and leave exactly the stalled-client
// attribution line behind. The peer connects over an unbuffered pipe, so
// every scenario's write is blocked from its first byte; the cases cover
// each place the block can sit: the connection write of a large frame, the
// flush behind a small one (streaming and non-streaming), and the keepalive
// heartbeat behind a silent upstream. In the flush cases the upstream ends
// at its own deadline while that flush is still blocked — the error must be
// read off the flush itself, or the stream ends up blamed on the upstream.
func TestExecutorClientStopsReadingIsBoundedByUpstreamTimeout(t *testing.T) {
	const budget = 250 * time.Millisecond
	cases := []struct {
		name        string
		keepalive   string
		chunk       int
		prefix      int64
		stream      bool
		contentType string
		pattern     string
	}{
		{name: "plain copy, write-blocked", keepalive: "0", prefix: -1, stream: true, contentType: "text/event-stream"},
		{name: "keepalive copy, write-blocked", keepalive: "20ms", prefix: -1, stream: true, contentType: "text/event-stream"},
		{name: "keepalive copy, flush-blocked", keepalive: "20ms", chunk: 1500, prefix: 1500, stream: true, contentType: "text/event-stream"},
		{name: "plain copy, flush-blocked", keepalive: "0", chunk: 1500, prefix: 1500, stream: true, contentType: "text/event-stream"},
		// A non-stream body is peeked 64 bytes ahead of the copy (the fake
		// reply check), so a 64-byte body reaches the copy whole in its first
		// read: nothing follows the blocked flush but the upstream ending.
		{name: "non-stream body, flush-blocked", keepalive: "0", prefix: 64, contentType: "application/json",
			pattern: `{"id":"resp","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`},
		{name: "heartbeat flush-blocked behind a silent upstream", keepalive: "20ms", stream: true, contentType: "text/event-stream"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := captureLogs(t)
			goroutines := testsyn.Goroutines()

			upstream := &stallingBody{chunk: tc.chunk, prefix: tc.prefix, pattern: tc.pattern, closed: make(chan struct{})}
			state := &inflightCountingState{}
			type outcome struct {
				result  Result
				elapsed time.Duration
			}
			done := make(chan outcome, 1)
			runtime := Runtime{Scheduling: configdomain.Scheduling{UpstreamTimeout: budget.String(), StreamKeepalive: tc.keepalive}}
			requestBody := `{"model":"model","stream":true,"messages":[]}`
			if !tc.stream {
				requestBody = `{"model":"model","stream":false,"messages":[]}`
			}
			listener := testsyn.NewSilentPeerListener()
			server := &httptest.Server{
				Listener: listener,
				Config: &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					started := time.Now()
					result := (Executor{Client: &stallingDoer{body: upstream, contentType: tc.contentType}, State: state}).Execute(
						attemptWithRuntimeBody(t, runtime, r, w, requestBody))
					done <- outcome{result: result, elapsed: time.Since(started)}
				})},
			}
			server.Start()
			t.Cleanup(func() { testsyn.LeakCheck(t, goroutines, "stalled-client stream") })
			t.Cleanup(server.Close)

			client, err := listener.Connect()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = client.Close() })
			// A complete request whose response is then never read: the client
			// keeps the connection and simply stops consuming the stream, so
			// the server's first response write is already blocked.
			if _, err := io.WriteString(client, "POST /v1/chat/completions HTTP/1.1\r\nHost: proxy.test\r\nContent-Length: 0\r\n\r\n"); err != nil {
				t.Fatal(err)
			}

			select {
			case out := <-done:
				if !out.result.Committed {
					t.Fatalf("result = %+v; headers had committed, the abandoned stream must finish committed", out.result)
				}
				if out.elapsed < 2*budget-20*time.Millisecond {
					t.Fatalf("handler returned after %v, before the %v budget + tail grace expired", out.elapsed, budget)
				}
			case <-time.After(10 * time.Second):
				t.Fatalf("handler still parked in the client write %v after its %v upstream timeout expired", 10*time.Second, budget)
			}
			// The peer never went away: its end of the pipe is still ours to
			// close (the cleanup does).
			if state.taken != 1 || state.released != 1 {
				t.Fatalf("in-flight slots taken = %d, released = %d; want 1/1 — a stalled peer must not pin the account's occupancy", state.taken, state.released)
			}
			// The ledger's verdict is the upstream's: it admitted the request
			// (2xx) before the client stopped reading, so the slot's release
			// reports a success — the concurrency ledger measures admission,
			// not delivery.
			if state.outcome != InflightSucceeded {
				t.Fatalf("in-flight outcome = %d, want succeeded (the upstream admitted the stream)", state.outcome)
			}
			if state.failures != 0 || state.modelFailures != 0 {
				t.Fatalf("state failures = %d, model failures = %d; a stalled client is not an upstream fault", state.failures, state.modelFailures)
			}
			select {
			case <-upstream.closed:
			default:
				t.Fatal("the upstream body was not closed after the stream was abandoned")
			}
			if tc.prefix != 0 && upstream.served.Load() == 0 {
				t.Fatal("the upstream never produced bytes; the client write was never exercised")
			}
			assertLoggedOnce(t, logs.String(), stalledClientLine)
			for _, misattribution := range []string{"upstream stream ended", "scheduling timeout", "client disconnected mid-stream"} {
				if strings.Contains(logs.String(), misattribution) {
					t.Fatalf("a stalled client was attributed as %q:\n%s", misattribution, logs.String())
				}
			}
		})
	}
}

// errWriteDeadline is what the connection surfaces when a client write
// blocked past its deadline (net.OpError around os.ErrDeadlineExceeded).
var errWriteDeadline = &net.OpError{Op: "write", Net: "tcp", Err: os.ErrDeadlineExceeded}

// stalledPeerWriter models a non-reading peer behind a bounded writer: the
// first small Write still lands in the response bufio buffer, the Flush onto
// the full socket fails with the deadline error (http.Flusher.Flush has no
// error return), and only the NEXT Write surfaces the sticky error.
type stalledPeerWriter struct{ writes int }

func (writer *stalledPeerWriter) Header() http.Header { return make(http.Header) }
func (*stalledPeerWriter) WriteHeader(int)            {}
func (*stalledPeerWriter) Flush()                     {}
func (writer *stalledPeerWriter) Write(p []byte) (int, error) {
	writer.writes++
	if writer.writes >= 2 {
		return 0, errWriteDeadline
	}
	return len(p), nil
}

// TestExecutorStreamClientWriteStallIsLoggedAsStalled: when the write that
// fails carries the deadline error, serve.log must name the stalled client —
// it used to read as a plain disconnect, which sent the 2026-11-26 audit to
// the connection owner instead of the stopped reader.
func TestExecutorStreamClientWriteStallIsLoggedAsStalled(t *testing.T) {
	for _, keepalive := range []string{"0", "20ms"} {
		t.Run("keepalive="+keepalive, func(t *testing.T) {
			logs := captureLogs(t)
			request := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", nil)
			runtime := Runtime{Scheduling: configdomain.Scheduling{StreamKeepalive: keepalive}}
			body := &flowingBody{chunks: 3, closed: make(chan struct{})}
			result := (Executor{Client: &flowingSSEDoer{body: body}, State: &executorState{}}).Execute(
				streamingAttemptWithRuntime(t, runtime, request, &stalledPeerWriter{}))
			if !result.Committed {
				t.Fatalf("result = %+v; headers had committed, the abandoned stream must finish committed", result)
			}
			assertLoggedOnce(t, logs.String(), stalledClientLine)
			if strings.Contains(logs.String(), "client disconnected mid-stream") {
				t.Fatalf("a stalled client was logged as a disconnect:\n%s", logs.String())
			}
		})
	}
}

// A write failure that is not the deadline error keeps the disconnect
// attribution: the bound must not relabel an ordinary closed peer.
func TestExecutorStreamNonDeadlineWriteErrorStaysClientDisconnect(t *testing.T) {
	logs := captureLogs(t)
	request := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", nil)
	body := &flowingBody{chunks: 3, closed: make(chan struct{})}
	writer := &errorPeerWriter{err: errors.New("write tcp: connection reset by peer")}
	result := (Executor{Client: &flowingSSEDoer{body: body}, State: &executorState{}}).Execute(
		streamingAttemptWithRuntime(t, Runtime{}, request, writer))
	if !result.Committed {
		t.Fatalf("result = %+v", result)
	}
	assertLoggedOnce(t, logs.String(),
		"[proto=openai provider=upstream] client disconnected mid-stream before its terminal — nothing synthesized; not an upstream fault")
	if strings.Contains(logs.String(), "client stopped reading") {
		t.Fatalf("a reset connection was logged as a stalled client:\n%s", logs.String())
	}
}

type errorPeerWriter struct {
	closedPeerWriter
	err error
}

func (writer *errorPeerWriter) Write(p []byte) (int, error) {
	writer.writes++
	if writer.writes >= 2 {
		return 0, writer.err
	}
	return len(p), nil
}

// clientWriteDeadline is the request's upstream deadline plus the tail grace,
// capped at the budget itself: a short upstream_timeout keeps the bound
// proportionally tight instead of granting a fixed 5s.
func TestClientWriteDeadline(t *testing.T) {
	upstream := time.Date(2026, 11, 26, 2, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		budget time.Duration
		want   time.Duration
	}{
		{budget: 30 * time.Minute, want: 5 * time.Second},
		{budget: 5 * time.Second, want: 5 * time.Second},
		{budget: 250 * time.Millisecond, want: 250 * time.Millisecond},
		{budget: -time.Second, want: 0},
	} {
		if got := clientWriteDeadline(upstream, tc.budget).Sub(upstream); got != tc.want {
			t.Fatalf("budget %v: grace = %v, want %v", tc.budget, got, tc.want)
		}
	}
}

// completeSSEDoer answers every request with the same finished stream.
type completeSSEDoer struct{}

func (*completeSSEDoer) Do(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(strings.NewReader(parkedChunk + "data: [DONE]\n\n")),
	}, nil
}

// TestExecutorClientWriteDeadlineIsClearedAfterResponse is the healthy-peer
// control for the client-write bound, on a real TCP connection: a complete
// response is delivered byte-identical with the deadline armed, and the
// kept-alive connection still serves the next request after that deadline
// has passed — a deadline left on the connection would fail every later
// write once it expired. Both copy paths.
func TestExecutorClientWriteDeadlineIsClearedAfterResponse(t *testing.T) {
	const budget = 200 * time.Millisecond
	for _, keepalive := range []string{"0", "20ms"} {
		t.Run("keepalive="+keepalive, func(t *testing.T) {
			runtime := Runtime{Scheduling: configdomain.Scheduling{UpstreamTimeout: budget.String(), StreamKeepalive: keepalive}}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				(Executor{Client: &completeSSEDoer{}, State: &executorState{}}).Execute(
					streamingAttemptWithRuntime(t, runtime, r, w))
			}))
			t.Cleanup(server.Close)
			transport := &http.Transport{}
			t.Cleanup(transport.CloseIdleConnections)
			client := &http.Client{Transport: transport}
			const stream = parkedChunk + "data: [DONE]\n\n"
			post := func(what string) bool {
				t.Helper()
				request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", strings.NewReader(`{}`))
				if err != nil {
					t.Fatal(err)
				}
				var reused bool
				request = request.WithContext(httptrace.WithClientTrace(request.Context(), &httptrace.ClientTrace{
					GotConn: func(info httptrace.GotConnInfo) { reused = info.Reused },
				}))
				response, err := client.Do(request)
				if err != nil {
					t.Fatalf("%s: %v", what, err)
				}
				got, err := io.ReadAll(response.Body)
				response.Body.Close()
				if err != nil {
					t.Fatalf("%s: reading the response: %v", what, err)
				}
				if response.StatusCode != http.StatusOK || string(got) != stream {
					t.Fatalf("%s = %d %q, want the complete 200 stream", what, response.StatusCode, got)
				}
				return reused
			}
			if post("first request") {
				t.Fatal("the first request cannot be reusing a connection")
			}
			// The first response's client-write deadline is its upstream
			// timeout plus the tail grace (the budget itself here, being
			// shorter than 5s). The second request is sent only once that
			// instant is in the past, on the same kept-alive connection.
			writeDeadline := time.Now().Add(2*budget + 50*time.Millisecond)
			if wait := time.Until(writeDeadline); wait > 0 {
				<-time.After(wait)
			}
			if !post("second request on the kept-alive connection") {
				t.Fatal("the second request did not reuse the connection; a stale deadline would never have been reached")
			}
		})
	}
}
