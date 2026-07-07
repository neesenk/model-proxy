package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"model-proxy/provider"
)

// TestForward_ProviderRouting_SplitsByModel verifies that the proxy routes
// requests to different providers based on the route's model→provider/model map.
func TestForward_ProviderRouting_SplitsByModel(t *testing.T) {
	var codexHit, gwHit requestHit
	codexUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		codexHit = captureHit(r)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer codexUp.Close()
	gwUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gwHit = captureHit(r)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer gwUp.Close()

	cfg := &Config{

		Providers: map[string]Provider{
			"codex":   {OpenAIBaseURL: codexUp.URL, Provider: "static"},
			"compass": {OpenAIBaseURL: gwUp.URL, Provider: "static"},
		},
		Routes: map[string][]RouteTarget{
			"gpt-5.5": {{Provider: "codex", Model: "gpt-5.5"}},
			"glm-5.2": {{Provider: "compass", Model: "glm-5.2"}},
		},
	}
	p := NewProxy(cfg)
	// Override both providers' auth with known tokens for deterministic test.
	p.providers["codex"] = &testProv{key: "codex-token"}
	p.providers["compass"] = &testProv{key: "gw-key"}

	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	// 1) gpt-5.5 → codex provider
	codexHit, gwHit = requestHit{}, requestHit{}
	post(t, px.URL+"/v1/responses", `{"model":"gpt-5.5","input":[]}`)
	if codexHit.path == "" {
		t.Error("gpt-5.5: expected to hit codex backend")
	}
	if gwHit.path != "" {
		t.Error("gpt-5.5: should not hit compass backend")
	}
	if codexHit.auth != "Bearer codex-token" {
		t.Errorf("gpt-5.5 auth=%q want Bearer codex-token", codexHit.auth)
	}
	// P2-1: assert the model field was rewritten to the upstream model name
	if codexHit.model != "gpt-5.5" {
		t.Errorf("gpt-5.5 model rewrite: upstream model=%q want gpt-5.5", codexHit.model)
	}

	// 2) glm-5.2 → compass provider
	codexHit, gwHit = requestHit{}, requestHit{}
	post(t, px.URL+"/v1/responses", `{"model":"glm-5.2","input":[]}`)
	if gwHit.path == "" {
		t.Error("glm-5.2: expected to hit compass backend")
	}
	if codexHit.path != "" {
		t.Error("glm-5.2: should not hit codex backend")
	}
	if gwHit.auth != "Bearer gw-key" {
		t.Errorf("glm-5.2 auth=%q want Bearer gw-key", gwHit.auth)
	}
	// P2-1: assert model rewrite
	if gwHit.model != "glm-5.2" {
		t.Errorf("glm-5.2 model rewrite: upstream model=%q want glm-5.2", gwHit.model)
	}
}

// TestForward_UnknownModel errors when the model isn't in the route.
func TestForward_UnknownModel(t *testing.T) {
	gwUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer gwUp.Close()
	cfg := &Config{

		Providers: map[string]Provider{
			"compass": {OpenAIBaseURL: gwUp.URL, Provider: "static"},
		},
		Routes: map[string][]RouteTarget{
			"gpt-5.5": {{Provider: "compass", Model: "gpt-5.5"}},
		},
	}
	p := NewProxy(cfg)
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()
	resp, err := http.Post(px.URL+"/v1/responses", "application/json", stringReader(`{"model":"unknown"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 502 {
		t.Errorf("expected 502 for unknown model, got %d", resp.StatusCode)
	}
}

type requestHit struct {
	path  string
	auth  string
	model string
}

func captureHit(r *http.Request) requestHit {
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	var v struct {
		Model string `json:"model"`
	}
	json.Unmarshal(body, &v)
	return requestHit{path: r.URL.Path, auth: r.Header.Get("Authorization"), model: v.Model}
}

func post(t *testing.T, url, body string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", stringReader(body))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func stringReader(s string) io.Reader { return &stringReaderImpl{s: s} }

type stringReaderImpl struct {
	s   string
	pos int
}

func (r *stringReaderImpl) Read(p []byte) (int, error) {
	if r.pos >= len(r.s) {
		return 0, io.EOF
	}
	n := copy(p, r.s[r.pos:])
	r.pos += n
	return n, nil
}

// testProv implements provider.Provider for deterministic tests.
type testProv struct {
	key string
}

func (t *testProv) AuthHeaders(req *http.Request) error {
	req.Header.Set("Authorization", "Bearer "+t.key)
	req.Header.Del("x-api-key")
	return nil
}
func (t *testProv) Refresh() error { return nil }
func (t *testProv) RewriteRequest(url string, body []byte, path string) (string, []byte) {
	return url, body
}
func (t *testProv) Login() error                            { return nil }
func (t *testProv) Logout() error                           { return nil }
func (t *testProv) Usage() (any, error)                     { return nil, nil }
func (t *testProv) FetchModels() ([]string, error)          { return nil, nil }
func (t *testProv) Quota() (*provider.QuotaSnapshot, error) { return nil, nil }
func (t *testProv) Surplus(snap *provider.QuotaSnapshot, now time.Time, peakMult float64) float64 {
	return snap.Surplus(now, peakMult)
}

var _ provider.Provider = (*testProv)(nil)
