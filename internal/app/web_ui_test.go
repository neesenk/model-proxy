package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebServesUI(t *testing.T) {
	w, _ := newTestWeb(t)
	mux := http.NewServeMux()
	w.Register(mux)

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

	// app.js is an ES module importing './pure.js' — that target must be
	// served too, with the JS content type, or the SPA fails to load.
	rec3 := httptest.NewRecorder()
	mux.ServeHTTP(rec3, httptest.NewRequest("GET", "/ui/pure.js", nil))
	if rec3.Code != 200 || !strings.Contains(rec3.Header().Get("content-type"), "javascript") {
		t.Fatalf("GET /ui/pure.js status=%d content-type=%q — the app.js import target must be served", rec3.Code, rec3.Header().Get("content-type"))
	}
}
