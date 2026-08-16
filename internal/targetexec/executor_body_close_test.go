package targetexec

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/protocol"
)

// closeCountingBody wraps an SSE upstream body and counts Close calls.
type closeCountingBody struct {
	reader io.Reader
	closes atomic.Int32
}

func (b *closeCountingBody) Read(p []byte) (int, error) { return b.reader.Read(p) }
func (b *closeCountingBody) Close() error {
	b.closes.Add(1)
	return nil
}

// TestExecutorClosesUpstreamBodyAfterStreamingConversion: the cross-protocol
// STREAMING path pipes response.Body through the SSE converter into the
// client pipeline; io.NopCloser on that leg drops the Close chain, so the
// upstream body is never closed (connection reuse lost; correctness left to
// implicit EOF/ctx cleanup). Close must reach the upstream body exactly once.
func TestExecutorClosesUpstreamBodyAfterStreamingConversion(t *testing.T) {
	provider := &executorTestProvider{}
	writer := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/chat/completions", nil)
	plan := NewPlan(PlanInput{
		Target: configdomain.RouteTarget{Provider: "upstream", Model: "chat-model"},
		ProviderConfig: configdomain.Provider{
			Provider: "test", AnthropicBaseURL: "https://upstream.test",
		},
		Provider:        provider,
		ClientProtocol:  protocol.OpenAI,
		BackendProtocol: protocol.Anthropic,
		ClientPath:      "/v1/chat/completions",
	})
	attempt := NewAttempt(
		Runtime{},
		plan,
		Exchange{
			Request: request,
			Writer:  writer,
			Body:    []byte(`{"model":"chat-model","stream":true,"messages":[]}`),
		},
		Scope{},
		Policy{LastTarget: true},
	)

	upstream := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","model":"chat-model","role":"assistant"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	body := &closeCountingBody{reader: bytes.NewBufferString(upstream)}
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       body,
	}
	result := (Executor{
		Client: &sequenceDoer{responses: []*http.Response{response}},
		State:  &executorState{},
	}).Execute(attempt)
	if !result.Committed {
		t.Fatalf("result = %+v; client body=%q", result, writer.Body.String())
	}
	if got := body.closes.Load(); got != 1 {
		t.Fatalf("upstream body Close calls = %d, want exactly 1 (streaming conversion must propagate Close)", got)
	}
	if !bytes.Contains(writer.Body.Bytes(), []byte(`"hi"`)) {
		t.Fatalf("client body missing converted delta: %q", writer.Body.String())
	}
}
