package targetexec

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/protocol"
)

// infiniteZeroReader is an endless source of 'a' bytes (cheaper than a 64MiB slice).
type infiniteZeroReader struct{}

func (infiniteZeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'a'
	}
	return len(p), nil
}

// TestExecutorOversizedConvertResponseFailsClosed: an upstream 2xx body larger
// than the conversion buffer must FAIL (502), not be silently truncated — a
// truncated JSON converts into a corrupt "successful" response that is also
// cached and replayed.
func TestExecutorOversizedConvertResponseFailsClosed(t *testing.T) {
	provider := &executorTestProvider{}
	writer := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "http://proxy.test/v1/responses", nil)
	plan := NewPlan(PlanInput{
		Target: configdomain.RouteTarget{Provider: "upstream", Model: "chat-model"},
		ProviderConfig: configdomain.Provider{
			Provider: "test", OpenAIBaseURL: "https://upstream.test",
		},
		Provider:        provider,
		ClientProtocol:  protocol.Responses,
		BackendProtocol: protocol.OpenAI,
		ClientPath:      "/v1/responses",
	})
	attempt := NewAttempt(
		Runtime{},
		plan,
		Exchange{
			Request: request,
			Writer:  writer,
			Body:    []byte(`{"model":"chat-model","input":[]}`),
		},
		Scope{},
		Policy{LastTarget: true},
	)
	// Non-streaming upstream + converting route → the buffered read path.
	response := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(io.LimitReader(infiniteZeroReader{}, maxConvertBufferBytes+1)),
	}
	result := (Executor{
		Client: &sequenceDoer{responses: []*http.Response{response}},
		State:  &executorState{},
	}).Execute(attempt)
	if !result.Committed {
		t.Fatalf("result = %+v; the fail-closed 502 must still be a terminal commit", result)
	}
	if writer.Code != http.StatusBadGateway {
		t.Fatalf("oversized converting response status = %d, want 502 (fail-closed, never a truncated 200)", writer.Code)
	}
	if strings.Contains(writer.Body.String(), `"choices"`) {
		t.Fatalf("truncated body was converted and delivered: %.80q", writer.Body.String())
	}
}
