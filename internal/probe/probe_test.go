package probe

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/provider"
)

type stubImpl struct{}

func (stubImpl) AuthHeaders(*http.Request) error                              { return nil }
func (stubImpl) Refresh() error                                               { return nil }
func (stubImpl) RewriteRequest(u string, b []byte, _ string) (string, []byte) { return u, b }
func (stubImpl) Logout() error                                                { return nil }
func (stubImpl) Usage() error                                                 { return nil }
func (stubImpl) FetchModels() ([]string, error)                               { return nil, nil }
func (stubImpl) Quota() (*provider.QuotaSnapshot, error)                      { return nil, nil }
func (stubImpl) ProbeRequest(modelID string) provider.ProbeRequest {
	return provider.ProbeRequest{Method: http.MethodPost, Path: "/chat/completions"}
}
func (stubImpl) ExtraHeaders(*http.Request, []byte, string)           {}
func (stubImpl) FilterModelIDs(ids []string) (kept, dropped []string) { return ids, nil }

func TestCallableOpenAISuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	ok, status, reason := Callable(context.Background(), srv.Client(),
		configdomain.Provider{OpenAIBaseURL: srv.URL}, stubImpl{}, "m")
	if !ok || status != 200 || reason != "" {
		t.Errorf("Callable = %v %d %q", ok, status, reason)
	}
}

func TestCallableAnthropicBaseUsesMessages(t *testing.T) {
	var gotPath, gotVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotVersion = r.Header.Get("anthropic-version")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	ok, _, _ := Callable(context.Background(), srv.Client(),
		configdomain.Provider{OpenAIBaseURL: "http://unused", AnthropicBaseURL: srv.URL}, stubImpl{}, "m")
	if !ok {
		t.Fatal("Callable failed")
	}
	if gotPath != "/v1/messages" || gotVersion != "2023-06-01" {
		t.Errorf("path=%q version=%q", gotPath, gotVersion)
	}
}

func TestCallableErrorReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_model","message":"no such model. Request id: abc123"}}`))
	}))
	defer srv.Close()
	ok, status, reason := Callable(context.Background(), srv.Client(),
		configdomain.Provider{OpenAIBaseURL: srv.URL}, stubImpl{}, "m")
	if ok || status != 400 {
		t.Fatalf("Callable = %v %d", ok, status)
	}
	if reason != "invalid_model: no such model." {
		t.Errorf("reason = %q", reason)
	}
}

func TestCallableTransportError(t *testing.T) {
	ok, status, reason := Callable(context.Background(), http.DefaultClient,
		configdomain.Provider{OpenAIBaseURL: "http://127.0.0.1:1"}, stubImpl{}, "m")
	if ok || status != 0 || !strings.HasPrefix(reason, "request: ") {
		t.Errorf("Callable = %v %d %q", ok, status, reason)
	}
}

func TestExchangeMeasuresLatency(t *testing.T) {
	// The handler stalls 30ms so a real wall-clock measurement must land
	// strictly above that floor — proving Latency measures the exchange.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(30 * time.Millisecond)
	}))
	defer srv.Close()
	res := Exchange(context.Background(), srv.Client(),
		configdomain.Provider{OpenAIBaseURL: srv.URL}, stubImpl{}, "m")
	if !res.OK || res.Latency < 30*time.Millisecond {
		t.Errorf("Exchange = %+v, want OK with latency >= 30ms handler stall", res)
	}
}

// maxTokensProbeImpl returns an OpenAI chat probe body that carries
// max_tokens, exercising the max_completion_tokens retry path.
type maxTokensProbeImpl struct{ stubImpl }

func (maxTokensProbeImpl) ProbeRequest(modelID string) provider.ProbeRequest {
	body, _ := json.Marshal(map[string]any{
		"model":      modelID,
		"messages":   []map[string]string{{"role": "user", "content": "hi"}},
		"max_tokens": 1,
		"stream":     false,
	})
	return provider.ProbeRequest{Method: http.MethodPost, Path: "/chat/completions", Body: body}
}

func TestCallableRetriesWithMaxCompletionTokens(t *testing.T) {
	var calls int
	var secondBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead."}}`))
			return
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &secondBody)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	ok, status, reason := Callable(context.Background(), srv.Client(),
		configdomain.Provider{OpenAIBaseURL: srv.URL}, maxTokensProbeImpl{}, "gpt-5.5")
	if !ok || status != 200 || reason != "" {
		t.Fatalf("Callable = %v %d %q, want retried success", ok, status, reason)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (initial + retry)", calls)
	}
	if _, legacy := secondBody["max_tokens"]; legacy {
		t.Errorf("retry body still carries max_tokens: %v", secondBody)
	}
	if v, ok := secondBody["max_completion_tokens"]; !ok || v != float64(1) {
		t.Errorf("retry body max_completion_tokens = %v (ok=%v), want 1", v, ok)
	}
}

func TestCallableNoRetryWithoutMaxTokensParam(t *testing.T) {
	// stubImpl's probe body is empty (no max_tokens), so even an upstream
	// max_completion_tokens rejection must NOT trigger a retry - there is
	// nothing to rename (codex's /responses probe is the real-world case).
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Use 'max_completion_tokens' instead."}}`))
	}))
	defer srv.Close()
	ok, status, _ := Callable(context.Background(), srv.Client(),
		configdomain.Provider{OpenAIBaseURL: srv.URL}, stubImpl{}, "m")
	if ok || status != 400 {
		t.Fatalf("Callable = %v %d, want dropped", ok, status)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (no retry without max_tokens in body)", calls)
	}
}

func TestCallableRetryFailureSurfacesRetryReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), "max_completion_tokens") {
			_, _ = w.Write([]byte(`{"error":{"code":"invalid_model","message":"no such model"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"error":{"message":"Use 'max_completion_tokens' instead."}}`))
	}))
	defer srv.Close()
	ok, _, reason := Callable(context.Background(), srv.Client(),
		configdomain.Provider{OpenAIBaseURL: srv.URL}, maxTokensProbeImpl{}, "m")
	if ok {
		t.Fatal("Callable succeeded, want dropped after failed retry")
	}
	if reason != "invalid_model: no such model" {
		t.Errorf("reason = %q, want the retry response's reason", reason)
	}
}

// Cross-leg fallback: OpenCode Go-style gateways enforce PER-MODEL protocols
// (a chat-only model hard-rejects /v1/messages with 400 ModelProtocolUnsupported).
// Callable must retry the remaining configured legs and report the model
// callable on the leg that answers; any OTHER failure keeps the single-probe
// semantics (auth errors never trigger fallback).
func TestCallableProtocolLegFallback_AnthropicPrimaryToChat(t *testing.T) {
	var order []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		order = append(order, r.URL.Path)
		switch r.URL.Path {
		case "/v1/messages":
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"ModelProtocolUnsupported","message":"Model does not support this protocol."}}`))
		case "/chat/completions":
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected probe path %q", r.URL.Path)
		}
	}))
	defer srv.Close()
	ok, status, reason := Callable(context.Background(), srv.Client(),
		configdomain.Provider{OpenAIBaseURL: srv.URL, AnthropicBaseURL: srv.URL}, stubImpl{}, "glm-5.3-flash")
	if !ok || status != 200 || reason != "" {
		t.Fatalf("Callable = %v %d %q, want chat-leg success", ok, status, reason)
	}
	if len(order) != 2 || order[0] != "/v1/messages" || order[1] != "/chat/completions" {
		t.Errorf("probe order = %v, want anthropic primary then chat fallback", order)
	}
}

// Fallback walks to the responses leg when chat also draws the rejection
// (grok-style responses-only model).
func TestCallableProtocolLegFallback_ToResponses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/responses" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"ModelProtocolUnsupported","message":"Model does not support this protocol."}}`))
	}))
	defer srv.Close()
	ok, status, reason := Callable(context.Background(), srv.Client(),
		configdomain.Provider{OpenAIBaseURL: srv.URL, AnthropicBaseURL: srv.URL}, stubImpl{}, "grok-4.6")
	if !ok || status != 200 || reason != "" {
		t.Fatalf("Callable = %v %d %q, want responses-leg success", ok, status, reason)
	}
}

// All legs reject the model's protocol: report the failure (last leg's
// outcome) after exhausting the fallbacks — never a false positive.
func TestCallableProtocolLegFallback_AllUnsupported(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"type":"ModelProtocolUnsupported","message":"Model does not support this protocol."}}`))
	}))
	defer srv.Close()
	ok, status, reason := Callable(context.Background(), srv.Client(),
		configdomain.Provider{OpenAIBaseURL: srv.URL, AnthropicBaseURL: srv.URL}, stubImpl{}, "m")
	if ok || status != 400 {
		t.Fatalf("Callable = %v %d, want dropped after exhausting legs", ok, status)
	}
	if calls != 3 {
		t.Errorf("calls = %d, want 3 (anthropic + chat + responses)", calls)
	}
	if !strings.Contains(reason, "ModelProtocolUnsupported") {
		t.Errorf("reason = %q, want the protocol rejection surfaced", reason)
	}
}

// Auth failures (and any non-protocol failure) must NOT trigger the fallback:
// a 401 on the primary leg is reported as-is, single probe.
func TestCallableNoFallbackOnAuthError(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"type":"ModelProtocolUnsupported","message":"would be dangerous to fall back"}}`))
	}))
	defer srv.Close()
	ok, status, _ := Callable(context.Background(), srv.Client(),
		configdomain.Provider{OpenAIBaseURL: srv.URL, AnthropicBaseURL: srv.URL}, stubImpl{}, "m")
	if ok || status != 401 {
		t.Fatalf("Callable = %v %d, want the primary leg's 401", ok, status)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (non-2xx non-protocol-rejection must not fall back)", calls)
	}
}

// OpenAI-base primary (no anthropic base): the impl's chat probe is primary
// and the responses leg is the fallback (mirrors the anthropic-primary
// branch for openai-only gateways).
func TestCallableProtocolLegFallback_OpenAIPrimaryToResponses(t *testing.T) {
	var order []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		order = append(order, r.URL.Path)
		if r.URL.Path == "/responses" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"ModelProtocolUnsupported","message":"Model does not support this protocol."}}`))
	}))
	defer srv.Close()
	ok, status, _ := Callable(context.Background(), srv.Client(),
		configdomain.Provider{OpenAIBaseURL: srv.URL}, stubImpl{}, "m")
	if !ok || status != 200 {
		t.Fatalf("Callable = %v %d, want responses fallback success", ok, status)
	}
	if len(order) != 2 || order[0] != "/chat/completions" || order[1] != "/responses" {
		t.Errorf("probe order = %v, want chat primary then responses fallback", order)
	}
}

func TestStripRequestID(t *testing.T) {
	if got := StripRequestID("boom. Request id: deadbeef"); got != "boom." {
		t.Errorf("got %q", got)
	}
	if got := StripRequestID("plain"); got != "plain" {
		t.Errorf("got %q", got)
	}
}

func TestReasonForBodyFallback(t *testing.T) {
	long := strings.Repeat("x", 200)
	if got := reasonForBody([]byte(long)); len([]rune(got)) != 161 {
		t.Errorf("truncated len = %d", len([]rune(got)))
	}
}
