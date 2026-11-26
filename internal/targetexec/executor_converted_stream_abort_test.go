package targetexec

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/protocol"
)

// Regression (serve audit 2026-11-24): a CONVERTED stream that ended
// mid-generation left no per-event line in serve.log. Stream-abort
// attribution hangs off the passthrough terminal watcher, which is wired
// only for same-protocol streams; the six streaming converters fail closed
// on their own — an error terminal in the client protocol — and their only
// log is convertWarn, deduped per process per message and carrying neither
// provider nor cause. 13 zcode streams (openai clients reading an anthropic
// upstream) ended in "upstream stream terminated unexpectedly" that day.
// The whole day's evidence was one deduped
// "[convert] WARN: anthropic SSE scanner error: context canceled", so a
// client cancelling and zcode dropping a connection were indistinguishable.

// anthropicParkedStream is an anthropic message stream that has begun
// generating and carries no terminal: no stop_reason, no message_stop.
const anthropicParkedStream = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"model","content":[],"stop_reason":null,"usage":{"input_tokens":3,"output_tokens":1}}}` + "\n\n" +
	"event: content_block_start\n" +
	`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
	"event: content_block_delta\n" +
	`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}` + "\n\n"

// convertedErrorMessage is the message of the anthropic→openai converter's
// fail-closed terminal when the upstream read fails mid-stream — zcode's live
// final frame on 2026-11-24 (13×): an error chunk with no finish_reason and
// no [DONE]. The converter marshals the error object from a map, so its key
// order is not a contract and is asserted field by field.
const convertedErrorMessage = "upstream stream terminated unexpectedly"

// requireConvertedErrorTerminal asserts that body ends with the converter's
// fail-closed terminal and nothing after it: the last data frame is the
// error chunk, and the stream carries neither a finish chunk nor [DONE].
func requireConvertedErrorTerminal(t *testing.T, body string) {
	t.Helper()
	var last string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: ") {
			last = line[len("data: "):]
		}
	}
	var chunk struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    any    `json:"code"`
			Param   any    `json:"param"`
		} `json:"error"`
		Choices any `json:"choices"`
	}
	if err := json.Unmarshal([]byte(last), &chunk); err != nil {
		t.Fatalf("final data frame is not JSON: %v (%q)", err, last)
	}
	if chunk.Error.Message != convertedErrorMessage || chunk.Error.Type != "api_error" ||
		chunk.Error.Code != nil || chunk.Error.Param != nil || chunk.Choices != nil {
		t.Fatalf("final frame = %s; want only the converter's error terminal %q/api_error", last, convertedErrorMessage)
	}
	if strings.Contains(body, "data: [DONE]") || strings.Contains(body, `"finish_reason":"`) {
		t.Fatalf("a failed stream must not carry a clean terminal:\n…%s", body[max(0, len(body)-200):])
	}
}

// sseDoer serves one 200 SSE response with a fixed body.
type sseDoer struct{ body io.ReadCloser }

func (doer *sseDoer) Do(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       doer.body,
	}, nil
}

// cutBody serves anthropicParkedStream once, then fails every Read with
// io.ErrUnexpectedEOF — an upstream connection dropping mid-generation.
type cutBody struct{ served bool }

func (body *cutBody) Read(p []byte) (int, error) {
	if !body.served {
		body.served = true
		return copy(p, anthropicParkedStream), nil
	}
	return 0, io.ErrUnexpectedEOF
}

func (*cutBody) Close() error { return nil }

// parkedAnthropicDoer serves one 200 SSE response whose body delivers
// anthropicParkedStream and then parks in Read until the UPSTREAM REQUEST
// context ends, returning its error — the exact transport shape both when
// the caller disconnects mid-stream (the cancel propagates into the derived
// context) and when the scheduling timeout fires on it alone.
type parkedAnthropicDoer struct{}

func (*parkedAnthropicDoer) Do(r *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       &parkedAnthropicBody{ctx: r.Context()},
	}, nil
}

type parkedAnthropicBody struct {
	ctx    context.Context
	served bool
}

func (body *parkedAnthropicBody) Read(p []byte) (int, error) {
	if !body.served {
		body.served = true
		return copy(p, anthropicParkedStream), nil
	}
	<-body.ctx.Done()
	return 0, body.ctx.Err()
}

func (*parkedAnthropicBody) Close() error { return nil }

// convertedStreamingAttempt builds a converted streaming attempt (openai
// client, stream:true → anthropic upstream) — the path every zcode openai
// request takes.
func convertedStreamingAttempt(t *testing.T, runtime Runtime, request *http.Request, writer http.ResponseWriter) Attempt {
	t.Helper()
	plan := NewPlan(PlanInput{
		Target:          configdomain.RouteTarget{Provider: "upstream", Model: "model"},
		ProviderConfig:  configdomain.Provider{Provider: "test", AnthropicBaseURL: "https://upstream.test"},
		Provider:        &executorTestProvider{},
		ClientProtocol:  protocol.OpenAI,
		BackendProtocol: protocol.Anthropic,
		ClientPath:      "/v1/chat/completions",
	})
	return NewAttempt(runtime, plan,
		Exchange{Request: request, Writer: writer, Body: []byte(`{"model":"model","stream":true,"messages":[]}`)},
		Scope{}, Policy{})
}

// clientLeavesOnFirstWrite is a response writer whose client connection is
// lost the moment the first bytes are written: it cancels the request
// context (what net/http does when a client disconnects) while Write keeps
// succeeding — the response bufio absorbs the following small write, exactly
// as on a real closed connection.
type clientLeavesOnFirstWrite struct {
	http.ResponseWriter
	cancel context.CancelFunc
	once   sync.Once
}

func (writer *clientLeavesOnFirstWrite) Write(p []byte) (int, error) {
	writer.once.Do(writer.cancel)
	return writer.ResponseWriter.Write(p)
}

// TestExecutorConvertedStreamUpstreamCutIsLogged: the upstream connection
// fails mid-generation while the client is still there. The converter's
// error terminal still reaches the client byte-for-byte, and serve.log names
// the upstream failure — it used to carry nothing per event.
func TestExecutorConvertedStreamUpstreamCutIsLogged(t *testing.T) {
	logs := captureLogs(t)
	request := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", nil)
	writer := httptest.NewRecorder()
	result := (Executor{Client: &sseDoer{body: &cutBody{}}, State: &executorState{}}).Execute(
		convertedStreamingAttempt(t, Runtime{}, request, writer))
	if !result.Committed {
		t.Fatalf("result = %+v", result)
	}
	requireConvertedErrorTerminal(t, writer.Body.String())
	logged := logs.String()
	assertLoggedOnce(t, logged,
		"[proto=openai provider=upstream] converted stream (anthropic→openai) ended with an error terminal — delivered to the client: {\"error\":{")
	if !strings.Contains(logged, `"message":"`+convertedErrorMessage+`"`) {
		t.Fatalf("the evidence excerpt must carry the terminal's message:\n%s", logged)
	}
	if strings.Contains(logs.String(), "client disconnected") {
		t.Fatalf("an upstream failure was attributed to the client:\n%s", logs.String())
	}
}

// TestExecutorConvertedStreamClientCancelIsLoggedAsClientDisconnect: the
// client walks away mid-stream; the canceled request context interrupts the
// parked upstream Read and the converter emits its error terminal for a
// client that is already gone. serve.log must attribute it to the client —
// not to the upstream, and not to nothing.
func TestExecutorConvertedStreamClientCancelIsLoggedAsClientDisconnect(t *testing.T) {
	logs := captureLogs(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", nil).WithContext(ctx)
	writer := &clientLeavesOnFirstWrite{ResponseWriter: httptest.NewRecorder(), cancel: cancel}
	result := (Executor{Client: &parkedAnthropicDoer{}, State: &executorState{}}).Execute(
		convertedStreamingAttempt(t, Runtime{}, request, writer))
	if !result.Committed {
		t.Fatalf("result = %+v", result)
	}
	logged := logs.String()
	assertLoggedOnce(t, logged,
		"[proto=openai provider=upstream] client disconnected mid-stream — converted error terminal not delivered; not an upstream fault")
	if strings.Contains(logged, "ended with an error terminal") {
		t.Fatalf("client disconnect was misattributed as an upstream failure:\n%s", logged)
	}
}

// Same shape on the production copy path: stream_keepalive > 0 selects
// flushCopyHeartbeat, whose reader goroutine is parked in the converted Read
// when the context cancel reaches it.
func TestExecutorConvertedStreamKeepaliveClientCancelIsLoggedAsClientDisconnect(t *testing.T) {
	logs := captureLogs(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", nil).WithContext(ctx)
	writer := &clientLeavesOnFirstWrite{ResponseWriter: httptest.NewRecorder(), cancel: cancel}
	runtime := Runtime{Scheduling: configdomain.Scheduling{StreamKeepalive: "1h"}}
	result := (Executor{Client: &parkedAnthropicDoer{}, State: &executorState{}}).Execute(
		convertedStreamingAttempt(t, runtime, request, writer))
	if !result.Committed {
		t.Fatalf("result = %+v; headers had committed, the stream copy must finish committed", result)
	}
	assertLoggedOnce(t, logs.String(),
		"[proto=openai provider=upstream] client disconnected mid-stream — converted error terminal not delivered; not an upstream fault")
}

// TestExecutorConvertedStreamSchedulingTimeoutIsLogged: the upstream context
// deadline (not the client) ends the converted stream — the error terminal
// is delivered to a client that is still there, and the line says so. The
// only clock here is the scheduling timeout under test.
func TestExecutorConvertedStreamSchedulingTimeoutIsLogged(t *testing.T) {
	logs := captureLogs(t)
	request := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", nil)
	writer := httptest.NewRecorder()
	runtime := Runtime{Scheduling: configdomain.Scheduling{UpstreamTimeout: "100ms"}}
	result := (Executor{Client: &parkedAnthropicDoer{}, State: &executorState{}}).Execute(
		convertedStreamingAttempt(t, runtime, request, writer))
	if !result.Committed {
		t.Fatalf("result = %+v", result)
	}
	if request.Context().Err() != nil {
		t.Fatalf("the client context must still be live: %v", request.Context().Err())
	}
	requireConvertedErrorTerminal(t, writer.Body.String())
	logged := logs.String()
	assertLoggedOnce(t, logged,
		"[proto=openai provider=upstream] scheduling timeout ended the converted stream — error terminal delivered")
	if strings.Contains(logged, "client disconnected") || strings.Contains(logged, "ended with an error terminal") {
		t.Fatalf("a scheduling timeout was misattributed:\n%s", logged)
	}
}

// Control: a converted stream the upstream completes leaves no stream-end
// line at all.
func TestExecutorConvertedStreamCompleteLogsNoStreamEnd(t *testing.T) {
	logs := captureLogs(t)
	request := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", nil)
	complete := anthropicParkedStream +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	writer := httptest.NewRecorder()
	result := (Executor{Client: &sseDoer{body: io.NopCloser(bytes.NewBufferString(complete))}, State: &executorState{}}).Execute(
		convertedStreamingAttempt(t, Runtime{}, request, writer))
	if !result.Committed {
		t.Fatalf("result = %+v", result)
	}
	if client := writer.Body.String(); !strings.Contains(client, `"finish_reason":"stop"`) || !strings.Contains(client, "data: [DONE]") {
		t.Fatalf("the converted stream must complete normally:\n%s", client)
	}
	if logged := logs.String(); strings.Contains(logged, "error terminal") || strings.Contains(logged, "disconnected") || strings.Contains(logged, "scheduling timeout") {
		t.Fatalf("a complete converted stream must not log a stream abort:\n%s", logged)
	}
}
