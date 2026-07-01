package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestForward_ModelRouting_SplitsByModel verifies that a route with model_routing
// sends matching models to the entry's upstream+auth and others to the route's
// default upstream+auth.
func TestForward_ModelRouting_SplitsByModel(t *testing.T) {
	var codexHit, gwHit requestHit
	// codex native backend
	codexUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		codexHit = captureHit(r)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer codexUp.Close()
	// gateway backend
	gwUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gwHit = captureHit(r)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	}))
	defer gwUp.Close()

	// CQP provider with a static key (skip real minting).
	cfg := &Config{
		Auth: AuthCfg{StaticKey: "cqp-key"},
		Routes: []Route{{
			Name:         "codex",
			PathPrefixes: []string{"/v1/responses"},
			Upstream:     gwUp.URL, // default = gateway
			Auth:         "static",
			ModelRouting: []ModelRoute{{
				Models:   []string{"gpt-5.5"},
				Upstream: codexUp.URL,
				Auth:     "static",
			}},
		}},
	}
	// Make the codex_oauth entry use a static key too (override after build):
	p := NewProxy(cfg)
	// Replace the model_route auth with a static-key provider for deterministic test.
	for i := range p.routes {
		if p.routes[i].route.Name == "codex" {
			for j := range p.routes[i].modelRoutes {
				p.routes[i].modelRoutes[j].auth = &StaticProvider{key: "codex-oauth-token"}
			}
		}
	}
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()

	// 1) gpt-5.5 → codex backend, codex OAuth token.
	codexHit, gwHit = requestHit{}, requestHit{}
	post(t, px.URL+"/v1/responses", `{"model":"gpt-5.5","input":[]}`)
	if codexHit.path == "" {
		t.Error("gpt-5.5: expected to hit codex backend")
	}
	if gwHit.path != "" {
		t.Error("gpt-5.5: should not hit gateway backend")
	}
	if codexHit.auth != "Bearer codex-oauth-token" {
		t.Errorf("gpt-5.5 auth=%q want Bearer codex-oauth-token", codexHit.auth)
	}

	// 2) glm-5.2 → gateway backend, CQP key.
	codexHit, gwHit = requestHit{}, requestHit{}
	post(t, px.URL+"/v1/responses", `{"model":"glm-5.2","input":[]}`)
	if gwHit.path == "" {
		t.Error("glm-5.2: expected to hit gateway backend")
	}
	if codexHit.path != "" {
		t.Error("glm-5.2: should not hit codex backend")
	}
	if gwHit.auth != "Bearer cqp-key" {
		t.Errorf("glm-5.2 auth=%q want Bearer cqp-key", gwHit.auth)
	}
}

type requestHit struct {
	path string
	auth string
}

func captureHit(r *http.Request) requestHit {
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	_ = body
	return requestHit{path: r.URL.Path, auth: r.Header.Get("Authorization")}
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

// Ensure the static provider's key is used end-to-end (no time dependency).
func TestForward_ModelRouting_NoMatchFallsBack(t *testing.T) {
	gwUp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{}`))
	}))
	defer gwUp.Close()
	cfg := &Config{
		Auth: AuthCfg{StaticKey: "k"},
		Routes: []Route{{
			Name: "codex", PathPrefixes: []string{"/v1/responses"},
			Upstream: gwUp.URL, Auth: "static",
			ModelRouting: []ModelRoute{{Models: []string{"only-this"}, Upstream: "http://nope", Auth: "static"}},
		}},
	}
	p := NewProxy(cfg)
	px := httptest.NewServer(http.HandlerFunc(p.handler))
	defer px.Close()
	// model not in any routing entry → falls back to route default (gwUp).
	resp, err := http.Post(px.URL+"/v1/responses", "application/json", stringReader(`{"model":"other"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("expected 200 from fallback upstream, got %d", resp.StatusCode)
	}
}

// avoid unused import warnings if time isn't used elsewhere here
var _ = time.Now
