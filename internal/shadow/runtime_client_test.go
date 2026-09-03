package shadow

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/protocol"
	"model-proxy/internal/targetexec"
)

// Job.Client must take precedence over the runtime default client so the
// shadow request follows the shadow provider's proxy chain (app injects the
// per-provider client).
func TestExecuteUsesJobClientOverride(t *testing.T) {
	var defaultCalls, overrideCalls atomic.Int64
	shadowRuntime := NewRuntime(Options{Timeout: time.Second})
	shadowRuntime.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		defaultCalls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("default"))}, nil
	})
	override := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		overrideCalls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("override"))}, nil
	})}

	plan := targetexec.NewPlan(targetexec.PlanInput{
		Target:          configdomain.RouteTarget{Provider: "candidate", Model: "m"},
		ProviderConfig:  configdomain.Provider{Provider: "test", OpenAIBaseURL: "http://127.0.0.1:1/base"},
		Provider:        &testProvider{},
		ClientProtocol:  protocol.OpenAI,
		BackendProtocol: protocol.OpenAI,
		ClientPath:      "/v1/chat/completions",
	})
	result := shadowRuntime.Execute(t.Context(), Job{
		Plan: plan, Body: []byte(`{"model":"m","messages":[]}`), CalledModel: "m",
		Client: override, MaxBodyBytes: 64,
	})
	if result.Err != nil {
		t.Fatalf("Execute: %v", result.Err)
	}
	if overrideCalls.Load() != 1 || defaultCalls.Load() != 0 {
		t.Fatalf("override/default client calls = %d/%d, want 1/0", overrideCalls.Load(), defaultCalls.Load())
	}
	if string(result.Capture.Body) != "override" {
		t.Fatalf("capture = %q, want the override client's response", result.Capture.Body)
	}
}
