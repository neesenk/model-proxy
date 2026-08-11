package probe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
func (stubImpl) ExtraHeaders(*http.Request, string)                   {}
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	res := Exchange(context.Background(), srv.Client(),
		configdomain.Provider{OpenAIBaseURL: srv.URL}, stubImpl{}, "m")
	if !res.OK || res.Latency < 0 {
		t.Errorf("Exchange = %+v", res)
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
