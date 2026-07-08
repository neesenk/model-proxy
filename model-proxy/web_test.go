package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestWeb(t *testing.T) (*webServer, *Proxy) {
	t.Helper()
	cfg, _ := LoadConfigFromBytes("test", []byte(`listen: 127.0.0.1:0
providers:
  zhipu: {provider_id: zhipu, openai_base_url: https://x}
`))
	p := NewProxy(cfg)
	return newWebServer(p, "test-config.yaml"), p
}

func TestWebServesUI(t *testing.T) {
	w, _ := newTestWeb(t)
	mux := http.NewServeMux()
	w.register(mux)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/ui/", nil))
	if rec.Code != 200 {
		t.Fatalf("GET /ui/ status=%d want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "<!doctype html>") {
		t.Errorf("/ui/ body missing doctype: %q", rec.Body.String())
	}

	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, httptest.NewRequest("GET", "/ui/app.js", nil))
	if rec2.Code != 200 {
		t.Fatalf("GET /ui/app.js status=%d want 200", rec2.Code)
	}
}
