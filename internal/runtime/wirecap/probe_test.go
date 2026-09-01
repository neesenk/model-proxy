package wirecap

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	configdomain "model-proxy/internal/config"
	"model-proxy/internal/provider"
)

type stubImpl struct{ seenPath string }

func (s *stubImpl) AuthHeaders(req *http.Request) error {
	req.Header.Set("authorization", "Bearer k")
	return nil
}
func (s *stubImpl) Refresh() error { return nil }
func (s *stubImpl) RewriteRequest(url string, body []byte, path string) (string, []byte) {
	return url, body
}
func (s *stubImpl) Logout() error                           { return nil }
func (s *stubImpl) Usage() error                            { return nil }
func (s *stubImpl) FetchModels() ([]string, error)          { return nil, nil }
func (s *stubImpl) Quota() (*provider.QuotaSnapshot, error) { return nil, nil }
func (s *stubImpl) ProbeRequest(modelID string) provider.ProbeRequest {
	return provider.ProbeRequest{Method: http.MethodPost, Path: "/chat/completions"}
}
func (s *stubImpl) ExtraHeaders(req *http.Request, path string) {
	req.Header.Set("x-extra", "1")
}
func (s *stubImpl) FilterModelIDs(ids []string) (kept, dropped []string) { return ids, nil }

func TestProbeBodies(t *testing.T) {
	responses, anthropic := ProbeBodies("glm")
	if !strings.Contains(string(responses), `"model":"glm"`) || !strings.Contains(string(responses), `"input"`) {
		t.Errorf("responses body = %s", responses)
	}
	if !strings.Contains(string(anthropic), `"messages"`) || !strings.Contains(string(anthropic), `"max_tokens":1`) {
		t.Errorf("anthropic body = %s", anthropic)
	}
}

func TestProbeBuildsRequest(t *testing.T) {
	var got struct {
		path, auth, extra, anthropicVersion string
		body                                []byte
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path = r.URL.Path
		got.auth = r.Header.Get("authorization")
		got.extra = r.Header.Get("x-extra")
		got.anthropicVersion = r.Header.Get("anthropic-version")
		got.body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	prov := configdomain.Provider{OpenAIBaseURL: srv.URL + "/", Headers: map[string]string{"x-cfg": "h"}}
	status, err := Probe(srv.Client(), prov, &stubImpl{}, "/v1/messages", []byte(`{"m":1}`))
	if err != nil || status != http.StatusOK {
		t.Fatalf("Probe = %d, %v", status, err)
	}
	if got.path != "/v1/messages" {
		t.Errorf("path = %q", got.path)
	}
	if got.auth != "Bearer k" || got.extra != "1" {
		t.Errorf("headers auth=%q extra=%q", got.auth, got.extra)
	}
	if got.anthropicVersion != "2023-06-01" {
		t.Errorf("anthropic-version = %q", got.anthropicVersion)
	}
	if string(got.body) != `{"m":1}` {
		t.Errorf("body = %s", got.body)
	}
}

func TestProbeNonAnthropicPathOmitsVersion(t *testing.T) {
	var versionSeen bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		versionSeen = r.Header.Get("anthropic-version") != ""
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	prov := configdomain.Provider{OpenAIBaseURL: srv.URL}
	status, err := Probe(srv.Client(), prov, &stubImpl{}, "/responses", []byte(`{}`))
	if err != nil || status != http.StatusNotFound {
		t.Fatalf("Probe = %d, %v", status, err)
	}
	if versionSeen {
		t.Error("anthropic-version must only be set on /v1/messages")
	}
}

func TestProbeModel(t *testing.T) {
	cfg := &configdomain.Config{
		Providers: map[string]configdomain.Provider{"p": {}},
		Routes:    map[string][]configdomain.RouteTarget{"m1": {{Provider: "p", Model: "m1"}}},
	}
	if got := ProbeModel(cfg, nil, "p"); got != "m1" {
		t.Errorf("route fallback = %q, want m1", got)
	}
	cfg.Providers["p"] = configdomain.Provider{Models: []string{"m0"}}
	if got := ProbeModel(cfg, nil, "p"); got != "m0" {
		t.Errorf("provider.Models wins = %q, want m0", got)
	}
	// Derived route fallback when neither provider models nor routes name it.
	bare := &configdomain.Config{Providers: map[string]configdomain.Provider{"p": {}}}
	derived := map[string][]configdomain.RouteTarget{"mi": {{Provider: "p", Model: "mi"}}}
	if got := ProbeModel(bare, derived, "p"); got != "mi" {
		t.Errorf("derived fallback = %q, want mi", got)
	}
	if got := ProbeModel(bare, nil, "p"); got != "" {
		t.Errorf("empty fallback = %q, want \"\"", got)
	}
}
