package targetexec

import (
	"io"
	"net/http"
	"testing"

	"model-proxy/internal/observe/counters"
)

// usageEffects wires the production usage scanner (internal/observe/counters)
// into the executor's CaptureUsage hook so tests assert the real accounting.
type usageEffects struct {
	executorEffects
	tc  *counters.TokenCounter
	key counters.TokenKey
}

func (effects *usageEffects) CaptureUsage(
	body io.ReadCloser,
	_ AttemptDTO,
	observe func(Usage),
) io.ReadCloser {
	return counters.NewUsageScanner(body, effects.key, effects.tc, func(usage counters.TokenUsage) {
		observe(Usage{Input: usage.Input, Output: usage.Output, CacheRead: usage.CacheRead, CacheCreation: usage.CacheCreation})
	})
}

// A non-streaming upstream body must be token-counted exactly like a streamed
// one: the executor installs CaptureUsage unconditionally (the scanner's SSE
// path idles and its whole-body JSON fallback counts the usage object). This
// is the decisions-protocol case — 100% non-streaming traffic.
func TestExecutorCountsUsageForNonStreamJSON(t *testing.T) {
	provider := &executorTestProvider{}
	attempt, writer := testAttempt(provider, `{"model":"model"}`, Policy{LastTarget: true})
	body := `{"id":"chatcmpl-1","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":40}}}`
	doer := &sequenceDoer{responses: []*http.Response{testResponse(200, body)}}
	effects := &usageEffects{
		tc:  counters.NewTokenCounter(),
		key: counters.TokenKey{Provider: "upstream", Model: "model"},
	}
	result := (Executor{Client: doer, State: &executorState{}, Effects: effects}).Execute(attempt)
	if !result.Committed || writer.Code != 200 {
		t.Fatalf("result=%+v status=%d", result, writer.Code)
	}
	usage := effects.tc.Snapshot()[effects.key]
	if usage.Input != 100 || usage.Output != 20 || usage.CacheRead != 40 || usage.Requests != 1 {
		t.Errorf("token counter = %+v, want the non-stream usage counted", usage)
	}
	commit := effects.lastCommit
	if commit.Usage.Input != 100 || commit.Usage.Output != 20 {
		t.Errorf("attempt usage = %+v, want the observe callback fed", commit.Usage)
	}
}

// A streamed upstream must keep its SSE accounting (no double count from the
// non-stream fallback).
func TestExecutorKeepsSSEUsageAccounting(t *testing.T) {
	provider := &executorTestProvider{}
	attempt, writer := testAttempt(provider, `{"model":"model","stream":true}`, Policy{LastTarget: true})
	sseBody := "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n" +
		"data: {\"id\":\"chatcmpl-1\",\"choices\":[],\"usage\":{\"prompt_tokens\":50,\"completion_tokens\":9}}\n\n" +
		"data: [DONE]\n\n"
	resp := testResponse(200, sseBody)
	resp.Header.Set("content-type", "text/event-stream")
	doer := &sequenceDoer{responses: []*http.Response{resp}}
	effects := &usageEffects{
		tc:  counters.NewTokenCounter(),
		key: counters.TokenKey{Provider: "upstream", Model: "model"},
	}
	result := (Executor{Client: doer, State: &executorState{}, Effects: effects}).Execute(attempt)
	if !result.Committed || writer.Code != 200 {
		t.Fatalf("result=%+v status=%d", result, writer.Code)
	}
	usage := effects.tc.Snapshot()[effects.key]
	if usage.Input != 50 || usage.Output != 9 || usage.Requests != 1 {
		t.Errorf("token counter = %+v, want exactly the SSE usage (no double count)", usage)
	}
}
